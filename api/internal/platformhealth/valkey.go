package platformhealth

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	valkeygo "github.com/valkey-io/valkey-go"
)

var dnsLabelPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

type URIError struct {
	Field  string
	Reason string
}

func (e *URIError) Error() string {
	return fmt.Sprintf("поле %s: %s", e.Field, e.Reason)
}

type Target struct {
	address  string
	host     string
	password string
}

type Targets struct {
	Primary Target
	Read    *Target
}

func ParseTargets(primaryValue, readValue, baseDomain string, port int) (Targets, error) {
	primary, primaryLabel, err := parseTarget("X-Valkey-Primary", primaryValue, baseDomain, port)
	if err != nil {
		return Targets{}, err
	}

	targets := Targets{Primary: primary}
	if readValue == "" {
		return targets, nil
	}

	read, readLabel, err := parseTarget("X-Valkey-Read", readValue, baseDomain, port)
	if err != nil {
		return Targets{}, err
	}
	if readLabel != primaryLabel+"-ro" {
		return Targets{}, &URIError{Field: "X-Valkey-Read", Reason: "адрес не относится к primary"}
	}
	targets.Read = &read

	return targets, nil
}

func parseTarget(field, value, baseDomain string, port int) (Target, string, error) {
	if value == "" {
		return Target{}, "", &URIError{Field: field, Reason: "заголовок обязателен"}
	}

	parsed, err := url.Parse(value)
	if err != nil {
		return Target{}, "", &URIError{Field: field, Reason: "URI имеет неверный формат"}
	}
	if parsed.Scheme != "rediss" || parsed.Opaque != "" || parsed.Path != "" || parsed.RawPath != "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return Target{}, "", &URIError{Field: field, Reason: "нужен URI rediss без path, query и fragment"}
	}
	if parsed.User == nil || parsed.User.Username() != "app" {
		return Target{}, "", &URIError{Field: field, Reason: "пользователь должен быть app"}
	}
	password, hasPassword := parsed.User.Password()
	if !hasPassword || password == "" {
		return Target{}, "", &URIError{Field: field, Reason: "пароль обязателен"}
	}

	host := strings.ToLower(parsed.Hostname())
	baseDomain = strings.ToLower(strings.TrimSuffix(baseDomain, "."))
	if host == "" || net.ParseIP(host) != nil {
		return Target{}, "", &URIError{Field: field, Reason: "host должен быть DNS-именем"}
	}
	suffix := "." + baseDomain
	if !strings.HasSuffix(host, suffix) {
		return Target{}, "", &URIError{Field: field, Reason: "host находится вне управляемого домена"}
	}
	label := strings.TrimSuffix(host, suffix)
	if strings.Contains(label, ".") || !dnsLabelPattern.MatchString(label) {
		return Target{}, "", &URIError{Field: field, Reason: "перед базовым доменом нужен один DNS-label"}
	}
	if parsed.Port() != strconv.Itoa(port) {
		return Target{}, "", &URIError{Field: field, Reason: "port не совпадает с VALKEY_PUBLIC_PORT"}
	}

	return Target{address: net.JoinHostPort(host, parsed.Port()), host: host, password: password}, label, nil
}

type TargetProber interface {
	Probe(context.Context, Target) ValkeyTargetCheck
}

type ValkeyService struct {
	baseDomain string
	port       int
	clock      Clock
	prober     TargetProber
}

func NewValkeyService(baseDomain string, port int, clock Clock, prober TargetProber) *ValkeyService {
	return &ValkeyService{baseDomain: baseDomain, port: port, clock: clock, prober: prober}
}

