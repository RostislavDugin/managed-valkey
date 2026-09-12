package platformhealth

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testBaseDomain = "valkey.test"
	testValkeyPort = 41379
)

type targetProberStub struct {
	probe func(context.Context, Target) ValkeyTargetCheck
}

func (p targetProberStub) Probe(ctx context.Context, target Target) ValkeyTargetCheck {
	return p.probe(ctx, target)
}

func Test_ParseTargets_WithValidUris_ReturnsExpectedTargets(t *testing.T) {
	targets, err := ParseTargets(
		"rediss://app:primary-secret@cache-abc123.valkey.test:41379",
		"rediss://app:read-secret@cache-abc123-ro.valkey.test:41379",
		testBaseDomain,
		testValkeyPort,
	)
	if err != nil {
		t.Fatalf("разобрать допустимые URI: %v", err)
	}

	if targets.Primary.address != "cache-abc123.valkey.test:41379" ||
		targets.Primary.host != "cache-abc123.valkey.test" ||
		targets.Primary.password != "primary-secret" {
		t.Errorf("неверный primary: %+v", targets.Primary)
	}
	if targets.Read == nil || targets.Read.address != "cache-abc123-ro.valkey.test:41379" ||
		targets.Read.password != "read-secret" {
		t.Errorf("неверный read: %+v", targets.Read)
	}
}

func Test_ParseTargets_WithoutRead_ReturnsOnlyPrimary(t *testing.T) {
	targets, err := ParseTargets(
		"rediss://app:secret@cache-abc123.valkey.test:41379",
		"",
		testBaseDomain,
		testValkeyPort,
	)
	if err != nil {
		t.Fatalf("разобрать URI primary: %v", err)
	}
	if targets.Read != nil {
		t.Errorf("неожиданный read: %+v", targets.Read)
	}
}

func Test_ParseTargets_WithInvalidUri_ReturnsSafeValidationError(t *testing.T) {
	tests := []struct {
		name    string
		primary string
		read    string
	}{
		{name: "primary отсутствует"},
		{name: "схема без TLS", primary: "redis://app:secret@cache.valkey.test:41379"},
		{name: "передан IP", primary: "rediss://app:secret@127.0.0.1:41379"},
		{name: "другой домен", primary: "rediss://app:secret@cache.example.test:41379"},
		{name: "другой порт", primary: "rediss://app:secret@cache.valkey.test:6379"},
		{name: "другой пользователь", primary: "rediss://default:secret@cache.valkey.test:41379"},
		{name: "пароль отсутствует", primary: "rediss://app@cache.valkey.test:41379"},
		{name: "есть path", primary: "rediss://app:secret@cache.valkey.test:41379/0"},
		{name: "есть query", primary: "rediss://app:secret@cache.valkey.test:41379?db=0"},
		{name: "есть fragment", primary: "rediss://app:secret@cache.valkey.test:41379#part"},
		{name: "два label перед доменом", primary: "rediss://app:secret@one.cache.valkey.test:41379"},
		{
			name:    "read другого инстанса",
			primary: "rediss://app:secret@cache-a.valkey.test:41379",
			read:    "rediss://app:secret@cache-b-ro.valkey.test:41379",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseTargets(test.primary, test.read, testBaseDomain, testValkeyPort)
			if err == nil {
				t.Fatal("недопустимый URI принят")
			}
			if strings.Contains(err.Error(), "secret") ||
				(test.primary != "" && strings.Contains(err.Error(), test.primary)) {
				t.Errorf("ошибка раскрывает URI или пароль: %v", err)
			}
		})
	}
}

func Test_CheckValkeyHealth_WithInvalidUri_DoesNotProbeNetwork(t *testing.T) {
	var calls atomic.Int32
	service := NewValkeyService(testBaseDomain, testValkeyPort, fixedClock{now: time.Now()}, targetProberStub{
		probe: func(context.Context, Target) ValkeyTargetCheck {
			calls.Add(1)

			return ValkeyTargetCheck{Status: StatusOK}
		},
	})

	_, err := service.Check(context.Background(), "rediss://app:secret@127.0.0.1:41379", "")
	if err == nil {
		t.Fatal("недопустимый URI принят")
	}
	if calls.Load() != 0 {
		t.Errorf("выполнено сетевых проверок: %d", calls.Load())
	}
}

