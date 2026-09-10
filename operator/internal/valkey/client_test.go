package valkey_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	operatorvalkey "github.com/RostislavDugin/managed-valkey/operator/internal/valkey"
)

func Test_SetAppUser_WhenContextIsCanceled_ReturnsContextCancellation(t *testing.T) {
	server := newRESPServer(t, func(command []string) (string, bool) {
		if command[0] == "ACL" {
			return "", false
		}

		return "+OK\r\n", false
	})
	client, session := dialSession(t, server.address())
	defer client.Close()
	defer session.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- session.SetAppUser(ctx, false, strings.Repeat("ab", 32))
	}()

	server.waitCommands(t, "ACL", 1)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ожидалась отмена контекста, получено %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("команда не завершилась после отмены контекста")
	}
}

func Test_SetAppUser_WhenResponseIsLost_SendsMutationOnce(t *testing.T) {
	server := newRESPServer(t, func(command []string) (string, bool) {
		if command[0] == "ACL" {
			return "", true
		}

		return "+OK\r\n", false
	})
	client, session := dialSession(t, server.address())
	defer client.Close()
	defer session.Close()

	err := session.SetAppUser(context.Background(), false, strings.Repeat("ab", 32))
	if err == nil {
		t.Fatal("ожидалась ошибка потерянного ответа")
	}

	time.Sleep(100 * time.Millisecond)
	if got := server.commandCount("ACL"); got != 1 {
		t.Fatalf("изменяющая команда отправлена %d раз", got)
	}
}

func Test_SetAppUser_WhenServerRejectsSensitiveCommand_RedactsArgumentsFromError(t *testing.T) {
	const secret = "operator-secret"
	hash := strings.Repeat("ab", 32)
	server := newRESPServer(t, func(command []string) (string, bool) {
		if command[0] == "HELLO" {
			return "+OK\r\n", false
		}

		return "-ERR rejected " + secret + " " + hash + "\r\n", false
	})
	client, session := dialSession(t, server.address())
	defer client.Close()
	defer session.Close()

	err := session.SetAppUser(context.Background(), false, hash)
	if err == nil {
		t.Fatal("ожидалась ошибка ACL SETUSER")
	}
	for _, value := range []string{secret, hash} {
		if strings.Contains(err.Error(), value) {
			t.Fatalf("ошибка содержит секретное значение %q: %v", value, err)
		}
	}
}

func Test_TakeControl_AfterAuthentication_ReadsStateAndKillsPreviousOperatorConnections(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	server := newRESPServer(t, func(command []string) (string, bool) {
		switch command[0] {
		case "HELLO":
			return "+OK\r\n", false
		case "AUTH":
			return "+OK\r\n", false
		case "PING":
			return "+PONG\r\n", false
		case "CLIENT":
			return ":1\r\n", false
		case "ROLE":
			return "*3\r\n$6\r\nmaster\r\n:0\r\n*0\r\n", false
		case "ACL":
			return "*4\r\n$5\r\nflags\r\n*1\r\n$3\r\noff\r\n$9\r\npasswords\r\n*1\r\n$64\r\n" + hash + "\r\n", false
		case "INFO":
			body := "# Server\r\nrun_id:process-1\r\n"
			if len(command) > 1 && command[1] == "replication" {
				body = "# Replication\r\nrole:master\r\nmaster_replid:history-1\r\n" +
					"master_replid2:history-0\r\nmaster_repl_offset:42\r\nsecond_repl_offset:12\r\n"
			}
			return fmt.Sprintf("$%d\r\n%s\r\n", len(body), body), false
		default:
			return "-ERR unexpected command\r\n", false
		}
	})
	server.inspectAfter("AUTH")
	client, session := dialSession(t, server.address())
	defer client.Close()
	defer session.Close()

	state, err := session.TakeControl(context.Background(), "operator-secret")
	if err != nil {
		t.Fatalf("принять управление: %v", err)
	}
	if state.Role != "primary" || state.RunID != "process-1" || state.AppEnabled {
		t.Fatalf("неверное состояние процесса: %#v", state)
	}
	if len(state.AppPasswordHashes) != 1 || state.AppPasswordHashes[0] != hash {
		t.Fatalf("неверные хеши app: %v", state.AppPasswordHashes)
	}
	if state.Replication == nil || state.Replication.ReplicationID != "history-1" ||
		state.Replication.SecondaryReplicationID != "history-0" ||
		state.Replication.Offset != 42 || state.Replication.SecondaryOffset != 12 {
		t.Fatalf("неверное состояние репликации: %+v", state.Replication)
	}
	if server.pipelinedAfter("AUTH") {
		t.Fatal("следующая команда отправлена до ответа AUTH")
	}

	want := []string{"AUTH", "PING", "CLIENT KILL", "ROLE", "ACL GETUSER", "INFO", "INFO"}
	if got := server.applicationCommands(); !slices.Equal(got, want) {
		t.Fatalf("порядок команд %v, ожидался %v", got, want)
	}
}

