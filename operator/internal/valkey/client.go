package valkey

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	valkeygo "github.com/valkey-io/valkey-go"
)

const (
	defaultDialTimeout    = 3 * time.Second
	defaultCommandTimeout = 5 * time.Second
)

var (
	ErrClosed           = errors.New("клиент Valkey закрыт")
	ErrInvalidAddress   = errors.New("адрес Valkey не задан")
	ErrLeaseLost        = errors.New("lease оператора потерян")
	ErrUnexpectedReply  = errors.New("неожиданный ответ Valkey")
	ErrTransportFailure = errors.New("транспорт Valkey недоступен")
	ErrBusy             = errors.New("valkey занят выполнением скрипта")
	ErrLoading          = errors.New("valkey загружает данные")
	ErrAuthentication   = errors.New("valkey отклонил авторизацию")
	ErrNoBusyProcess    = errors.New("занятый Lua или Function не найден")
	ErrUnkillable       = errors.New("занятый Lua или Function уже изменил данные")
	ErrProcessChanged   = errors.New("процесс Valkey изменился во время чтения")
)

type ErrorKind string

const (
	ErrorKindOther     ErrorKind = "other"
	ErrorKindTransport ErrorKind = "transport"
	ErrorKindBusy      ErrorKind = "busy"
	ErrorKindLoading   ErrorKind = "loading"
	ErrorKindAuth      ErrorKind = "auth"
	ErrorKindServer    ErrorKind = "server"
)

type ClientConfig struct {
	Address        string
	Username       string
	Password       string
	DialTimeout    time.Duration
	CommandTimeout time.Duration
	LeaseContext   context.Context
	OnClose        func()
}

type Client struct {
	address        string
	raw            valkeygo.Client
	commandTimeout time.Duration
	leaseContext   context.Context
	onClose        func()
	closeOnce      sync.Once
}

func Dial(ctx context.Context, cfg ClientConfig) (*Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cfg.Address == "" {
		return nil, ErrInvalidAddress
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = defaultDialTimeout
	}
	if cfg.CommandTimeout <= 0 {
		cfg.CommandTimeout = defaultCommandTimeout
	}
	if cfg.LeaseContext == nil {
		cfg.LeaseContext = context.Background()
	}
	if err := cfg.LeaseContext.Err(); err != nil {
		return nil, ErrLeaseLost
	}

	raw, err := valkeygo.NewClient(valkeygo.ClientOption{
		InitAddress:           []string{cfg.Address},
		Username:              cfg.Username,
		Password:              cfg.Password,
		Dialer:                net.Dialer{Timeout: cfg.DialTimeout},
		ConnWriteTimeout:      cfg.CommandTimeout,
		ClientSetInfo:         valkeygo.DisableClientSetInfo,
		DisableRetry:          true,
		DisableCache:          true,
		DisableAutoPipelining: true,
		AlwaysRESP2:           true,
		ForceSingleClient:     true,
	})
	if err != nil {
		if raw != nil {
			raw.Close()
		}
		if cfg.Username != "" || cfg.Password != "" {
			return nil, commandError("AUTH", true, err)
		}

		return nil, fmt.Errorf("подключиться к Valkey: %w", err)
	}
	if err := ctx.Err(); err != nil {
		raw.Close()

		return nil, err
	}

	return &Client{
		address:        cfg.Address,
		raw:            raw,
		commandTimeout: cfg.CommandTimeout,
		leaseContext:   cfg.LeaseContext,
		onClose:        cfg.OnClose,
	}, nil
}

func (c *Client) OpenSession(ctx context.Context) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c == nil || c.raw == nil {
		return nil, ErrClosed
	}

	raw, release := c.raw.Dedicate()

	return &Session{
		address:        c.address,
		raw:            raw,
		release:        release,
		commandTimeout: c.commandTimeout,
		leaseContext:   c.leaseContext,
	}, nil
}

func (c *Client) Close() {
	if c == nil {
		return
	}

	c.closeOnce.Do(func() {
		if c.raw != nil {
			c.raw.Close()
		}
		if c.onClose != nil {
			c.onClose()
		}
	})
}

