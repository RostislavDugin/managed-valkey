//go:build integration

package integrations

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
)

var errValkeyCommandRejected = errors.New("Valkey отклонил команду")

type kubernetesReader struct {
	reader client.Reader
}

func newKubernetesReader(kubeconfig string) (*kubernetesReader, error) {
	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, fmt.Errorf("загрузить административный kubeconfig: %w", err)
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("добавить core/v1 в схему: %w", err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("добавить apps/v1 в схему: %w", err)
	}
	if err := valkeyv1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("добавить ValkeyInstance в схему: %w", err)
	}
	reader, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("создать Kubernetes-клиент: %w", err)
	}

	return &kubernetesReader{reader: reader}, nil
}

func (reader *kubernetesReader) Namespace(ctx context.Context, name string) (*corev1.Namespace, error) {
	namespace := &corev1.Namespace{}
	if err := reader.reader.Get(ctx, client.ObjectKey{Name: name}, namespace); err != nil {
		return nil, err
	}

	return namespace, nil
}

func (reader *kubernetesReader) Instance(
	ctx context.Context,
	namespace string,
	slug string,
) (*valkeyv1alpha1.ValkeyInstance, error) {
	resource := &valkeyv1alpha1.ValkeyInstance{}
	if err := reader.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: slug}, resource); err != nil {
		return nil, err
	}

	return resource, nil
}

func (reader *kubernetesReader) Pod(
	ctx context.Context,
	namespace string,
	slug string,
) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	if err := reader.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: slug + "-0"}, pod); err != nil {
		return nil, err
	}

	return pod, nil
}

type valkeyConnection struct {
	connection net.Conn
	reader     *bufio.Reader
}

func openValkeyConnection(
	ctx context.Context,
	address string,
	hostname string,
	caFile string,
	password string,
) (*valkeyConnection, error) {
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("прочитать CA Valkey: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("CA Valkey не содержит сертификат")
	}
	dialer := &tls.Dialer{Config: &tls.Config{
		MinVersion: tls.VersionTLS12,
		RootCAs:    roots,
		ServerName: hostname,
	}}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("подключиться к Valkey по TLS: %w", err)
	}
	result := &valkeyConnection{connection: connection, reader: bufio.NewReader(connection)}
	if _, err := result.command(ctx, "AUTH", "app", password); err != nil {
		_ = connection.Close()

		return nil, err
	}

	return result, nil
}

func (connection *valkeyConnection) Close() error {
	return connection.connection.Close()
}

func (connection *valkeyConnection) Ping(ctx context.Context) error {
	value, err := connection.command(ctx, "PING")
	if err != nil {
		return err
	}
	if value != "PONG" {
		return fmt.Errorf("PING вернул %q", value)
	}

	return nil
}

func (connection *valkeyConnection) Set(ctx context.Context, key, value string) error {
	response, err := connection.command(ctx, "SET", key, value)
	if err != nil {
		return err
	}
	if response != "OK" {
		return fmt.Errorf("SET вернул %q", response)
	}

	return nil
}

func (connection *valkeyConnection) Get(ctx context.Context, key string) (string, error) {
	return connection.command(ctx, "GET", key)
}

func (connection *valkeyConnection) command(ctx context.Context, values ...string) (string, error) {
	deadline := time.Now().Add(10 * time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := connection.connection.SetDeadline(deadline); err != nil {
		return "", fmt.Errorf("задать срок команды Valkey: %w", err)
	}
	var request strings.Builder
	_, _ = fmt.Fprintf(&request, "*%d\r\n", len(values))
	for _, value := range values {
		_, _ = fmt.Fprintf(&request, "$%d\r\n%s\r\n", len(value), value)
	}
	if _, err := io.WriteString(connection.connection, request.String()); err != nil {
		return "", fmt.Errorf("отправить команду Valkey: %w", err)
	}

	return connection.readResponse()
}

func (connection *valkeyConnection) readResponse() (string, error) {
	line, err := connection.reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("прочитать ответ Valkey: %w", err)
	}
	if len(line) < 3 || !strings.HasSuffix(line, "\r\n") {
		return "", errors.New("Valkey вернул повреждённый ответ")
	}
	payload := strings.TrimSuffix(line[1:], "\r\n")
	switch line[0] {
	case '+', ':':
		return payload, nil
	case '-':
		return "", errValkeyCommandRejected
	case '$':
		length, parseErr := strconv.Atoi(payload)
		if parseErr != nil || length < 0 {
			return "", errors.New("Valkey вернул неверную длину ответа")
		}
		data := make([]byte, length+2)
		if _, err := io.ReadFull(connection.reader, data); err != nil {
			return "", fmt.Errorf("прочитать значение Valkey: %w", err)
		}
		if string(data[length:]) != "\r\n" {
			return "", errors.New("Valkey вернул повреждённое значение")
		}

		return string(data[:length]), nil
	default:
		return "", errors.New("Valkey вернул неподдерживаемый ответ")
	}
}