func Test_Observe_AfterAuthentication_ReadsStateWithoutKillingOperatorConnections(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	server := newRESPServer(t, func(command []string) (string, bool) {
		switch command[0] {
		case "HELLO", "AUTH":
			return "+OK\r\n", false
		case "PING":
			return "+PONG\r\n", false
		case "ROLE":
			return "*3\r\n$6\r\nmaster\r\n:0\r\n*0\r\n", false
		case "ACL":
			return "*4\r\n$5\r\nflags\r\n*1\r\n$3\r\noff\r\n$9\r\npasswords\r\n*1\r\n$64\r\n" + hash + "\r\n", false
		case "INFO":
			body := "# Server\r\nrun_id:process-1\r\n"
			if len(command) > 1 && command[1] == "replication" {
				body = "# Replication\r\nrole:master\r\nmaster_replid:history-1\r\nmaster_repl_offset:42\r\n"
			}
			return fmt.Sprintf("$%d\r\n%s\r\n", len(body), body), false
		default:
			return "-ERR unexpected command\r\n", false
		}
	})
	client, session := dialSession(t, server.address())
	defer client.Close()
	defer session.Close()

	state, err := session.Observe(context.Background(), "operator-secret")
	if err != nil || state.RunID != "process-1" {
		t.Fatalf("прочитать состояние без приёма управления: state=%+v error=%v", state, err)
	}
	want := []string{"AUTH", "PING", "ROLE", "ACL GETUSER", "INFO", "INFO"}
	if got := server.applicationCommands(); !slices.Equal(got, want) {
		t.Fatalf("наблюдение выполнило лишние команды %v, ожидались %v", got, want)
	}
}

func Test_TakeControl_WhenAuthenticationFails_RedactsPasswordAndClassifiesError(t *testing.T) {
	const password = "operator-secret"
	server := newRESPServer(t, func(command []string) (string, bool) {
		if command[0] == "HELLO" {
			return "+OK\r\n", false
		}

		return "-WRONGPASS rejected " + password + "\r\n", false
	})
	client, session := dialSession(t, server.address())
	defer client.Close()
	defer session.Close()

	_, err := session.TakeControl(context.Background(), password)
	if err == nil {
		t.Fatal("ожидалась ошибка AUTH")
	}
	if strings.Contains(err.Error(), password) {
		t.Fatalf("ошибка содержит пароль: %v", err)
	}
	if got := operatorvalkey.ClassifyError(err); got != operatorvalkey.ErrorKindAuth ||
		!errors.Is(err, operatorvalkey.ErrAuthentication) {
		t.Fatalf("ошибка авторизации классифицирована как %q: %v", got, err)
	}
}