type Session struct {
	address        string
	raw            valkeygo.DedicatedClient
	release        func()
	commandTimeout time.Duration
	leaseContext   context.Context
	closeOnce      sync.Once
}

type ProcessState struct {
	Role              string
	RunID             string
	AppEnabled        bool
	AppPasswordHashes []string
	Replication       *ReplicationState
}

type ReplicationState struct {
	ReplicationID          string
	SecondaryReplicationID string
	SecondaryOffset        int64
	Offset                 int64
	UpstreamHost           string
	UpstreamPort           int32
	LinkUp                 bool
	SyncInProgress         bool
	SyncedAt               *time.Time
}

type Metrics struct {
	Role             string
	RunID            string
	UsedMemoryBytes  int64
	MaxmemoryBytes   int64
	ConnectedClients int64
	OpsPerSec        int64
	KeyspaceHits     int64
	KeyspaceMisses   int64
	EvictedKeys      int64
}

func (s *Session) Close() {
	if s == nil {
		return
	}

	s.closeOnce.Do(func() {
		if s.raw != nil {
			s.raw.Close()
		} else if s.release != nil {
			s.release()
		}
	})
}

func (s *Session) SetAppUser(ctx context.Context, enabled bool, passwordHash string) error {
	state := "off"
	if enabled {
		state = "on"
	}

	cmd := s.raw.B().AclSetuser().Username("app").Rule(state, "resetpass", "#"+passwordHash).Build()
	result, err := s.execute(ctx, "ACL SETUSER", true, cmd)
	if err != nil {
		return err
	}
	if reply, err := result.ToString(); err != nil || reply != "OK" {
		return commandError("ACL SETUSER", true, errors.Join(ErrUnexpectedReply, err))
	}

	return nil
}

func (s *Session) RotateAppPassword(ctx context.Context, passwordHash string) error {
	cmd := s.raw.B().AclSetuser().Username("app").Rule("resetpass", "#"+passwordHash).Build()
	result, err := s.execute(ctx, "ACL SETUSER", true, cmd)
	if err != nil {
		return err
	}
	if reply, err := result.ToString(); err != nil || reply != "OK" {
		return commandError("ACL SETUSER", true, errors.Join(ErrUnexpectedReply, err))
	}
	return nil
}

func (s *Session) StopBusy(ctx context.Context, operatorPassword string) error {
	if err := s.authenticate(ctx, operatorPassword); err != nil {
		return err
	}
	if err := s.killBusyCommand(ctx, "SCRIPT KILL", s.raw.B().ScriptKill().Build()); err == nil {
		return nil
	} else if !errors.Is(err, ErrNoBusyProcess) && !functionKillRequired(err) {
		return err
	}
	return s.killBusyCommand(ctx, "FUNCTION KILL", s.raw.B().FunctionKill().Build())
}

func functionKillRequired(err error) bool {
	for current := err; current != nil; current = errors.Unwrap(current) {
		if response, ok := valkeygo.IsValkeyErr(current); ok {
			message := response.Error()
			return strings.HasPrefix(message, "BUSY ") && strings.Contains(message, "FUNCTION KILL")
		}
	}
	return false
}

func valkeyErrorPrefix(err error, prefix string) bool {
	for current := err; current != nil; current = errors.Unwrap(current) {
		if response, ok := valkeygo.IsValkeyErr(current); ok {
			message := response.Error()
			return message == prefix || strings.HasPrefix(message, prefix+" ")
		}
	}
	return false
}

func (s *Session) Ping(ctx context.Context) error {
	result, err := s.execute(ctx, "PING", false, s.raw.B().Ping().Build())
	if err != nil {
		return err
	}
	if reply, err := result.ToString(); err != nil || reply != "PONG" {
		return commandError("PING", false, errors.Join(ErrUnexpectedReply, err))
	}

	return nil
}

