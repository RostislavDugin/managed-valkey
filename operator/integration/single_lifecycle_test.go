//go:build integration

package integration_test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/tools/remotecommand"
	transportspdy "k8s.io/client-go/transport/spdy"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/RostislavDugin/managed-valkey/internal/logging"
	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	operatorconfig "github.com/RostislavDugin/managed-valkey/operator/internal/config"
	operatorcontroller "github.com/RostislavDugin/managed-valkey/operator/internal/operator"
)

const (
	systemNamespace = "valkey-system"
	instanceLabel   = "valkey.h3llo-demo.com/instance"
	userIDLabel     = "valkey.h3llo-demo.com/user-id"
)

func TestSingleLifecycle(t *testing.T) {
	h := newHarness(t)
	h.startOperator(t)
	t.Cleanup(func() { h.close(t) })

	first := h.createSingle(t, "fulla1", valkeyv1alpha1.WhitelistSpec{})
	h.waitFor(t, first, func(instance *valkeyv1alpha1.ValkeyInstance) bool {
		return instance.Status.CredentialsInitialized && !instance.Status.Initialized
	}, "подготовки учётных данных до публичной готовности")
	h.stopOperator(t)
	notReady := h.getInstance(t, first)
	if notReady.Status.Initialized || notReady.Status.Phase == valkeyv1alpha1.InstancePhaseRunning {
		t.Fatalf("остановленный во время создания оператор подтвердил готовность: %+v", notReady.Status)
	}
	if output, err := h.dockerCLI(t, first, h.clientPreReady, first.password, "PING"); err == nil &&
		strings.Contains(output, "PONG") {
		t.Fatal("app доступен до завершения первоначальных проверок")
	}
	h.startOperator(t)
	firstStatus := h.waitRunning(t, first)
	assertAppliedStatus(t, firstStatus)
	servicePasswords := h.servicePasswords(t, first)

	firstClient := openPersistentConnection(t, h.publicAddr, first, h.caFile, false, func() {})
	defer closeConnections([]*persistentConnection{firstClient})
	setValue(t, firstClient, "instance-key", "first")
	if value := getValue(t, firstClient, "instance-key"); value != "first" {
		t.Fatalf("SET/GET вернул %q", value)
	}
	writeRESP(t, firstClient, "CONFIG", "GET", "maxmemory")
	if _, err := readRESPResult(t, firstClient); err == nil {
		t.Fatal("app получил запрещённую административную команду CONFIG GET")
	}
	assertAuthenticationRejected(t, h, first, "wrong-password")
	assertWrongSNIRejected(t, h, first)

	connections := h.openEnvoyConnections(t, first)
	defer closeConnections(connections)
	pingConnections(t, connections)

	second := h.createSingle(t, "fullb2", valkeyv1alpha1.WhitelistSpec{
		IsEnabled: true,
		CIDRs:     []string{h.clientAllowed + "/32"},
	})
	h.waitRunning(t, second)
	pingConnections(t, connections)
	if output, err := h.dockerCLI(t, first, h.clientBlocked, first.password, "PING"); err != nil ||
		!strings.Contains(output, "PONG") {
		t.Fatalf("выключенный whitelist не пропустил второго клиента: output=%q error=%v", output, err)
	}
	if output, err := h.dockerCLI(t, second, h.clientAllowed, second.password, "PING"); err != nil ||
		!strings.Contains(output, "PONG") {
		t.Fatalf("разрешённый CIDR не пропустил клиента: output=%q error=%v", output, err)
	}
	if output, err := h.dockerCLI(
		t,
		second,
		h.clientBlocked,
		second.password,
		"PING",
	); err == nil &&
		strings.Contains(output, "PONG") {
		t.Fatal("whitelist пропустил запрещённый адрес")
	}
	if output, err := h.dockerCLI(
		t,
		second,
		h.clientAllowed,
		first.password,
		"PING",
	); err == nil &&
		strings.Contains(output, "PONG") {
		t.Fatal("пароль первого инстанса подошёл ко второму")
	}
	assertPodIsolation(t, h, first, second)

	h.deleteInstance(t, second)
	pingConnections(t, connections)
	if value := getValue(t, firstClient, "instance-key"); value != "first" {
		t.Fatalf("удаление соседа изменило данные первого инстанса: %q", value)
	}
	closeConnections(connections)
	connections = nil

	third := h.createSingle(t, "fullc3", valkeyv1alpha1.WhitelistSpec{IsEnabled: true})
	h.waitRunning(t, third)
	if output, err := h.dockerCLI(
		t,
		third,
		h.clientAllowed,
		third.password,
		"PING",
	); err == nil &&
		strings.Contains(output, "PONG") {
		t.Fatal("пустой включённый whitelist пропустил клиента")
	}
	h.deleteInstance(t, third)

	oldPod := h.getPod(t, first)
	h.stopOperator(t)
	_, _ = execInPod(t, h.adminREST, oldPod.Namespace, oldPod.Name, []string{
		"/bin/sh",
		"-c",
		"export REDISCLI_AUTH=\"$(cat /etc/valkey-auth/operator-password)\"; valkey-cli --user operator --no-auth-warning shutdown nosave",
	})
	h.startOperator(t)
	newPod := h.waitForReplacement(t, first, oldPod.UID)
	h.waitRunning(t, first)
	if newPod.UID == oldPod.UID {
		t.Fatal("завершённый процесс не заменён")
	}
	replacementClient := openPersistentConnection(t, h.publicAddr, first, h.caFile, false, func() {})
	if replacementPasswords := h.servicePasswords(t, first); replacementPasswords != servicePasswords {
		closeConnections([]*persistentConnection{replacementClient})
		t.Fatal("служебные пароли изменились при восстановлении single")
	}
	writeRESP(t, replacementClient, "GET", "instance-key")
	if value := readRESP(t, replacementClient); value != nil {
		closeConnections([]*persistentConnection{replacementClient})
		t.Fatalf("замена single сохранила старый кэш: %#v", value)
	}
	closeConnections([]*persistentConnection{replacementClient})

	h.requestDeletion(t, first)
	h.waitFor(t, first, func(instance *valkeyv1alpha1.ValkeyInstance) bool {
		return instance.Status.Deletion != nil &&
			(instance.Status.Deletion.Stage == valkeyv1alpha1.DeletionStageStopping ||
				instance.Status.Deletion.Stage == valkeyv1alpha1.DeletionStageVerifying)
	}, "стадии остановки удаления")
	h.stopOperator(t)
	if instance := h.getInstance(t, first); instance.DeletionTimestamp.IsZero() {
		t.Fatal("CR исчез до подтверждения остановки")
	}
	h.startOperator(t)
	h.waitDeleted(t, first)
}