func Test_TakeControl_WhenObservationCommandFails_ClassifiesServerReply(t *testing.T) {
	tests := []struct {
		name     string
		command  string
		reply    string
		kind     operatorvalkey.ErrorKind
		sentinel error
	}{
		{
			name:     "при ответе BUSY на PING возвращает ошибку занятого процесса",
			command:  "PING",
			reply:    "-BUSY running a script\r\n",
			kind:     operatorvalkey.ErrorKindBusy,
			sentinel: operatorvalkey.ErrBusy,
		},
		{
			name:     "при ответе LOADING на ROLE возвращает ошибку загрузки",
			command:  "ROLE",
			reply:    "-LOADING dataset\r\n",
			kind:     operatorvalkey.ErrorKindLoading,
			sentinel: operatorvalkey.ErrLoading,
		},
		{
			name:     "при ответе NOAUTH на INFO возвращает ошибку авторизации",
			command:  "INFO",
			reply:    "-NOAUTH authentication required\r\n",
			kind:     operatorvalkey.ErrorKindAuth,
			sentinel: operatorvalkey.ErrAuthentication,
		},
		{
			name: "при серверной ошибке INFO возвращает ошибку сервера", command: "INFO", reply: "-ERR unavailable\r\n",
			kind: operatorvalkey.ErrorKindServer,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newRESPServer(t, func(command []string) (string, bool) {
				if command[0] == test.command {
					return test.reply, false
				}
				return successfulObservationReply(command), false
			})
			client, session := dialSession(t, server.address())
			defer client.Close()
			defer session.Close()

			_, err := session.TakeControl(context.Background(), "operator-secret")
			if err == nil {
				t.Fatal("ожидалась ошибка наблюдения")
			}
			if got := operatorvalkey.ClassifyError(err); got != test.kind {
				t.Fatalf("получена категория %q, ожидалась %q: %v", got, test.kind, err)
			}
			if test.sentinel != nil && !errors.Is(err, test.sentinel) {
				t.Fatalf("ошибка не соответствует %v: %v", test.sentinel, err)
			}
		})
	}
}

func Test_TakeControl_WhenPingTimesOut_ClassifiesTransportFailure(t *testing.T) {
	server := newRESPServer(t, func(command []string) (string, bool) {
		if command[0] == "PING" {
			return "", false
		}
		return successfulObservationReply(command), false
	})
	client, session := dialSessionWithConfig(t, operatorvalkey.ClientConfig{
		Address: server.address(), DialTimeout: time.Second, CommandTimeout: 20 * time.Millisecond,
	})
	defer client.Close()
	defer session.Close()

	_, err := session.TakeControl(context.Background(), "operator-secret")
	if got := operatorvalkey.ClassifyError(err); got != operatorvalkey.ErrorKindTransport ||
		!errors.Is(err, operatorvalkey.ErrTransportFailure) {
		t.Fatalf("таймаут PING классифицирован как %q: %v", got, err)
	}
}

func Test_PromoteAndFollow_WhenChangingRole_SendEachMutationOnce(t *testing.T) {
	server := newRESPServer(t, func(command []string) (string, bool) {
		if command[0] == "REPLICAOF" {
			return "+OK\r\n", false
		}
		return successfulObservationReply(command), false
	})
	client, session := dialSession(t, server.address())
	defer client.Close()
	defer session.Close()

	if err := session.Promote(context.Background()); err != nil {
		t.Fatalf("назначить primary: %v", err)
	}
	if err := session.Follow(context.Background(), "10.42.0.10", 6379); err != nil {
		t.Fatalf("назначить upstream: %v", err)
	}
	server.mu.Lock()
	commands := slices.Clone(server.commands)
	server.mu.Unlock()
	var roleCommands [][]string
	for _, command := range commands {
		if len(command) > 0 && command[0] == "REPLICAOF" {
			roleCommands = append(roleCommands, command)
		}
	}
	want := [][]string{{"REPLICAOF", "NO", "ONE"}, {"REPLICAOF", "10.42.0.10", "6379"}}
	if !slices.EqualFunc(roleCommands, want, slices.Equal[[]string]) {
		t.Fatalf("отправлены неверные команды роли: %v", roleCommands)
	}
}