func (s *Session) KillAppClients(ctx context.Context) error {
	cmd := s.raw.B().ClientKill().User("app").SkipmeYes().Build()
	result, err := s.execute(ctx, "CLIENT KILL app", false, cmd)
	if err != nil {
		return err
	}
	if _, err := result.AsInt64(); err != nil {
		return commandError("CLIENT KILL", false, errors.Join(ErrUnexpectedReply, err))
	}

	return nil
}

func (s *Session) TakeControl(ctx context.Context, operatorPassword string) (ProcessState, error) {
	if err := s.authenticate(ctx, operatorPassword); err != nil {
		return ProcessState{}, err
	}
	if err := s.Ping(ctx); err != nil {
		return ProcessState{}, err
	}
	if err := s.killPreviousOperators(ctx); err != nil {
		return ProcessState{}, err
	}
	return s.readState(ctx)
}

func (s *Session) Observe(ctx context.Context, operatorPassword string) (ProcessState, error) {
	if err := s.authenticate(ctx, operatorPassword); err != nil {
		return ProcessState{}, err
	}
	if err := s.Ping(ctx); err != nil {
		return ProcessState{}, err
	}
	return s.readState(ctx)
}

func (s *Session) ReadMetrics(ctx context.Context, operatorPassword string) (Metrics, error) {
	if err := s.authenticate(ctx, operatorPassword); err != nil {
		return Metrics{}, err
	}

	result, err := s.execute(ctx, "INFO", false, s.raw.B().Info().Build())
	if err != nil {
		return Metrics{}, err
	}
	info, err := result.ToString()
	if err != nil {
		return Metrics{}, commandError("INFO", false, errors.Join(ErrUnexpectedReply, err))
	}

	values := parseInfo(info)
	role := values["role"]
	switch role {
	case "master":
		role = "primary"
	case "slave":
		role = "replica"
	default:
		return Metrics{}, commandError("INFO", false, ErrUnexpectedReply)
	}
	if values["run_id"] == "" {
		return Metrics{}, commandError("INFO", false, ErrUnexpectedReply)
	}

	fields := [...]struct {
		name   string
		target *int64
	}{
		{name: "used_memory"},
		{name: "maxmemory"},
		{name: "connected_clients"},
		{name: "instantaneous_ops_per_sec"},
		{name: "keyspace_hits"},
		{name: "keyspace_misses"},
		{name: "evicted_keys"},
	}
	metrics := Metrics{Role: role, RunID: values["run_id"]}
	fields[0].target = &metrics.UsedMemoryBytes
	fields[1].target = &metrics.MaxmemoryBytes
	fields[2].target = &metrics.ConnectedClients
	fields[3].target = &metrics.OpsPerSec
	fields[4].target = &metrics.KeyspaceHits
	fields[5].target = &metrics.KeyspaceMisses
	fields[6].target = &metrics.EvictedKeys
	for _, field := range fields {
		value, err := parseInfoInt(values, field.name)
		if err != nil || value < 0 {
			return Metrics{}, commandError("INFO", false, errors.Join(ErrUnexpectedReply, err))
		}
		*field.target = value
	}

	return metrics, nil
}

func (s *Session) VerifyRunID(ctx context.Context, expected string) error {
	runID, err := s.readRunID(ctx)
	if err != nil {
		return err
	}
	if runID != expected {
		return commandError("INFO server", false, ErrProcessChanged)
	}

	return nil
}

func (s *Session) Promote(ctx context.Context) error {
	result, err := s.execute(
		ctx,
		"REPLICAOF NO ONE",
		false,
		s.raw.B().Replicaof().No().One().Build(),
	)
	if err != nil {
		return err
	}
	if reply, err := result.ToString(); err != nil || reply != "OK" {
		return commandError("REPLICAOF NO ONE", false, errors.Join(ErrUnexpectedReply, err))
	}

	return nil
}

func (s *Session) Follow(ctx context.Context, host string, port int32) error {
	result, err := s.execute(
		ctx,
		"REPLICAOF",
		false,
		s.raw.B().Replicaof().Host(host).Port(int64(port)).Build(),
	)
	if err != nil {
		return err
	}
	if reply, err := result.ToString(); err != nil || reply != "OK" {
		return commandError("REPLICAOF", false, errors.Join(ErrUnexpectedReply, err))
	}

	return nil
}