type harness struct {
	adminREST      *rest.Config
	operatorREST   *rest.Config
	k8s            client.Client
	clientset      kubernetes.Interface
	publicAddr     string
	baseDomain     string
	dockerNet      string
	dockerHost     string
	dockerPort     string
	clientAllowed  string
	clientBlocked  string
	clientWrongSNI string
	clientPreReady string
	operatorCIDRs  []string
	caFile         string

	mu       sync.Mutex
	operator *operatorProcess
	created  []*testInstance
}

type operatorProcess struct {
	cancel context.CancelFunc
	done   chan error
}

type testInstance struct {
	slug      string
	namespace string
	password  string
	hostname  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	adminREST := loadKubeconfig(t, requiredEnv(t, "MANAGED_VALKEY_ADMIN_KUBECONFIG"))
	operatorREST := loadKubeconfig(t, requiredEnv(t, "MANAGED_VALKEY_OPERATOR_KUBECONFIG"))
	k8s, err := client.New(adminREST, client.Options{Scheme: operatorcontroller.NewScheme()})
	if err != nil {
		t.Fatalf("создать административный клиент Kubernetes: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(adminREST)
	if err != nil {
		t.Fatalf("создать clientset Kubernetes: %v", err)
	}
	h := &harness{
		adminREST: adminREST, operatorREST: operatorREST, k8s: k8s, clientset: clientset,
		publicAddr:     envOrDefault("MANAGED_VALKEY_PUBLIC_ADDRESS", "127.0.0.1:41379"),
		baseDomain:     envOrDefault("VALKEY_BASE_DOMAIN", operatorconfig.DefaultBaseDomain),
		dockerNet:      envOrDefault("MANAGED_VALKEY_DOCKER_NETWORK", "managed-valkey-dev_default"),
		dockerHost:     envOrDefault("MANAGED_VALKEY_DOCKER_HOST", "k3s-server"),
		dockerPort:     envOrDefault("MANAGED_VALKEY_DOCKER_PORT", "31379"),
		clientAllowed:  envOrDefault("MANAGED_VALKEY_CLIENT_ALLOWED", "172.27.0.101"),
		clientBlocked:  envOrDefault("MANAGED_VALKEY_CLIENT_BLOCKED", "172.27.0.102"),
		clientWrongSNI: envOrDefault("MANAGED_VALKEY_CLIENT_WRONG_SNI", "172.27.0.103"),
		clientPreReady: envOrDefault("MANAGED_VALKEY_CLIENT_PRE_READY", "172.27.0.104"),
	}
	dockerCIDR := dockerGatewayCIDR(t, h.dockerNet)
	processCIDR := podRouteSourceCIDR(t, h.k8s)
	h.operatorCIDRs = []string{processCIDR}
	if dockerCIDR != processCIDR {
		h.operatorCIDRs = append(h.operatorCIDRs, dockerCIDR)
	}
	h.caFile = h.writeCAFile(t)

	return h
}

func (h *harness) startOperator(t *testing.T) {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.operator != nil {
		t.Fatal("оператор уже запущен")
	}
	mgr, err := operatorcontroller.NewManager(
		h.operatorREST,
		operatorconfig.Config{
			SystemNamespace: systemNamespace,
			ValkeyImage:     operatorconfig.DefaultValkeyImage,
			BaseDomain:      h.baseDomain,
			OperatorCIDRs:   h.operatorCIDRs,
			Logging: logging.Config{
				ServiceName: operatorconfig.ServiceName, Environment: logging.EnvironmentDev,
			},
		},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		operatorcontroller.WithSkipControllerNameValidation(),
		operatorcontroller.WithProbeAddr("127.0.0.1:0"),
		operatorcontroller.WithLeaderElection(false),
	)
	if err != nil {
		t.Fatalf("создать manager оператора: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- operatorcontroller.Run(ctx, mgr) }()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		cancel()
		t.Fatal("кэш manager не синхронизировался")
	}
	select {
	case err := <-done:
		cancel()
		t.Fatalf("manager завершился при запуске: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	h.operator = &operatorProcess{cancel: cancel, done: done}
}

func (h *harness) stopOperator(t *testing.T) {
	t.Helper()
	h.mu.Lock()
	process := h.operator
	h.operator = nil
	h.mu.Unlock()
	if process == nil {
		return
	}
	process.cancel()
	select {
	case err := <-process.done:
		if err != nil {
			t.Fatalf("остановить оператор: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("оператор не остановился за 30 секунд")
	}
}

func (h *harness) close(t *testing.T) {
	h.mu.Lock()
	running := h.operator != nil
	h.mu.Unlock()
	if !running {
		h.startOperator(t)
	}
	for index := len(h.created) - 1; index >= 0; index-- {
		instance := h.created[index]
		_ = h.k8s.Delete(context.Background(), &valkeyv1alpha1.ValkeyInstance{
			ObjectMeta: metav1.ObjectMeta{Name: instance.slug, Namespace: instance.namespace},
		})
		_ = wait.PollUntilContextTimeout(
			context.Background(), time.Second, 2*time.Minute, true,
			func(ctx context.Context) (bool, error) {
				current := &valkeyv1alpha1.ValkeyInstance{}
				err := h.k8s.Get(ctx, client.ObjectKey{Name: instance.slug, Namespace: instance.namespace}, current)
				return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
			},
		)
		_ = h.k8s.Delete(
			context.Background(),
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instance.namespace}},
		)
	}
	h.stopOperator(t)
}

func (h *harness) createSingle(
	t *testing.T,
	suffix string,
	whitelist valkeyv1alpha1.WhitelistSpec,
) *testInstance {
	t.Helper()
	stamp := strings.ToLower(strconv.FormatInt(time.Now().UnixNano(), 36))
	if len(stamp) > 6 {
		stamp = stamp[len(stamp)-6:]
	}
	slug := suffix + "-" + stamp
	if len(suffix) < 3 || len(suffix) > 20 {
		t.Fatalf("неверный префикс slug %q", suffix)
	}
	namespace := "valkey-" + slug
	instanceID := mustUUIDv7(t)
	userID := mustUUIDv7(t)
	password := "test-" + mustUUIDv7(t)
	digest := sha256.Sum256([]byte(password))
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: valkeyv1alpha1.AuthSecretName(slug), Namespace: namespace},
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			valkeyv1alpha1.AppPasswordHashKeyPrefix + "1": []byte(hex.EncodeToString(digest[:])),
		},
	}
	namespaceObject := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: namespace,
		Labels: map[string]string{
			instanceLabel: slug,
			userIDLabel:   userID,
		},
	}}
	if err := h.k8s.Create(t.Context(), namespaceObject); err != nil {
		t.Fatalf("создать namespace %s: %v", namespace, err)
	}
	if err := h.k8s.Create(t.Context(), secret); err != nil {
		t.Fatalf("создать Secret %s: %v", slug, err)
	}
	resource := &valkeyv1alpha1.ValkeyInstance{
		ObjectMeta: metav1.ObjectMeta{Name: slug, Namespace: namespace},
		Spec: valkeyv1alpha1.ValkeyInstanceSpec{
			InstanceID: instanceID, Slug: slug, Mode: valkeyv1alpha1.ValkeyModeSingle,
			VCPU: 1, RAMGB: 1, PublicPort: 41379, Whitelist: &whitelist,
			PasswordVersion: 1, DesiredGeneration: 1,
		},
	}
	if err := h.k8s.Create(t.Context(), resource); err != nil {
		t.Fatalf("создать ValkeyInstance %s: %v", slug, err)
	}
	result := &testInstance{
		slug: slug, namespace: namespace, password: password,
		hostname: slug + "." + h.baseDomain,
	}
	h.created = append(h.created, result)

	return result
}

