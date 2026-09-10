package logging

import (
	"bytes"
	"context"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func Test_ConfigureOtlpExport_WithInvalidTransportOrCredentials_ReportsErrorWithoutLeakingPassword(t *testing.T) {
	passwordFile := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(passwordFile, []byte("private-test-password\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		url      string
		username string
		file     string
	}{
		{"адрес HTTP отключает экспорт и не раскрывает пароль", "http://localhost/logs", "operator", passwordFile},
		{"отсутствующее имя пользователя отключает экспорт и не раскрывает пароль", "https://localhost/logs", "", passwordFile},
		{"отсутствующий файл пароля отключает экспорт и не раскрывает пароль", "https://localhost/logs", "operator", ""},
		{"недоступный файл пароля отключает экспорт и не раскрывает пароль", "https://localhost/logs", "operator", passwordFile + ".missing"},
		{"учётные данные в URL отключают экспорт и не раскрывают пароль", "https://operator:private-test-password@localhost/logs", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			logger, shutdown := NewWithWriter(&out, Config{
				OTLPURL:          tc.url,
				OTLPUsername:     tc.username,
				OTLPPasswordFile: tc.file,
			})
			logger.Info("stdout работает")
			if err := shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "не удалось настроить экспорт логов") ||
				!strings.Contains(out.String(), "stdout работает") {
				t.Fatal("ошибка экспорта или запись stdout потеряна")
			}
			if strings.Contains(out.String(), "private-test-password") {
				t.Fatal("пароль попал в stdout")
			}
		})
	}
}

func Test_ExportLogs_WithAuthenticatedHttps_SendsOneAuthorizedRequestWithoutRedirect(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusTemporaryRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var received, leaked atomic.Int32
			downgrade := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				leaked.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer downgrade.Close()
			receiver := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				username, password, ok := r.BasicAuth()
				if !ok || username != "valkey-operator" || password != "private-test-password" ||
					r.URL.Path != "/insert/opentelemetry/v1/logs" {
					t.Error("неверная авторизация или путь OTLP")
				}
				_, _ = io.Copy(io.Discard, r.Body)
				received.Add(1)
				w.Header().Set("Location", downgrade.URL)
				w.WriteHeader(status)
			}))
			defer receiver.Close()

			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.TLSClientConfig = receiver.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
			transport.TLSClientConfig.InsecureSkipVerify = false
			transport.TLSClientConfig.RootCAs = x509.NewCertPool()
			transport.TLSClientConfig.RootCAs.AddCert(receiver.Certificate())
			original := http.DefaultTransport
			http.DefaultTransport = transport
			t.Cleanup(func() {
				http.DefaultTransport = original
				transport.CloseIdleConnections()
			})

			passwordFile := filepath.Join(t.TempDir(), "password")
			if err := os.WriteFile(passwordFile, []byte("private-test-password\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			logger, shutdown := NewWithWriter(&out, Config{
				OTLPURL:          receiver.URL + "/insert/opentelemetry/v1/logs",
				OTLPUsername:     "valkey-operator",
				OTLPPasswordFile: passwordFile,
			})
			logger.Info("запись")
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := shutdown(ctx)
			if status == http.StatusOK && err != nil {
				t.Fatal(err)
			}
			if received.Load() != 1 || leaked.Load() != 0 {
				t.Fatalf("получено запросов: TLS=%d, HTTP=%d", received.Load(), leaked.Load())
			}
			if strings.Contains(out.String(), "private-test-password") {
				t.Fatal("пароль попал в stdout")
			}
		})
	}
}

func Test_ExportLogs_WithUntrustedCertificate_DoesNotSendRequest(t *testing.T) {
	var received atomic.Int32
	receiver := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()

	passwordFile := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(passwordFile, []byte("private-test-password"), 0o600); err != nil {
		t.Fatal(err)
	}
	logger, shutdown := NewWithWriter(io.Discard, Config{
		OTLPURL:          receiver.URL + "/insert/opentelemetry/v1/logs",
		OTLPUsername:     "valkey-operator",
		OTLPPasswordFile: passwordFile,
	})
	logger.Info("запись")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = shutdown(ctx)
	if received.Load() != 0 {
		t.Fatal("сервер с недоверенным сертификатом получил запрос")
	}
}
