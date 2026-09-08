package api_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/RostislavDugin/managed-valkey/api/internal/api"
	"github.com/RostislavDugin/managed-valkey/internal/logging"
)

func TestServeFinishesStartedRequestAndStopsAccepting(t *testing.T) {
	router := gin.New()

	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})

	router.GET("/slow", func(c *gin.Context) {
		close(requestStarted)
		<-releaseRequest
		c.String(http.StatusOK, "готово")
	})

	logs := &bytes.Buffer{}
	logger, _ := logging.NewWithWriter(logs, logging.Config{ServiceName: "api", Environment: logging.EnvironmentProd})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("занять порт: %v", err)
	}

	address := listener.Addr().String()
	server := api.NewServer(address, router, logger, 10*time.Second)

	ctx, cancel := context.WithCancel(context.Background())

	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(ctx, listener) }()

	responseReceived := make(chan *http.Response, 1)
	requestFailed := make(chan error, 1)

	go func() {
		response, err := http.Get("http://" + address + "/slow")
		if err != nil {
			requestFailed <- err

			return
		}

		responseReceived <- response
	}()

	select {
	case <-requestStarted:
	case err := <-requestFailed:
		t.Fatalf("запрос не дошёл до обработчика: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("обработчик не получил запрос")
	}

	cancel()

	waitUntilRefused(t, address)

	close(releaseRequest)

	select {
	case response := <-responseReceived:
		defer func() { _ = response.Body.Close() }()

		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatalf("прочитать ответ: %v", err)
		}

		if response.StatusCode != http.StatusOK || string(body) != "готово" {
			t.Errorf("ответ %d %q, ожидался 200 \"готово\"", response.StatusCode, body)
		}
	case err := <-requestFailed:
		t.Fatalf("начатый запрос оборван: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("начатый запрос не завершился")
	}

	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("остановка сервера: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("сервер не остановился")
	}
}

// Закрытие listener асинхронно относительно отмены контекста.
func waitUntilRefused(t *testing.T, address string) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
		if err != nil {
			return
		}

		_ = conn.Close()
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatal("сервер продолжает принимать новые соединения после отмены контекста")
}