func (h *harness) waitRunning(t *testing.T, instance *testInstance) *valkeyv1alpha1.ValkeyInstance {
	t.Helper()
	return h.waitFor(t, instance, func(current *valkeyv1alpha1.ValkeyInstance) bool {
		return current.Status.Initialized && current.Status.Phase == valkeyv1alpha1.InstancePhaseRunning &&
			current.Status.Network != nil &&
			current.Status.Network.VerificationStatus == valkeyv1alpha1.NetworkVerificationVerified &&
			current.Status.Network.DesiredFingerprint != "" &&
			current.Status.Network.DesiredFingerprint == current.Status.Network.VerifiedFingerprint
	}, "фазы running")
}

func (h *harness) servicePasswords(t *testing.T, instance *testInstance) [3]string {
	t.Helper()

	secret := &corev1.Secret{}
	if err := h.k8s.Get(t.Context(), client.ObjectKey{
		Name: valkeyv1alpha1.AuthSecretName(instance.slug), Namespace: instance.namespace,
	}, secret); err != nil {
		t.Fatalf("прочитать служебные пароли %s: %v", instance.slug, err)
	}
	passwords := [3]string{
		string(secret.Data[valkeyv1alpha1.OperatorPasswordKey]),
		string(secret.Data[valkeyv1alpha1.ReplicaPasswordKey]),
		string(secret.Data[valkeyv1alpha1.HealthPasswordKey]),
	}
	for _, password := range passwords {
		if password == "" {
			t.Fatalf("Secret %s не содержит полный комплект служебных паролей", instance.slug)
		}
	}

	return passwords
}