func Test_CheckValkeyHealth_WithTwoTargets_ProbesThemConcurrently(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	service := NewValkeyService(testBaseDomain, testValkeyPort, fixedClock{now: time.Now()}, targetProberStub{
		probe: func(_ context.Context, target Target) ValkeyTargetCheck {
			started <- target.host
			<-release

			return ValkeyTargetCheck{Status: StatusOK}
		},
	})
	reports := make(chan ValkeyReport, 1)
	go func() {
		report, _ := service.Check(
			context.Background(),
			"rediss://app:secret@cache.valkey.test:41379",
			"rediss://app:secret@cache-ro.valkey.test:41379",
		)
		reports <- report
	}()

	waitTargetStarted(t, started)
	waitTargetStarted(t, started)
	close(release)
	report := <-reports

	if report.Status != StatusOK || report.Checks.Read == nil || report.Checks.Read.Status != StatusOK {
		t.Errorf("неверный результат двух адресов: %+v", report)
	}
}

func Test_ProbeValkey_WithTlsAuthAndPing_ReturnsSuccessAndClosesConnection(t *testing.T) {
	certificate, roots := testCertificate(t, []string{"cache.valkey.test"})
	server := startTLSServer(t, certificate, "secret", true)
	dialServer := dialTLSAddress(server.address)
	var dials atomic.Int32
	prober := ValkeyGoProber{
		RootCAs: roots,
		DialContextFn: func(ctx context.Context, address string, dialer *net.Dialer, tlsConfig *tls.Config) (net.Conn, error) {
			dials.Add(1)

			return dialServer(ctx, address, dialer, tlsConfig)
		},
		Timeout: time.Second,
	}

	result := prober.Probe(context.Background(), testTarget("cache.valkey.test", "secret"))

	if result.Status != StatusOK || result.Code != "" {
		t.Errorf("неверный результат PING: %+v", result)
	}
	waitConnectionClosed(t, server.closed)
	if dials.Load() != 1 {
		t.Errorf("выполнено подключений: %d", dials.Load())
	}
	commands := <-server.commands
	seenAuth := false
	seenPing := false
	for _, command := range commands {
		switch strings.ToUpper(command[0]) {
		case "AUTH":
			seenAuth = true
		case "PING":
			seenPing = true
		case "HELLO":
		default:
			t.Errorf("отправлена команда с данными или неизвестная команда: %q", command[0])
		}
	}
	if !seenAuth || !seenPing {
		t.Errorf("AUTH или PING отсутствует: %+v", commands)
	}
}

func Test_ProbeValkey_WithWrongCertificate_ReturnsTlsFailure(t *testing.T) {
	certificate, roots := testCertificate(t, []string{"other.valkey.test"})
	server := startTLSServer(t, certificate, "secret", true)
	prober := ValkeyGoProber{RootCAs: roots, DialContextFn: dialTLSAddress(server.address), Timeout: time.Second}

	result := prober.Probe(context.Background(), testTarget("cache.valkey.test", "secret"))

	if result.Status != StatusFail || result.Code != "tls_failed" {
		t.Errorf("неверная ошибка TLS: %+v", result)
	}
}

func Test_ProbeValkey_WithWrongPassword_ReturnsAuthFailure(t *testing.T) {
	certificate, roots := testCertificate(t, []string{"cache.valkey.test"})
	server := startTLSServer(t, certificate, "expected-secret", true)
	prober := ValkeyGoProber{RootCAs: roots, DialContextFn: dialTLSAddress(server.address), Timeout: time.Second}

	result := prober.Probe(context.Background(), testTarget("cache.valkey.test", "wrong-secret"))

	if result.Status != StatusFail || result.Code != "auth_failed" {
		t.Errorf("неверная ошибка AUTH: %+v", result)
	}
}

func Test_ProbeValkey_WhenServerDoesNotReply_ReturnsTimeout(t *testing.T) {
	certificate, roots := testCertificate(t, []string{"cache.valkey.test"})
	server := startTLSServer(t, certificate, "secret", false)
	prober := ValkeyGoProber{
		RootCAs:       roots,
		DialContextFn: dialTLSAddress(server.address),
		Timeout:       50 * time.Millisecond,
	}

	result := prober.Probe(context.Background(), testTarget("cache.valkey.test", "secret"))

	if result.Status != StatusFail || result.Code != "timeout" {
		t.Errorf("неверная ошибка таймаута: %+v", result)
	}
}