func Test_Promote_WhenResponseIsLost_SendsRoleMutationOnce(t *testing.T) {
	server := newRESPServer(t, func(command []string) (string, bool) {
		if command[0] == "REPLICAOF" {
			return "", true
		}
		return successfulObservationReply(command), false
	})
	client, session := dialSession(t, server.address())
	defer client.Close()
	defer session.Close()

	if err := session.Promote(context.Background()); err == nil {
		t.Fatal("потерянный ответ REPLICAOF принят как успех")
	}
	time.Sleep(100 * time.Millisecond)
	if got := server.commandCount("REPLICAOF"); got != 1 {
		t.Fatalf("REPLICAOF отправлен %d раз", got)
	}
}

func Test_RotateAppPassword_WhenSendingCommand_PreservesAppEnabledState(t *testing.T) {
	hash := strings.Repeat("cd", 32)
	server := newRESPServer(t, func(command []string) (string, bool) {
		if len(command) >= 2 && command[0] == "ACL" && command[1] == "SETUSER" {
			return "+OK\r\n", false
		}
		return successfulObservationReply(command), false
	})
	client, session := dialSession(t, server.address())
	defer client.Close()
	defer session.Close()

	if err := session.RotateAppPassword(context.Background(), hash); err != nil {
		t.Fatalf("сменить хеш app: %v", err)
	}
	server.mu.Lock()
	commands := slices.Clone(server.commands)
	server.mu.Unlock()
	want := []string{"ACL", "SETUSER", "app", "resetpass", "#" + hash}
	if !slices.ContainsFunc(commands, func(command []string) bool { return slices.Equal(command, want) }) {
		t.Fatalf("точная команда ротации не отправлена: %v", commands)
	}
	for _, command := range commands {
		if len(command) >= 2 && command[0] == "ACL" && command[1] == "SETUSER" &&
			(slices.Contains(command, "on") || slices.Contains(command, "off")) {
			t.Fatalf("ротация изменила состояние app: %v", command)
		}
	}
}

func Test_StopBusy_WhenScriptIsNotRunningOrFunctionIsRunning_FallsBackToFunctionKill(t *testing.T) {
	for name, scriptReply := range map[string]string{
		"когда Lua-скрипт не выполняется, отправляет FUNCTION KILL": "-NOTBUSY No scripts in execution right now.\r\n",
		"когда выполняется Function, отправляет FUNCTION KILL":      "-BUSY Valkey is busy running a script. You can only call FUNCTION KILL or SHUTDOWN NOSAVE.\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			server := newRESPServer(t, func(command []string) (string, bool) {
				if len(command) >= 2 && command[0] == "SCRIPT" && command[1] == "KILL" {
					return scriptReply, false
				}
				if len(command) >= 2 && command[0] == "FUNCTION" && command[1] == "KILL" {
					return "+OK\r\n", false
				}
				return successfulObservationReply(command), false
			})
			client, session := dialSession(t, server.address())
			defer client.Close()
			defer session.Close()

			if err := session.StopBusy(context.Background(), "operator-secret"); err != nil {
				t.Fatalf("остановить Function после ответа SCRIPT KILL: %v", err)
			}
			if server.commandCount("SCRIPT") != 1 || server.commandCount("FUNCTION") != 1 {
				t.Fatalf("неверное число команд KILL: %+v", server.commands)
			}
		})
	}
}

func Test_StopBusy_WhenScriptCannotBeKilled_ClassifiesUnkillableWithoutFunctionKill(t *testing.T) {
	server := newRESPServer(t, func(command []string) (string, bool) {
		if len(command) >= 2 && command[0] == "SCRIPT" && command[1] == "KILL" {
			return "-UNKILLABLE script already executed write commands\r\n", false
		}
		return successfulObservationReply(command), false
	})
	client, session := dialSession(t, server.address())
	defer client.Close()
	defer session.Close()

	if err := session.StopBusy(context.Background(), "operator-secret"); !errors.Is(err, operatorvalkey.ErrUnkillable) {
		t.Fatalf("UNKILLABLE классифицирован неверно: %v", err)
	}
	if server.commandCount("FUNCTION") != 0 {
		t.Fatal("после UNKILLABLE отправлен FUNCTION KILL")
	}
}