func (h *harness) waitFor(
	t *testing.T,
	instance *testInstance,
	condition func(*valkeyv1alpha1.ValkeyInstance) bool,
	description string,
) *valkeyv1alpha1.ValkeyInstance {
	return h.waitForWithin(t, instance, 5*time.Minute, condition, description)
}

func (h *harness) waitForWithin(
	t *testing.T,
	instance *testInstance,
	timeout time.Duration,
	condition func(*valkeyv1alpha1.ValkeyInstance) bool,
	description string,
) *valkeyv1alpha1.ValkeyInstance {
	t.Helper()
	var current valkeyv1alpha1.ValkeyInstance
	err := wait.PollUntilContextTimeout(
		t.Context(), 500*time.Millisecond, timeout, true,
		func(ctx context.Context) (bool, error) {
			err := h.k8s.Get(ctx, client.ObjectKey{Name: instance.slug, Namespace: instance.namespace}, &current)
			if err != nil {
				return false, err
			}
			return condition(&current), nil
		},
	)
	if err != nil {
		t.Fatalf("дождаться %s для %s: %v; status=%+v", description, instance.slug, err, current.Status)
	}

	return current.DeepCopy()
}

func (h *harness) getInstance(t *testing.T, instance *testInstance) *valkeyv1alpha1.ValkeyInstance {
	t.Helper()
	current := &valkeyv1alpha1.ValkeyInstance{}
	if err := h.k8s.Get(
		t.Context(),
		client.ObjectKey{Name: instance.slug, Namespace: instance.namespace},
		current,
	); err != nil {
		t.Fatalf("прочитать ValkeyInstance %s: %v", instance.slug, err)
	}
	return current
}

func (h *harness) getPod(t *testing.T, instance *testInstance) *corev1.Pod {
	t.Helper()
	pod := &corev1.Pod{}
	key := client.ObjectKey{Name: instance.slug + "-0", Namespace: instance.namespace}
	if err := h.k8s.Get(t.Context(), key, pod); err != nil {
		t.Fatalf("прочитать Pod %s: %v", instance.slug, err)
	}
	return pod
}

func (h *harness) waitForReplacement(
	t *testing.T,
	instance *testInstance,
	oldUID types.UID,
) *corev1.Pod {
	t.Helper()
	var pod corev1.Pod
	err := wait.PollUntilContextTimeout(
		t.Context(),
		time.Second,
		5*time.Minute,
		true,
		func(ctx context.Context) (bool, error) {
			err := h.k8s.Get(ctx, client.ObjectKey{Name: instance.slug + "-0", Namespace: instance.namespace}, &pod)
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			return pod.UID != oldUID && pod.Status.PodIP != "", nil
		},
	)
	if err != nil {
		t.Fatalf("дождаться замены Pod %s: %v", instance.slug, err)
	}
	return pod.DeepCopy()
}