func Test_CheckValkeyHealth_WhenReadFails_PreservesPrimarySuccess(t *testing.T) {
	service := NewValkeyService(testBaseDomain, testValkeyPort, fixedClock{now: time.Now()}, targetProberStub{
		probe: func(_ context.Context, target Target) ValkeyTargetCheck {
			if strings.Contains(target.host, "-ro.") {
				return ValkeyTargetCheck{Status: StatusFail, Code: "connect_failed"}
			}

			return ValkeyTargetCheck{Status: StatusOK}
		},
	})

	report, err := service.Check(
		context.Background(),
		"rediss://app:secret@cache.valkey.test:41379",
		"rediss://app:secret@cache-ro.valkey.test:41379",
	)
	if err != nil {
		t.Fatalf("проверить адреса: %v", err)
	}

	if report.Status != StatusFail || report.Checks.Primary.Status != StatusOK || report.Checks.Read == nil ||
		report.Checks.Read.Code != "connect_failed" {
		t.Errorf("неверный частичный результат: %+v", report)
	}
}

type tlsServer struct {
	address  string
	closed   <-chan struct{}
	commands <-chan [][]string
}

func startTLSServer(t *testing.T, certificate tls.Certificate, password string, reply bool) tlsServer {
	t.Helper()

	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("запустить TLS-сервер: %v", err)
	}
	closed := make(chan struct{})
	commands := make(chan [][]string, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			commands <- nil
			close(commands)
			close(closed)

			return
		}
		received := make([][]string, 0, 3)
		defer func() {
			_ = connection.Close()
			commands <- received
			close(commands)
			close(closed)
		}()
		reader := bufio.NewReader(connection)
		writer := bufio.NewWriter(connection)

		for {
			command, readErr := readRESPCommand(reader)
			if readErr != nil {
				return
			}
			received = append(received, command)
			if !reply {
				time.Sleep(200 * time.Millisecond)

				return
			}
			switch strings.ToUpper(command[0]) {
			case "AUTH":
				if len(command) == 3 && command[1] == "app" && command[2] == password {
					_, _ = writer.WriteString("+OK\r\n")
				} else {
					_, _ = writer.WriteString("-WRONGPASS invalid credentials\r\n")
				}
			case "HELLO":
				_, _ = writer.WriteString("*2\r\n$5\r\nproto\r\n:2\r\n")
			case "PING":
				_, _ = writer.WriteString("+PONG\r\n")
			}
			if flushErr := writer.Flush(); flushErr != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
	})

	return tlsServer{address: listener.Addr().String(), closed: closed, commands: commands}
}

func readRESPCommand(reader *bufio.Reader) ([]string, error) {
	header, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if len(header) < 3 || header[0] != '*' {
		return nil, fmt.Errorf("неверный заголовок RESP")
	}
	count, err := strconv.Atoi(strings.TrimSpace(header[1:]))
	if err != nil {
		return nil, err
	}

	command := make([]string, 0, count)
	for range count {
		lengthLine, lengthErr := reader.ReadString('\n')
		if lengthErr != nil {
			return nil, lengthErr
		}
		if len(lengthLine) < 3 || lengthLine[0] != '$' {
			return nil, fmt.Errorf("неверная длина RESP")
		}
		length, parseErr := strconv.Atoi(strings.TrimSpace(lengthLine[1:]))
		if parseErr != nil {
			return nil, parseErr
		}
		value := make([]byte, length+2)
		if _, readErr := io.ReadFull(reader, value); readErr != nil {
			return nil, readErr
		}
		command = append(command, string(value[:length]))
	}

	return command, nil
}

func testCertificate(t *testing.T, dnsNames []string) (tls.Certificate, *x509.CertPool) {
	t.Helper()

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("создать закрытый ключ: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: dnsNames[0]},
		DNSNames:              dnsNames,
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatalf("создать сертификат: %v", err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	privateKeyPEM := pem.EncodeToMemory(
		&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)},
	)
	certificate, err := tls.X509KeyPair(certificatePEM, privateKeyPEM)
	if err != nil {
		t.Fatalf("разобрать сертификат: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("разобрать корневой сертификат: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)

	return certificate, roots
}

func dialTLSAddress(address string) DialContextFunc {
	return func(ctx context.Context, _ string, dialer *net.Dialer, tlsConfig *tls.Config) (net.Conn, error) {
		connection, err := dialer.DialContext(ctx, "tcp", address)
		if err != nil {
			return nil, err
		}

		return tls.Client(connection, tlsConfig), nil
	}
}

func testTarget(host, password string) Target {
	return Target{address: net.JoinHostPort(host, strconv.Itoa(testValkeyPort)), host: host, password: password}
}

func waitTargetStarted(t *testing.T, started <-chan string) {
	t.Helper()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("проверка адреса не началась")
	}
}

func waitConnectionClosed(t *testing.T, closed <-chan struct{}) {
	t.Helper()

	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("клиент не закрыл соединение")
	}
}