func (s *ValkeyService) Check(ctx context.Context, primaryValue, readValue string) (ValkeyReport, error) {
	targets, err := ParseTargets(primaryValue, readValue, s.baseDomain, s.port)
	if err != nil {
		return ValkeyReport{}, err
	}

	checkedAt := s.clock.Now().UTC()
	checkContext, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()

	primaryResult := make(chan ValkeyTargetCheck, 1)
	go func() {
		primaryResult <- s.prober.Probe(checkContext, targets.Primary)
	}()

	var readResult chan ValkeyTargetCheck
	if targets.Read != nil {
		readResult = make(chan ValkeyTargetCheck, 1)
		go func() {
			readResult <- s.prober.Probe(checkContext, *targets.Read)
		}()
	}

	primary, primaryDone := ValkeyTargetCheck{}, false
	read, readDone := ValkeyTargetCheck{}, targets.Read == nil
	for !primaryDone || !readDone {
		select {
		case primary = <-primaryResult:
			primaryDone = true
		case read = <-readResult:
			readDone = true
		case <-checkContext.Done():
			if !primaryDone {
				primary = ValkeyTargetCheck{
					Status: StatusFail, LatencyMS: RequestTimeout.Milliseconds(), Code: "timeout",
				}
				primaryDone = true
			}
			if !readDone {
				read = ValkeyTargetCheck{Status: StatusFail, LatencyMS: RequestTimeout.Milliseconds(), Code: "timeout"}
				readDone = true
			}
		}
	}

	checks := ValkeyChecks{Primary: primary}
	status := primary.Status
	if targets.Read != nil {
		checks.Read = &read
		if read.Status == StatusFail {
			status = StatusFail
		}
	}

	return ValkeyReport{Status: status, CheckedAt: checkedAt, Checks: checks}, nil
}

type DialContextFunc func(context.Context, string, *net.Dialer, *tls.Config) (net.Conn, error)

type ValkeyGoProber struct {
	RootCAs       *x509.CertPool
	DialContextFn DialContextFunc
	Timeout       time.Duration
}

func (p ValkeyGoProber) Probe(ctx context.Context, target Target) ValkeyTargetCheck {
	started := time.Now()
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = RequestTimeout
	}
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    p.RootCAs,
		ServerName: target.host,
	}
	client, err := valkeygo.NewClient(valkeygo.ClientOption{
		InitAddress:       []string{target.address},
		Username:          "app",
		Password:          target.password,
		TLSConfig:         tlsConfig,
		DialCtxFn:         p.DialContextFn,
		Dialer:            net.Dialer{Timeout: timeout},
		ConnWriteTimeout:  timeout,
		ClientSetInfo:     valkeygo.DisableClientSetInfo,
		DisableRetry:      true,
		DisableCache:      true,
		AlwaysRESP2:       true,
		ForceSingleClient: true,
		PipelineMultiplex: -1,
	})
	if err != nil {
		return failedValkeyCheck(classifyValkeyError(err, "connect_failed"), started)
	}
	defer client.Close()

	result := client.Do(ctx, client.B().Ping().Build())
	reply, err := result.ToString()
	if err != nil {
		return failedValkeyCheck(classifyValkeyError(err, "ping_failed"), started)
	}
	if reply != "PONG" {
		return failedValkeyCheck("ping_failed", started)
	}

	return ValkeyTargetCheck{Status: StatusOK, LatencyMS: time.Since(started).Milliseconds()}
}

func classifyValkeyError(err error, fallback string) string {
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, os.ErrDeadlineExceeded) {
		return "timeout"
	}

	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return "dns_failed"
	}
	var certificateError *tls.CertificateVerificationError
	var hostnameError x509.HostnameError
	var authorityError x509.UnknownAuthorityError
	var recordError tls.RecordHeaderError
	if errors.As(err, &certificateError) || errors.As(err, &hostnameError) || errors.As(err, &authorityError) ||
		errors.As(err, &recordError) {
		return "tls_failed"
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		if response, ok := valkeygo.IsValkeyErr(current); ok {
			message := strings.ToUpper(response.Error())
			if strings.HasPrefix(message, "WRONGPASS") || strings.HasPrefix(message, "NOAUTH") ||
				strings.HasPrefix(message, "NOPERM") {
				return "auth_failed"
			}
		}
	}

	return fallback
}

func failedValkeyCheck(code string, started time.Time) ValkeyTargetCheck {
	return ValkeyTargetCheck{Status: StatusFail, LatencyMS: time.Since(started).Milliseconds(), Code: code}
}