func successfulObservationReply(command []string) string {
	switch command[0] {
	case "HELLO", "AUTH":
		return "+OK\r\n"
	case "PING":
		return "+PONG\r\n"
	case "CLIENT":
		return ":1\r\n"
	case "ROLE":
		return "*3\r\n$6\r\nmaster\r\n:0\r\n*0\r\n"
	case "ACL":
		return "*4\r\n$5\r\nflags\r\n*1\r\n$3\r\noff\r\n$9\r\npasswords\r\n*0\r\n"
	case "INFO":
		body := "# Server\r\nrun_id:process-1\r\n"
		if len(command) > 1 && command[1] == "replication" {
			body = "# Replication\r\nrole:master\r\nmaster_replid:history-1\r\n" +
				"master_replid2:0000000000000000000000000000000000000000\r\n" +
				"master_repl_offset:0\r\nsecond_repl_offset:-1\r\n"
		}
		return fmt.Sprintf("$%d\r\n%s\r\n", len(body), body)
	default:
		return "-ERR unexpected command\r\n"
	}
}

func Test_ClientClose_WhenCalledRepeatedly_CallsHookOnce(t *testing.T) {
	server := newRESPServer(t, func([]string) (string, bool) {
		return "+OK\r\n", false
	})
	closed := 0
	client, err := operatorvalkey.Dial(context.Background(), operatorvalkey.ClientConfig{
		Address: server.address(), DialTimeout: time.Second, CommandTimeout: time.Second,
		OnClose: func() { closed++ },
	})
	if err != nil {
		t.Fatalf("подключить клиент: %v", err)
	}

	client.Close()
	client.Close()
	if closed != 1 {
		t.Fatalf("обработчик закрытия вызван %d раз", closed)
	}
}

func Test_Session_WhenLeaseIsLost_CancelsActiveCheckAndRejectsFurtherCommands(t *testing.T) {
	server := newRESPServer(t, func(command []string) (string, bool) {
		if command[0] == "HELLO" {
			return "+OK\r\n", false
		}

		return "", false
	})
	leaseCtx, loseLease := context.WithCancel(context.Background())
	client, session := dialSessionWithConfig(t, operatorvalkey.ClientConfig{
		Address:        server.address(),
		DialTimeout:    time.Second,
		CommandTimeout: time.Second,
		LeaseContext:   leaseCtx,
	})
	defer client.Close()
	defer session.Close()

	done := make(chan error, 1)
	go func() {
		done <- session.Ping(context.Background())
	}()
	server.waitCommands(t, "PING", 1)
	loseLease()

	select {
	case err := <-done:
		if !errors.Is(err, operatorvalkey.ErrLeaseLost) {
			t.Fatalf("ожидалась потеря Lease, получено %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("проверка не завершилась после потери Lease")
	}

	err := session.SetAppUser(context.Background(), false, strings.Repeat("ab", 32))
	if !errors.Is(err, operatorvalkey.ErrLeaseLost) {
		t.Fatalf("изменение после потери Lease вернуло %v", err)
	}
	if got := server.commandCount("ACL"); got != 0 {
		t.Fatalf("после потери Lease отправлено %d изменяющих команд", got)
	}
}

func dialSession(t *testing.T, address string) (*operatorvalkey.Client, *operatorvalkey.Session) {
	t.Helper()

	return dialSessionWithConfig(t, operatorvalkey.ClientConfig{
		Address:        address,
		DialTimeout:    time.Second,
		CommandTimeout: 500 * time.Millisecond,
	})
}

func dialSessionWithConfig(
	t *testing.T,
	cfg operatorvalkey.ClientConfig,
) (*operatorvalkey.Client, *operatorvalkey.Session) {
	t.Helper()

	client, err := operatorvalkey.Dial(context.Background(), cfg)
	if err != nil {
		t.Fatalf("подключить клиент: %v", err)
	}

	session, err := client.OpenSession(context.Background())
	if err != nil {
		client.Close()
		t.Fatalf("открыть соединение: %v", err)
	}

	return client, session
}

type respServer struct {
	t        *testing.T
	listener net.Listener
	handler  func([]string) (string, bool)

	mu       sync.Mutex
	commands [][]string
	changed  chan struct{}
	inspect  string
	pipeline map[string]bool
}

func newRESPServer(t *testing.T, handler func([]string) (string, bool)) *respServer {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("открыть тестовый порт: %v", err)
	}

	server := &respServer{
		t:        t,
		listener: listener,
		handler:  handler,
		changed:  make(chan struct{}, 1),
		pipeline: make(map[string]bool),
	}
	t.Cleanup(func() {
		_ = listener.Close()
	})
	go server.serve()

	return server
}

func (s *respServer) inspectAfter(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.inspect = name
}

func (s *respServer) pipelinedAfter(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.pipeline[name]
}

func (s *respServer) applicationCommands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	commands := make([]string, 0, len(s.commands))
	for _, command := range s.commands {
		if len(command) == 0 || command[0] == "HELLO" {
			continue
		}

		name := command[0]
		if name == "CLIENT" || name == "ACL" {
			name += " " + command[1]
		}
		commands = append(commands, name)
	}

	return commands
}

