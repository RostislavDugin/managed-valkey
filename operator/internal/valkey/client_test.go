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

func TestCommandCancellation(t *testing.T) {
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

func TestMutationIsNotRetriedAfterLostResponse(t *testing.T) {
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

func TestSensitiveCommandErrorDoesNotContainArguments(t *testing.T) {
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

func TestTakeControlWaitsForAuthenticationAndReadsState(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	server := newRESPServer(t, func(command []string) (string, bool) {
		switch command[0] {
		case "HELLO":
			return "+OK\r\n", false
		case "AUTH":
			return "+OK\r\n", false
		case "CLIENT":
			return ":1\r\n", false
		case "ROLE":
			return "*3\r\n$6\r\nmaster\r\n:0\r\n*0\r\n", false
		case "ACL":
			return "*4\r\n$5\r\nflags\r\n*1\r\n$3\r\noff\r\n$9\r\npasswords\r\n*1\r\n$64\r\n" + hash + "\r\n", false
		case "INFO":
			body := "# Server\r\nrun_id:process-1\r\n"
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
	if server.pipelinedAfter("AUTH") {
		t.Fatal("следующая команда отправлена до ответа AUTH")
	}

	want := []string{"AUTH", "CLIENT KILL", "ROLE", "ACL GETUSER", "INFO"}
	if got := server.applicationCommands(); !slices.Equal(got, want) {
		t.Fatalf("порядок команд %v, ожидался %v", got, want)
	}
}

func TestAuthenticationErrorDoesNotContainPassword(t *testing.T) {
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
}

func TestLeaseLossCancelsChecksAndRejectsFurtherCommands(t *testing.T) {
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