func (h *harness) requestDeletion(t *testing.T, instance *testInstance) {
	t.Helper()
	resource := &valkeyv1alpha1.ValkeyInstance{ObjectMeta: metav1.ObjectMeta{
		Name: instance.slug, Namespace: instance.namespace,
	}}
	if err := h.k8s.Delete(t.Context(), resource); err != nil && !apierrors.IsNotFound(err) {
		t.Fatalf("запросить удаление %s: %v", instance.slug, err)
	}
}

func (h *harness) deleteInstance(t *testing.T, instance *testInstance) {
	t.Helper()
	h.requestDeletion(t, instance)
	h.waitDeleted(t, instance)
}

func (h *harness) waitDeleted(t *testing.T, instance *testInstance) {
	t.Helper()
	err := wait.PollUntilContextTimeout(
		t.Context(),
		time.Second,
		5*time.Minute,
		true,
		func(ctx context.Context) (bool, error) {
			current := &valkeyv1alpha1.ValkeyInstance{}
			err := h.k8s.Get(ctx, client.ObjectKey{Name: instance.slug, Namespace: instance.namespace}, current)
			return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
		},
	)
	if err != nil {
		t.Fatalf("дождаться удаления %s: %v", instance.slug, err)
	}
	if err := h.k8s.Delete(
		t.Context(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: instance.namespace}},
	); err != nil &&
		!apierrors.IsNotFound(err) {
		t.Fatalf("удалить namespace %s: %v", instance.namespace, err)
	}
	err = wait.PollUntilContextTimeout(
		t.Context(),
		time.Second,
		2*time.Minute,
		true,
		func(ctx context.Context) (bool, error) {
			namespace := &corev1.Namespace{}
			err := h.k8s.Get(ctx, client.ObjectKey{Name: instance.namespace}, namespace)
			return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
		},
	)
	if err != nil {
		t.Fatalf("дождаться удаления namespace %s: %v", instance.namespace, err)
	}
}

func (h *harness) writeCAFile(t *testing.T) string {
	t.Helper()
	secret := &corev1.Secret{}
	if err := h.k8s.Get(t.Context(), client.ObjectKey{
		Namespace: systemNamespace, Name: "valkey-wildcard-tls",
	}, secret); err != nil {
		t.Fatalf("прочитать TLS Secret стенда: %v", err)
	}
	certificate := secret.Data[corev1.TLSCertKey]
	if len(certificate) == 0 {
		t.Fatal("TLS Secret стенда не содержит tls.crt")
	}
	caSource := strings.TrimSpace(os.Getenv("MANAGED_VALKEY_CA_FILE"))
	if caSource == "" {
		output, err := exec.CommandContext(t.Context(), "mkcert", "-CAROOT").Output()
		if err != nil {
			t.Fatalf("получить каталог CA mkcert: %v", err)
		}
		caSource = filepath.Join(strings.TrimSpace(string(output)), "rootCA.pem")
	}
	caPEM, err := os.ReadFile(caSource)
	if err != nil {
		t.Fatalf("прочитать корневой сертификат %s: %v", caSource, err)
	}
	path := t.TempDir() + "/ca.crt"
	if err := os.WriteFile(path, caPEM, 0o600); err != nil {
		t.Fatalf("сохранить корневой сертификат стенда: %v", err)
	}
	return path
}

func (h *harness) dockerCLI(
	t *testing.T,
	instance *testInstance,
	address string,
	password string,
	command ...string,
) (string, error) {
	return h.dockerCLIWithTimeout(t, 12*time.Second, instance, address, password, command...)
}

func (h *harness) dockerCLIWithTimeout(
	t *testing.T,
	timeout time.Duration,
	instance *testInstance,
	address string,
	password string,
	command ...string,
) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	containerName := "managed-valkey-test-" + uuid.NewString()
	defer func() {
		_ = exec.Command("docker", "rm", "-f", containerName).Run()
	}()
	arguments := []string{
		"run", "--rm", "--name", containerName,
		"--label", "com.h3llo-demo.managed-valkey.test-network=" + h.dockerNet,
		"--network", h.dockerNet, "--ip", address,
		"-e", "REDISCLI_AUTH",
		"-v", h.caFile + ":/ca.crt:ro", operatorconfig.DefaultValkeyImage,
		"valkey-cli", "--tls", "--cacert", "/ca.crt", "--sni", instance.hostname,
		"-h", h.dockerHost, "-p", h.dockerPort, "--user", "app",
		"--no-auth-warning",
	}
	arguments = append(arguments, command...)
	process := exec.CommandContext(ctx, "docker", arguments...)
	process.Env = append(os.Environ(), "REDISCLI_AUTH="+password)
	output, err := process.CombinedOutput()
	if ctx.Err() != nil {
		return string(output), ctx.Err()
	}
	return string(output), err
}