func (s *respServer) address() string {
	return s.listener.Addr().String()
}

func (s *respServer) commandCount(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	for _, command := range s.commands {
		if len(command) > 0 && command[0] == name {
			count++
		}
	}

	return count
}

func (s *respServer) waitCommands(t *testing.T, name string, count int) {
	t.Helper()

	timer := time.NewTimer(time.Second)
	defer timer.Stop()

	for s.commandCount(name) < count {
		select {
		case <-s.changed:
		case <-timer.C:
			t.Fatalf("получено %d команд %s вместо %d", s.commandCount(name), name, count)
		}
	}
}

func (s *respServer) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}

		go s.serveConnection(conn)
	}
}

func (s *respServer) serveConnection(conn net.Conn) {
	defer conn.Close()

	reader := bufio.NewReader(conn)
	for {
		command, err := readRESPCommand(reader)
		if err != nil {
			return
		}

		s.mu.Lock()
		s.commands = append(s.commands, command)
		s.mu.Unlock()
		select {
		case s.changed <- struct{}{}:
		default:
		}

		response, closeAfter := s.handler(command)
		s.mu.Lock()
		inspect := s.inspect == command[0]
		s.mu.Unlock()
		if inspect {
			_ = conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			_, err := reader.Peek(1)
			_ = conn.SetReadDeadline(time.Time{})
			if err == nil {
				s.mu.Lock()
				s.pipeline[command[0]] = true
				s.mu.Unlock()
			}
		}
		if response != "" {
			if _, err := io.WriteString(conn, response); err != nil {
				return
			}
		}
		if closeAfter {
			return
		}
	}
}

func readRESPCommand(reader *bufio.Reader) ([]string, error) {
	header, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(header) < 3 || header[0] != '*' {
		return nil, fmt.Errorf("неверный заголовок RESP %q", header)
	}

	count, err := strconv.Atoi(strings.TrimSpace(header[1:]))
	if err != nil {
		return nil, err
	}

	command := make([]string, count)
	for index := range command {
		lengthLine, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if len(lengthLine) < 3 || lengthLine[0] != '$' {
			return nil, fmt.Errorf("неверная длина RESP %q", lengthLine)
		}

		length, err := strconv.Atoi(strings.TrimSpace(lengthLine[1:]))
		if err != nil {
			return nil, err
		}
		value := make([]byte, length+2)
		if _, err := io.ReadFull(reader, value); err != nil {
			return nil, err
		}
		command[index] = string(value[:length])
	}

	return command, nil
}