func (s *Session) readState(ctx context.Context) (ProcessState, error) {
	role, err := s.readRole(ctx)
	if err != nil {
		return ProcessState{}, err
	}
	appEnabled, passwordHashes, err := s.readAppACL(ctx)
	if err != nil {
		return ProcessState{}, err
	}
	runID, err := s.readRunID(ctx)
	if err != nil {
		return ProcessState{}, err
	}
	replication, err := s.readReplication(ctx)
	if err != nil {
		return ProcessState{}, err
	}

	return ProcessState{
		Role:              role,
		RunID:             runID,
		AppEnabled:        appEnabled,
		AppPasswordHashes: passwordHashes,
		Replication:       replication,
	}, nil
}

func (s *Session) killBusyCommand(ctx context.Context, name string, cmd valkeygo.Completed) error {
	result, err := s.execute(ctx, name, false, cmd)
	if err != nil {
		switch {
		case valkeyErrorPrefix(err, "NOTBUSY"):
			return ErrNoBusyProcess
		case valkeyErrorPrefix(err, "UNKILLABLE"):
			return ErrUnkillable
		default:
			return err
		}
	}
	if reply, err := result.ToString(); err != nil || reply != "OK" {
		return commandError(name, false, errors.Join(ErrUnexpectedReply, err))
	}
	return nil
}

func (s *Session) authenticate(ctx context.Context, password string) error {
	cmd := s.raw.B().Auth().Username("operator").Password(password).Build()
	result, err := s.execute(ctx, "AUTH", true, cmd)
	if err != nil {
		return err
	}
	if reply, err := result.ToString(); err != nil || reply != "OK" {
		return commandError("AUTH", true, errors.Join(ErrUnexpectedReply, err))
	}

	return nil
}

func (s *Session) killPreviousOperators(ctx context.Context) error {
	cmd := s.raw.B().ClientKill().User("operator").SkipmeYes().Build()
	result, err := s.execute(ctx, "CLIENT KILL operator", false, cmd)
	if err != nil {
		return err
	}
	if _, err := result.AsInt64(); err != nil {
		return commandError("CLIENT KILL", false, errors.Join(ErrUnexpectedReply, err))
	}

	return nil
}

func (s *Session) readRole(ctx context.Context) (string, error) {
	result, err := s.execute(ctx, "ROLE", false, s.raw.B().Role().Build())
	if err != nil {
		return "", err
	}

	values, err := result.ToArray()
	if err != nil || len(values) == 0 {
		return "", commandError("ROLE", false, errors.Join(ErrUnexpectedReply, err))
	}
	role, err := values[0].ToString()
	if err != nil {
		return "", commandError("ROLE", false, errors.Join(ErrUnexpectedReply, err))
	}

	switch role {
	case "master":
		return "primary", nil
	case "slave":
		return "replica", nil
	default:
		return "", commandError("ROLE", false, ErrUnexpectedReply)
	}
}

func (s *Session) readAppACL(ctx context.Context) (bool, []string, error) {
	cmd := s.raw.B().AclGetuser().Username("app").Build()
	result, err := s.execute(ctx, "ACL GETUSER", false, cmd)
	if err != nil {
		return false, nil, err
	}

	values, err := result.ToArray()
	if err != nil || len(values)%2 != 0 {
		return false, nil, commandError("ACL GETUSER", false, errors.Join(ErrUnexpectedReply, err))
	}

	var flags []string
	var passwordHashes []string
	for index := 0; index < len(values); index += 2 {
		name, err := values[index].ToString()
		if err != nil {
			return false, nil, commandError("ACL GETUSER", false, errors.Join(ErrUnexpectedReply, err))
		}

		switch name {
		case "flags":
			flags, err = values[index+1].AsStrSlice()
		case "passwords":
			passwordHashes, err = values[index+1].AsStrSlice()
		}
		if err != nil {
			return false, nil, commandError("ACL GETUSER", false, errors.Join(ErrUnexpectedReply, err))
		}
	}
	if !slices.Contains(flags, "on") && !slices.Contains(flags, "off") {
		return false, nil, commandError("ACL GETUSER", false, ErrUnexpectedReply)
	}

	return slices.Contains(flags, "on"), passwordHashes, nil
}

