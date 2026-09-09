package valkey

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
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
	ErrClosed          = errors.New("клиент Valkey закрыт")
	ErrInvalidAddress  = errors.New("адрес Valkey не задан")
	ErrLeaseLost       = errors.New("lease оператора потерян")
	ErrUnexpectedReply = errors.New("неожиданный ответ Valkey")
)

type ClientConfig struct {
	Address        string
	Username       string
	Password       string
	DialTimeout    time.Duration
	CommandTimeout time.Duration
	LeaseContext   context.Context
}

type Client struct {
	raw            valkeygo.Client
	commandTimeout time.Duration
	leaseContext   context.Context
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
		raw:            raw,
		commandTimeout: cfg.CommandTimeout,
		leaseContext:   cfg.LeaseContext,
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
	})
}

type Session struct {
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
	result, err := s.execute(ctx, "CLIENT KILL", false, cmd)
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
	if err := s.killPreviousOperators(ctx); err != nil {
		return ProcessState{}, err
	}

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

	return ProcessState{
		Role:              role,
		RunID:             runID,
		AppEnabled:        appEnabled,
		AppPasswordHashes: passwordHashes,
	}, nil
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
	result, err := s.execute(ctx, "CLIENT KILL", false, cmd)
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

	return result, nil
}

type sanitizedCommandError struct {
	name      string
	sensitive bool
	cause     error
}

func commandError(name string, sensitive bool, cause error) error {
	return &sanitizedCommandError{name: name, sensitive: sensitive, cause: cause}
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