type persistentConnection struct {
	conn net.Conn
	read *bufio.Reader
	pub  bool
	stop func()
}

func (h *harness) openEnvoyConnections(
	t *testing.T,
	instance *testInstance,
) []*persistentConnection {
	t.Helper()
	pods := &corev1.PodList{}
	if err := h.k8s.List(
		t.Context(), pods,
		client.InNamespace("envoy-gateway-system"),
		client.MatchingLabels{
			"gateway.envoyproxy.io/owning-gateway-name":      "valkey",
			"gateway.envoyproxy.io/owning-gateway-namespace": systemNamespace,
		},
	); err != nil {
		t.Fatalf("прочитать Pod Envoy: %v", err)
	}
	if len(pods.Items) != 2 {
		t.Fatalf("на стенде %d процессов Envoy вместо 2", len(pods.Items))
	}
	connections := make([]*persistentConnection, 0, 4)
	for index := range pods.Items {
		address, stop := h.forwardEnvoy(t, &pods.Items[index])
		connections = append(connections,
			openPersistentConnection(t, address, instance, h.caFile, false, stop),
			openPersistentConnection(t, address, instance, h.caFile, true, func() {}),
		)
	}
	return connections
}

func (h *harness) forwardEnvoy(t *testing.T, pod *corev1.Pod) (string, func()) {
	t.Helper()
	requestURL := h.clientset.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(pod.Namespace).Name(pod.Name).SubResource("portforward").URL()
	roundTripper, upgrader, err := transportspdy.RoundTripperFor(h.adminREST)
	if err != nil {
		t.Fatalf("создать транспорт port-forward: %v", err)
	}
	dialer := transportspdy.NewDialer(upgrader, &http.Client{Transport: roundTripper}, http.MethodPost, requestURL)
	stop := make(chan struct{})
	ready := make(chan struct{})
	forwarder, err := portforward.NewOnAddresses(
		dialer, []string{"127.0.0.1"}, []string{"0:41379"}, stop, ready, io.Discard, io.Discard,
	)
	if err != nil {
		t.Fatalf("настроить port-forward Envoy: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- forwarder.ForwardPorts() }()
	select {
	case <-ready:
	case err := <-done:
		close(stop)
		t.Fatalf("запустить port-forward Envoy: %v", err)
	case <-time.After(10 * time.Second):
		close(stop)
		t.Fatal("port-forward Envoy не запустился")
	}
	ports, err := forwarder.GetPorts()
	if err != nil || len(ports) != 1 {
		close(stop)
		t.Fatalf("получить порт Envoy: ports=%v error=%v", ports, err)
	}
	var once sync.Once
	closeForward := func() { once.Do(func() { close(stop) }) }

	return net.JoinHostPort("127.0.0.1", strconv.Itoa(int(ports[0].Local))), closeForward
}

func openPersistentConnection(
	t *testing.T,
	address string,
	instance *testInstance,
	caFile string,
	pubsub bool,
	stop func(),
) *persistentConnection {
	t.Helper()
	result := dialPersistentConnection(t, address, instance.hostname, caFile, stop)
	result.pub = pubsub
	writeRESP(t, result, "AUTH", "app", instance.password)
	if response := readRESP(t, result); response != "OK" {
		closeConnections([]*persistentConnection{result})
		t.Fatalf("AUTH через Envoy вернул %#v", response)
	}
	if pubsub {
		writeRESP(t, result, "SUBSCRIBE", "lifecycle")
		if response := readRESP(t, result); response == nil {
			closeConnections([]*persistentConnection{result})
			t.Fatal("SUBSCRIBE не подтвердил подписку")
		}
	}

	return result
}

func dialPersistentConnection(
	t *testing.T,
	address string,
	hostname string,
	caFile string,
	stop func(),
) *persistentConnection {
	t.Helper()
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatalf("прочитать CA: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("сертификат стенда не добавлен в пул доверия")
	}
	deadline := time.Now().Add(30 * time.Second)
	var conn *tls.Conn
	for {
		dialer := &net.Dialer{Timeout: 3 * time.Second}
		conn, err = tls.DialWithDialer(dialer, "tcp", address, &tls.Config{
			MinVersion: tls.VersionTLS12, ServerName: hostname, RootCAs: roots,
		})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			stop()
			t.Fatalf("подключиться напрямую к Envoy: %v", err)
		}
		select {
		case <-t.Context().Done():
			stop()
			t.Fatalf("подключиться напрямую к Envoy: %v", t.Context().Err())
		case <-time.After(500 * time.Millisecond):
		}
	}

	return &persistentConnection{conn: conn, read: bufio.NewReader(conn), stop: stop}
}

func pingConnections(t *testing.T, connections []*persistentConnection) {
	t.Helper()
	for _, connection := range connections {
		if connection.pub {
			writeRESP(t, connection, "PING", "probe")
			response := fmt.Sprint(readRESP(t, connection))
			if !strings.Contains(strings.ToLower(response), "pong") {
				t.Fatalf("pub/sub PING вернул %s", response)
			}
			continue
		}
		writeRESP(t, connection, "PING")
		if response := readRESP(t, connection); response != "PONG" {
			t.Fatalf("PING по установленному соединению вернул %#v", response)
		}
	}
}

func closeConnections(connections []*persistentConnection) {
	for _, connection := range connections {
		_ = connection.conn.Close()
		connection.stop()
	}
}

func writeRESP(t *testing.T, connection *persistentConnection, values ...string) {
	t.Helper()
	_ = connection.conn.SetDeadline(time.Now().Add(5 * time.Second))
	var request strings.Builder
	fmt.Fprintf(&request, "*%d\r\n", len(values))
	for _, value := range values {
		fmt.Fprintf(&request, "$%d\r\n%s\r\n", len(value), value)
	}
	if _, err := io.WriteString(connection.conn, request.String()); err != nil {
		t.Fatalf("отправить команду по установленному соединению: %v", err)
	}
}

func readRESP(t *testing.T, connection *persistentConnection) any {
	t.Helper()
	value, err := readRESPResult(t, connection)
	if err != nil {
		t.Fatalf("Valkey вернул ошибку: %v", err)
	}
	return value
}

func readRESPResult(t *testing.T, connection *persistentConnection) (any, error) {
	t.Helper()
	line, err := connection.read.ReadString('\n')
	if err != nil {
		t.Fatalf("прочитать ответ по установленному соединению: %v", err)
	}
	line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if line == "" {
		t.Fatal("пустой RESP-ответ")
	}
	switch line[0] {
	case '+':
		return line[1:], nil
	case '-':
		return nil, fmt.Errorf("%s", line[1:])
	case ':':
		value, err := strconv.ParseInt(line[1:], 10, 64)
		if err != nil {
			t.Fatalf("разобрать RESP integer: %v", err)
		}
		return value, nil
	case '$':
		length, err := strconv.Atoi(line[1:])
		if err != nil {
			t.Fatalf("разобрать RESP bulk length: %v", err)
		}
		if length < 0 {
			return nil, nil
		}
		value := make([]byte, length+2)
		if _, err := io.ReadFull(connection.read, value); err != nil {
			t.Fatalf("прочитать RESP bulk: %v", err)
		}
		return string(value[:length]), nil
	case '*':
		length, err := strconv.Atoi(line[1:])
		if err != nil {
			t.Fatalf("разобрать RESP array length: %v", err)
		}
		values := make([]any, length)
		for index := range values {
			values[index], err = readRESPResult(t, connection)
			if err != nil {
				return nil, err
			}
		}
		return values, nil
	default:
		t.Fatalf("неизвестный RESP-ответ %q", line)
	}
	return nil, nil
}

func assertAppliedStatus(t *testing.T, instance *valkeyv1alpha1.ValkeyInstance) {
	t.Helper()
	if instance.Status.Applied == nil || instance.Status.Applied.Mode != valkeyv1alpha1.ValkeyModeSingle ||
		instance.Status.Applied.VCPU != 1 || instance.Status.Applied.RAMGB != 1 ||
		instance.Status.AppliedPasswordVersion != 1 || instance.Status.ObservedGeneration != 1 ||
		instance.Status.PrimaryOrdinal == nil || *instance.Status.PrimaryOrdinal != 0 ||
		instance.Status.PrimaryPodUID == "" || instance.Status.PrimaryContainerID == "" ||
		instance.Status.ObservedAt == nil {
		t.Fatalf("неполный applied status: %+v", instance.Status)
	}
}

func assertAuthenticationRejected(t *testing.T, h *harness, instance *testInstance, password string) {
	t.Helper()
	connection := dialPersistentConnection(t, h.publicAddr, instance.hostname, h.caFile, func() {})
	defer closeConnections([]*persistentConnection{connection})
	writeRESP(t, connection, "AUTH", "app", password)
	if _, err := readRESPResult(t, connection); err == nil {
		t.Fatal("неверный пароль app принят")
	}
}

func assertWrongSNIRejected(t *testing.T, h *harness, instance *testInstance) {
	t.Helper()
	wrongHost := *instance
	wrongHost.hostname = "missing." + h.baseDomain
	if output, err := h.dockerCLIWithTimeout(
		t,
		3*time.Second,
		&wrongHost,
		h.clientWrongSNI,
		instance.password,
		"PING",
	); err == nil &&
		strings.Contains(output, "PONG") {
		t.Fatalf("неверный SNI принят для %s", instance.slug)
	}
}

func setValue(t *testing.T, client *persistentConnection, key, value string) {
	t.Helper()
	writeRESP(t, client, "SET", key, value)
	if response := readRESP(t, client); response != "OK" {
		t.Fatalf("SET %s вернул %#v", key, response)
	}
}

func getValue(t *testing.T, client *persistentConnection, key string) string {
	t.Helper()
	writeRESP(t, client, "GET", key)
	value := readRESP(t, client)
	result, ok := value.(string)
	if !ok {
		t.Fatalf("GET %s вернул %#v", key, value)
	}
	return result
}

func assertPodIsolation(t *testing.T, h *harness, source, target *testInstance) {
	t.Helper()
	targetPod := h.getPod(t, target)
	output, err := execInPod(t, h.adminREST, source.namespace, source.slug+"-0", []string{
		"/bin/sh", "-c",
		"timeout 3 valkey-cli -h " + targetPod.Status.PodIP + " -p 6379 ping",
	})
	if err == nil {
		t.Fatalf("Pod одного инстанса подключился к другому: %s", output)
	}
}

func execInPod(
	t *testing.T,
	config *rest.Config,
	namespace string,
	pod string,
	command []string,
) (string, error) {
	t.Helper()
	return execInPodContext(t.Context(), config, namespace, pod, command)
}

func execInPodContext(
	ctx context.Context,
	config *rest.Config,
	namespace string,
	pod string,
	command []string,
) (string, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return "", fmt.Errorf("создать клиент exec: %w", err)
	}
	request := clientset.CoreV1().RESTClient().Post().Resource("pods").
		Namespace(namespace).Name(pod).SubResource("exec")
	request.VersionedParams(&corev1.PodExecOptions{
		Container: "valkey", Command: command, Stdout: true, Stderr: true,
	}, scheme.ParameterCodec)
	executor, err := remotecommand.NewSPDYExecutor(config, http.MethodPost, request.URL())
	if err != nil {
		return "", fmt.Errorf("создать exec для Pod: %w", err)
	}
	var output strings.Builder
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &output, Stderr: &output,
	})
	return output.String(), err
}