func (s *Session) readRunID(ctx context.Context) (string, error) {
	cmd := s.raw.B().Info().Section("server").Build()
	result, err := s.execute(ctx, "INFO server", false, cmd)
	if err != nil {
		return "", err
	}

	info, err := result.ToString()
	if err != nil {
		return "", commandError("INFO server", false, errors.Join(ErrUnexpectedReply, err))
	}
	for line := range strings.SplitSeq(info, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if runID, found := strings.CutPrefix(line, "run_id:"); found && runID != "" {
			return runID, nil
		}
	}

	return "", commandError("INFO server", false, ErrUnexpectedReply)
}

func (s *Session) readReplication(ctx context.Context) (*ReplicationState, error) {
	cmd := s.raw.B().Info().Section("replication").Build()
	result, err := s.execute(ctx, "INFO replication", false, cmd)
	if err != nil {
		return nil, err
	}
	info, err := result.ToString()
	if err != nil {
		return nil, commandError("INFO replication", false, errors.Join(ErrUnexpectedReply, err))
	}
	values := parseInfo(info)
	role := values["role"]
	if role != "master" && role != "slave" {
		return nil, commandError("INFO replication", false, ErrUnexpectedReply)
	}
	offsetName := "master_repl_offset"
	if role == "slave" {
		offsetName = "slave_repl_offset"
	}
	offset, err := parseInfoInt(values, offsetName)
	if err != nil {
		return nil, commandError("INFO replication", false, errors.Join(ErrUnexpectedReply, err))
	}
	secondaryOffset, err := optionalInfoInt(values, "second_repl_offset")
	if err != nil {
		return nil, commandError("INFO replication", false, errors.Join(ErrUnexpectedReply, err))
	}
	upstreamPort, err := optionalInfoInt(values, "master_port")
	if err != nil {
		return nil, commandError("INFO replication", false, errors.Join(ErrUnexpectedReply, err))
	}

	return &ReplicationState{
		ReplicationID:          values["master_replid"],
		SecondaryReplicationID: values["master_replid2"],
		SecondaryOffset:        secondaryOffset,
		Offset:                 offset,
		UpstreamHost:           values["master_host"],
		UpstreamPort:           int32(upstreamPort),
		LinkUp:                 values["master_link_status"] == "up",
		SyncInProgress:         values["master_sync_in_progress"] == "1",
	}, nil
}

func parseInfo(info string) map[string]string {
	values := make(map[string]string)
	for line := range strings.SplitSeq(info, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, found := strings.Cut(line, ":")
		if found {
			values[name] = value
		}
	}

	return values
}

func parseInfoInt(values map[string]string, name string) (int64, error) {
	value, found := values[name]
	if !found {
		return 0, ErrUnexpectedReply
	}

	return strconv.ParseInt(value, 10, 64)
}

func optionalInfoInt(values map[string]string, name string) (int64, error) {
	value, found := values[name]
	if !found || value == "" {
		return 0, nil
	}

	return strconv.ParseInt(value, 10, 64)
}

func (s *Session) execute(
	ctx context.Context,
	name string,
	sensitive bool,
	cmd valkeygo.Completed,
) (valkeygo.ValkeyResult, error) {
	if s == nil || s.raw == nil {
		return valkeygo.NewErrorResult(ErrClosed), ErrClosed
	}
	if s.leaseContext.Err() != nil {
		return valkeygo.NewErrorResult(ErrLeaseLost), commandError(name, sensitive, ErrLeaseLost)
	}

	commandCtx, cancel := context.WithCancelCause(ctx)
	stopLeaseCancellation := context.AfterFunc(s.leaseContext, func() {
		cancel(ErrLeaseLost)
	})
	if err := runCommandControl(commandCtx, s.address, name, commandControlBefore); err != nil {
		stopLeaseCancellation()
		cancel(nil)
		return valkeygo.NewErrorResult(err), commandError(name, sensitive, err)
	}
	timedCtx, cancelTimeout := context.WithTimeout(commandCtx, s.commandTimeout)
	defer func() {
		cancelTimeout()
		stopLeaseCancellation()
		cancel(nil)
	}()

	completed := make(chan valkeygo.ValkeyResult, 1)
	go func() {
		completed <- s.raw.Do(timedCtx, cmd)
	}()

	var result valkeygo.ValkeyResult
	select {
	case result = <-completed:
	case <-timedCtx.Done():
		go s.Close()
		if cause := context.Cause(commandCtx); cause != nil {
			return valkeygo.NewErrorResult(cause), commandError(name, sensitive, cause)
		}

		return valkeygo.NewErrorResult(timedCtx.Err()), commandError(name, sensitive, timedCtx.Err())
	}
	if cause := context.Cause(commandCtx); cause != nil {
		return result, commandError(name, sensitive, cause)
	}
	if err := timedCtx.Err(); err != nil {
		return result, commandError(name, sensitive, err)
	}
	if err := result.Error(); err != nil {
		return result, commandError(name, sensitive, err)
	}
	if err := runCommandControl(commandCtx, s.address, name, commandControlAfter); err != nil {
		s.Close()
		return result, commandError(name, sensitive, err)
	}

	return result, nil
}

type commandControlStage uint8

const (
	commandControlBefore commandControlStage = iota
	commandControlAfter
)

type sanitizedCommandError struct {
	name      string
	sensitive bool
	cause     error
	kind      ErrorKind
}

func commandError(name string, sensitive bool, cause error) error {
	return &sanitizedCommandError{
		name: name, sensitive: sensitive, cause: cause, kind: classifyCause(cause),
	}
}

func ClassifyError(err error) ErrorKind {
	if command, ok := errors.AsType[*sanitizedCommandError](err); ok {
		return command.kind
	}

	return classifyCause(err)
}

func classifyCause(err error) ErrorKind {
	if err == nil || errors.Is(err, ErrLeaseLost) || errors.Is(err, ErrClosed) ||
		errors.Is(err, ErrInvalidAddress) || errors.Is(err, context.Canceled) {
		return ErrorKindOther
	}
	switch {
	case errors.Is(err, ErrTransportFailure):
		return ErrorKindTransport
	case errors.Is(err, ErrBusy):
		return ErrorKindBusy
	case errors.Is(err, ErrLoading):
		return ErrorKindLoading
	case errors.Is(err, ErrAuthentication):
		return ErrorKindAuth
	}
	if response, ok := valkeygo.IsValkeyErr(err); ok {
		message := response.Error()
		switch {
		case message == "BUSY" || strings.HasPrefix(message, "BUSY "):
			return ErrorKindBusy
		case message == "LOADING" || strings.HasPrefix(message, "LOADING "):
			return ErrorKindLoading
		case message == "NOAUTH" || strings.HasPrefix(message, "NOAUTH "),
			message == "WRONGPASS" || strings.HasPrefix(message, "WRONGPASS "):
			return ErrorKindAuth
		default:
			return ErrorKindServer
		}
	}
	if errors.Is(err, ErrUnexpectedReply) {
		return ErrorKindServer
	}

	return ErrorKindTransport
}

func (e *sanitizedCommandError) Error() string {
	if e.sensitive {
		return fmt.Sprintf("команда Valkey %s завершилась ошибкой", e.name)
	}

	return fmt.Sprintf("команда Valkey %s завершилась ошибкой: %v", e.name, e.cause)
}

func (e *sanitizedCommandError) Unwrap() error {
	return e.cause
}

func (e *sanitizedCommandError) Is(target error) bool {
	switch target {
	case ErrTransportFailure:
		return e.kind == ErrorKindTransport
	case ErrBusy:
		return e.kind == ErrorKindBusy
	case ErrLoading:
		return e.kind == ErrorKindLoading
	case ErrAuthentication:
		return e.kind == ErrorKindAuth
	default:
		return false
	}
}