func loadKubeconfig(t *testing.T, path string) *rest.Config {
	t.Helper()
	config, err := clientcmd.BuildConfigFromFlags("", path)
	if err != nil {
		t.Fatalf("прочитать kubeconfig %s: %v", path, err)
	}
	return config
}

func requiredEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("%s не задан", name)
	}
	return value
}

func envOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func dockerGatewayCIDR(t *testing.T, networkName string) string {
	t.Helper()
	output, err := exec.Command(
		"docker", "network", "inspect", "-f", "{{(index .IPAM.Config 0).Gateway}}", networkName,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("прочитать gateway docker-сети %s: output=%q error=%v", networkName, output, err)
	}
	address := strings.TrimSpace(string(output))
	ip := net.ParseIP(address)
	if ip == nil {
		t.Fatalf("docker-сеть %s вернула неверный gateway %q", networkName, address)
	}
	if ip.To4() != nil {
		return address + "/32"
	}
	return address + "/128"
}

func podRouteSourceCIDR(t *testing.T, k8s client.Client) string {
	t.Helper()
	pods := &corev1.PodList{}
	if err := k8s.List(t.Context(), pods, client.InNamespace("envoy-gateway-system")); err != nil {
		t.Fatalf("прочитать Pod для определения маршрута: %v", err)
	}
	for _, pod := range pods.Items {
		if pod.Status.PodIP == "" {
			continue
		}
		output, err := exec.Command("ip", "route", "get", pod.Status.PodIP).CombinedOutput()
		if err != nil {
			t.Fatalf("определить маршрут до Pod %s: output=%q error=%v", pod.Name, output, err)
		}
		fields := strings.Fields(string(output))
		for index := 0; index+1 < len(fields); index++ {
			if fields[index] == "src" {
				return hostCIDR(t, fields[index+1])
			}
		}
		t.Fatalf("маршрут до Pod %s не содержит исходящий адрес: %q", pod.Name, output)
	}
	t.Fatal("не найден Pod для определения маршрута")

	return ""
}

func hostCIDR(t *testing.T, address string) string {
	t.Helper()
	ip := net.ParseIP(address)
	if ip == nil {
		t.Fatalf("неверный IP-адрес %q", address)
	}
	if ip.To4() != nil {
		return address + "/32"
	}
	return address + "/128"
}

func mustUUIDv7(t *testing.T) string {
	t.Helper()
	value, err := uuidNewV7()
	if err != nil {
		t.Fatalf("создать UUIDv7: %v", err)
	}
	return value
}

func uuidNewV7() (string, error) {
	value, err := uuid.NewV7()
	return value.String(), err
}
