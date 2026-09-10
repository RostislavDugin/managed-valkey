package operator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/portforward"
	transportspdy "k8s.io/client-go/transport/spdy"
	"sigs.k8s.io/controller-runtime/pkg/client"

	valkeyv1alpha1 "github.com/RostislavDugin/managed-valkey/operator/api/v1alpha1"
	"github.com/RostislavDugin/managed-valkey/operator/internal/config"
)

const (
	envoyAdminPort       = 19000
	maxEnvoyResponseSize = 32 * 1024 * 1024
)

var (
	errEnvoyPending = errors.New("конфигурация Envoy ещё применяется")
	errEnvoyUnknown = errors.New("формат конфигурации Envoy не распознан")
)

type EnvoyAdminSnapshot struct {
	ConfigDump   []byte
	Certificates []byte
}

type EnvoySnapshotReader func(context.Context, corev1.Pod) (EnvoyAdminSnapshot, error)

type EnvoySnapshotCache struct {
	mu            sync.Mutex
	captured      time.Time
	attempted     time.Time
	processes     []valkeyv1alpha1.EnvoyProcessStatus
	snapshots     []EnvoyAdminSnapshot
	failureReason string
	refreshing    bool
	refreshingFor []valkeyv1alpha1.EnvoyProcessStatus
	verifiedFor   string
	verifiedAt    time.Time
	verifiedState valkeyv1alpha1.NetworkVerificationStatus
	verifiedCause string
}

func NewEnvoySnapshotCache() *EnvoySnapshotCache {
	return &EnvoySnapshotCache{}
}

type envoySnapshotState struct {
	snapshots     []EnvoyAdminSnapshot
	captured      time.Time
	fresh         bool
	found         bool
	failureReason string
	refreshing    bool
	refreshDue    bool
}

func (c *EnvoySnapshotCache) load(
	now time.Time,
	targets []envoyTarget,
) envoySnapshotState {
	if c == nil {
		return envoySnapshotState{}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	processes := envoyProcessStatuses(targets)
	state := envoySnapshotState{
		refreshing: c.refreshing && sameEnvoyProcesses(c.refreshingFor, processes),
	}
	if !sameEnvoyProcesses(c.processes, processes) {
		state.refreshDue = true
		return state
	}
	state.found = len(c.snapshots) == len(targets) && len(c.snapshots) > 0
	state.snapshots = slices.Clone(c.snapshots)
	state.captured = c.captured
	state.failureReason = c.failureReason
	state.fresh = state.found && c.failureReason == "" && !c.captured.IsZero() &&
		!now.Before(c.captured) && now.Sub(c.captured) < config.NetworkVerifyInterval
	state.refreshDue = c.attempted.IsZero() || now.Before(c.attempted) ||
		now.Sub(c.attempted) >= config.NetworkVerifyInterval

	return state
}

func (c *EnvoySnapshotCache) beginRefresh(targets []envoyTarget) bool {
	if c == nil {
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refreshing {
		return false
	}
	c.refreshing = true
	c.refreshingFor = envoyProcessStatuses(targets)

	return true
}

func (c *EnvoySnapshotCache) finishRefresh(
	now time.Time,
	targets []envoyTarget,
	snapshots []EnvoyAdminSnapshot,
	failureReason string,
) {
	if c == nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	processes := envoyProcessStatuses(targets)
	if !c.refreshing || !sameEnvoyProcesses(c.refreshingFor, processes) {
		return
	}
	c.refreshing = false
	c.refreshingFor = nil
	compositionChanged := !sameEnvoyProcesses(c.processes, processes)
	c.attempted = now
	c.processes = processes
	c.failureReason = failureReason
	if failureReason == "" {
		c.captured = now
		c.snapshots = slices.Clone(snapshots)
		c.verifiedFor = ""
		c.verifiedAt = time.Time{}
		c.verifiedState = ""
		c.verifiedCause = ""
	} else if compositionChanged {
		c.captured = time.Time{}
		c.snapshots = nil
		c.verifiedFor = ""
		c.verifiedAt = time.Time{}
		c.verifiedState = ""
		c.verifiedCause = ""
	}
}

func (c *EnvoySnapshotCache) loadVerification(
	fingerprint string,
	capturedAt time.Time,
) (valkeyv1alpha1.NetworkVerificationStatus, string, bool) {
	if c == nil || capturedAt.IsZero() {
		return "", "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.verifiedFor != fingerprint || !c.verifiedAt.Equal(capturedAt) {
		return "", "", false
	}
	return c.verifiedState, c.verifiedCause, true
}

func (c *EnvoySnapshotCache) storeVerification(
	fingerprint string,
	capturedAt time.Time,
	status valkeyv1alpha1.NetworkVerificationStatus,
	reason string,
) {
	if c == nil || capturedAt.IsZero() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.verifiedFor = fingerprint
	c.verifiedAt = capturedAt
	c.verifiedState = status
	c.verifiedCause = reason
}

type envoyTarget struct {
	pod    corev1.Pod
	status valkeyv1alpha1.EnvoyProcessStatus
}

type envoyVerification struct {
	status     valkeyv1alpha1.NetworkVerificationStatus
	reason     string
	processes  []valkeyv1alpha1.EnvoyProcessStatus
	capturedAt time.Time
}

func (r *ValkeyInstanceReconciler) verifyEnvoy(
	ctx context.Context,
	expected networkPrerequisites,
) envoyVerification {
	targets, err := r.envoyTargets(ctx)
	if err != nil || len(targets) != 2 {
		return envoyVerification{
			status: valkeyv1alpha1.NetworkVerificationUnknown,
			reason: "EnvoyCompositionUnknown",
		}
	}
	processes := envoyProcessStatuses(targets)
	snapshots, capturedAt, failureReason := r.envoySnapshots(ctx, targets)
	if failureReason != "" {
		status := valkeyv1alpha1.NetworkVerificationUnknown
		if failureReason == "EnvoyRefreshPending" {
			status = valkeyv1alpha1.NetworkVerificationPending
		}
		return envoyVerification{
			status:    status,
			reason:    failureReason,
			processes: processes,
		}
	}
	fingerprint := networkFingerprint(expected)
	if status, reason, found := r.EnvoyCache.loadVerification(fingerprint, capturedAt); found {
		result := envoyVerification{status: status, reason: reason, processes: processes}
		if status == valkeyv1alpha1.NetworkVerificationVerified {
			result.capturedAt = capturedAt
		}
		return result
	}

	pending := false
	for _, snapshot := range snapshots {
		if err := validateEnvoySnapshot(snapshot, expected, r.now()); err != nil {
			if errors.Is(err, errEnvoyPending) {
				pending = true
				continue
			}

			return r.cacheEnvoyVerification(envoyVerification{
				status:    valkeyv1alpha1.NetworkVerificationUnknown,
				reason:    "EnvoyFormatUnknown",
				processes: processes,
			}, fingerprint, capturedAt)
		}
		if expected.ReadOnly != nil {
			if err := validateEnvoySnapshot(snapshot, *expected.ReadOnly, r.now()); err != nil {
				if errors.Is(err, errEnvoyPending) {
					pending = true
					continue
				}

				return r.cacheEnvoyVerification(envoyVerification{
					status:    valkeyv1alpha1.NetworkVerificationUnknown,
					reason:    "EnvoyFormatUnknown",
					processes: processes,
				}, fingerprint, capturedAt)
			}
		}
	}
	if pending {
		return r.cacheEnvoyVerification(envoyVerification{
			status:    valkeyv1alpha1.NetworkVerificationPending,
			reason:    "EnvoyConfigurationPending",
			processes: processes,
		}, fingerprint, capturedAt)
	}

	return r.cacheEnvoyVerification(envoyVerification{
		status:     valkeyv1alpha1.NetworkVerificationVerified,
		reason:     "EnvoyVerified",
		processes:  processes,
		capturedAt: capturedAt,
	}, fingerprint, capturedAt)
}

func (r *ValkeyInstanceReconciler) cacheEnvoyVerification(
	verification envoyVerification,
	fingerprint string,
	capturedAt time.Time,
) envoyVerification {
	r.EnvoyCache.storeVerification(fingerprint, capturedAt, verification.status, verification.reason)
	return verification
}

func (r *ValkeyInstanceReconciler) envoySnapshots(
	ctx context.Context,
	targets []envoyTarget,
) ([]EnvoyAdminSnapshot, time.Time, string) {
	if r.EnvoyCache == nil {
		snapshots, fresh, failureReason := r.readEnvoySnapshots(ctx, targets)
		if fresh && failureReason == "" {
			return snapshots, r.now(), ""
		}
		return snapshots, time.Time{}, failureReason
	}

	state := r.EnvoyCache.load(r.now(), targets)
	if state.fresh {
		return state.snapshots, state.captured, ""
	}
	if state.refreshDue && !state.refreshing && r.EnvoyCache.beginRefresh(targets) {
		go r.refreshEnvoySnapshots(context.WithoutCancel(ctx), targets)
		state.refreshing = true
	}
	if state.failureReason != "" {
		return nil, time.Time{}, state.failureReason
	}
	if state.found {
		return state.snapshots, state.captured, ""
	}
	if state.refreshing || state.refreshDue {
		return nil, time.Time{}, "EnvoyRefreshPending"
	}

	return nil, time.Time{}, "EnvoyAdminUnavailable"
}

func (r *ValkeyInstanceReconciler) refreshEnvoySnapshots(ctx context.Context, targets []envoyTarget) {
	refreshCtx, cancel := context.WithTimeout(ctx, config.NetworkVerifyTimeout)
	defer cancel()

	snapshots, _, failureReason := r.readEnvoySnapshots(refreshCtx, targets)
	r.EnvoyCache.finishRefresh(r.now(), targets, snapshots, failureReason)
}

func (r *ValkeyInstanceReconciler) readEnvoySnapshots(
	ctx context.Context,
	targets []envoyTarget,
) ([]EnvoyAdminSnapshot, bool, string) {
	read := r.ReadEnvoy
	if read == nil {
		read = r.readEnvoySnapshot
	}
	type snapshotResult struct {
		snapshot EnvoyAdminSnapshot
		err      error
	}
	results := make(chan snapshotResult, len(targets))
	for index := range targets {
		pod := targets[index].pod
		go func() {
			snapshot, err := read(ctx, pod)
			results <- snapshotResult{snapshot: snapshot, err: err}
		}()
	}

	snapshots := make([]EnvoyAdminSnapshot, 0, len(targets))
	for range targets {
		result := <-results
		if result.err != nil {
			return nil, false, "EnvoyAdminUnavailable"
		}
		snapshots = append(snapshots, result.snapshot)
	}

	after, err := r.envoyTargets(ctx)
	if err != nil || !sameEnvoyTargets(targets, after) {
		return nil, false, "EnvoyCompositionChanged"
	}
	return snapshots, true, ""
}

func (r *ValkeyInstanceReconciler) envoyTargets(ctx context.Context) ([]envoyTarget, error) {
	pods := &corev1.PodList{}
	if err := r.List(
		ctx,
		pods,
		client.InNamespace(envoyNamespace),
		client.MatchingLabels{
			"gateway.envoyproxy.io/owning-gateway-name":      gatewayName,
			"gateway.envoyproxy.io/owning-gateway-namespace": r.SystemNamespace,
		},
	); err != nil {
		return nil, fmt.Errorf("прочитать Pod Envoy: %w", err)
	}

	targets := make([]envoyTarget, 0, len(pods.Items))
	for index := range pods.Items {
		pod := &pods.Items[index]
		container := envoyContainerStatus(pod.Status.ContainerStatuses)
		if pod.Spec.NodeName == "" || container == nil || container.ContainerID == "" ||
			container.State.Running == nil {
			continue
		}
		node := &corev1.Node{}
		if err := r.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, node); err != nil {
			return nil, fmt.Errorf("прочитать Node Envoy: %w", err)
		}
		targets = append(targets, envoyTarget{
			pod: *pod.DeepCopy(),
			status: valkeyv1alpha1.EnvoyProcessStatus{
				PodUID:      string(pod.UID),
				NodeName:    node.Name,
				NodeUID:     string(node.UID),
				ContainerID: container.ContainerID,
			},
		})
	}
	slices.SortFunc(targets, func(left, right envoyTarget) int {
		return strings.Compare(left.status.PodUID, right.status.PodUID)
	})

	return targets, nil
}

func envoyContainerStatus(statuses []corev1.ContainerStatus) *corev1.ContainerStatus {
	for index := range statuses {
		if statuses[index].Name == "envoy" {
			return &statuses[index]
		}
	}

	return nil
}

func sameEnvoyTargets(left, right []envoyTarget) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].status != right[index].status {
			return false
		}
	}

	return true
}

func envoyProcessStatuses(targets []envoyTarget) []valkeyv1alpha1.EnvoyProcessStatus {
	processes := make([]valkeyv1alpha1.EnvoyProcessStatus, len(targets))
	for index := range targets {
		processes[index] = targets[index].status
	}

	return processes
}

func sameEnvoyProcesses(
	left []valkeyv1alpha1.EnvoyProcessStatus,
	right []valkeyv1alpha1.EnvoyProcessStatus,
) bool {
	if len(left) != len(right) {
		return false
	}
	left = slices.Clone(left)
	right = slices.Clone(right)
	slices.SortFunc(left, compareEnvoyProcesses)
	slices.SortFunc(right, compareEnvoyProcesses)

	return slices.Equal(left, right)
}

func compareEnvoyProcesses(
	left valkeyv1alpha1.EnvoyProcessStatus,
	right valkeyv1alpha1.EnvoyProcessStatus,
) int {
	if result := strings.Compare(left.PodUID, right.PodUID); result != 0 {
		return result
	}
	if result := strings.Compare(left.ContainerID, right.ContainerID); result != 0 {
		return result
	}
	if result := strings.Compare(left.NodeUID, right.NodeUID); result != 0 {
		return result
	}

	return strings.Compare(left.NodeName, right.NodeName)
}

func (r *ValkeyInstanceReconciler) readEnvoySnapshot(
	ctx context.Context,
	pod corev1.Pod,
) (EnvoyAdminSnapshot, error) {
	if r.RESTConfig == nil {
		return EnvoyAdminSnapshot{}, errors.New("конфигурация Kubernetes для port-forward отсутствует")
	}
	clientset, err := kubernetes.NewForConfig(r.RESTConfig)
	if err != nil {
		return EnvoyAdminSnapshot{}, fmt.Errorf("создать клиент port-forward: %w", err)
	}
	requestURL := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(pod.Namespace).
		Name(pod.Name).
		SubResource("portforward").
		URL()
	roundTripper, upgrader, err := transportspdy.RoundTripperFor(r.RESTConfig)
	if err != nil {
		return EnvoyAdminSnapshot{}, fmt.Errorf("создать транспорт port-forward: %w", err)
	}
	dialer := transportspdy.NewDialer(
		upgrader,
		&http.Client{Transport: roundTripper},
		http.MethodPost,
		requestURL,
	)
	stop := make(chan struct{})
	ready := make(chan struct{})
	forwarder, err := portforward.NewOnAddresses(
		dialer,
		[]string{"127.0.0.1"},
		[]string{fmt.Sprintf("0:%d", envoyAdminPort)},
		stop,
		ready,
		io.Discard,
		io.Discard,
	)
	if err != nil {
		return EnvoyAdminSnapshot{}, fmt.Errorf("настроить port-forward Envoy: %w", err)
	}
	forwardErr := make(chan error, 1)
	go func() {
		forwardErr <- forwarder.ForwardPorts()
	}()
	select {
	case <-ctx.Done():
		close(stop)
		return EnvoyAdminSnapshot{}, ctx.Err()
	case err := <-forwardErr:
		close(stop)
		return EnvoyAdminSnapshot{}, fmt.Errorf("запустить port-forward Envoy: %w", err)
	case <-ready:
	}
	defer close(stop)

	ports, err := forwarder.GetPorts()
	if err != nil || len(ports) != 1 {
		return EnvoyAdminSnapshot{}, fmt.Errorf("получить локальный порт Envoy: %w", err)
	}
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", ports[0].Local)
	configDump, err := fetchEnvoyAdmin(ctx, baseURL+"/config_dump?include_eds")
	if err != nil {
		return EnvoyAdminSnapshot{}, err
	}
	certificates, err := fetchEnvoyAdmin(ctx, baseURL+"/certs")
	if err != nil {
		return EnvoyAdminSnapshot{}, err
	}

	return EnvoyAdminSnapshot{ConfigDump: configDump, Certificates: certificates}, nil
}

func fetchEnvoyAdmin(ctx context.Context, address string) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, fmt.Errorf("сформировать запрос admin API Envoy: %w", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("прочитать admin API Envoy: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("admin API Envoy вернул HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxEnvoyResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("прочитать ответ admin API Envoy: %w", err)
	}
	if len(data) > maxEnvoyResponseSize {
		return nil, errors.New("ответ admin API Envoy превышает допустимый размер")
	}

	return data, nil
}

type envoyConfigDump struct {
	Configs []json.RawMessage `json:"configs"`
}

type envoyTypedConfig struct {
	Type string `json:"@type"`
}

type envoyListenersDump struct {
	DynamicListeners []envoyDynamicListener `json:"dynamic_listeners"`
}

type envoyDynamicListener struct {
	ActiveState  *envoyListenerState `json:"active_state"`
	WarmingState *envoyListenerState `json:"warming_state"`
}

type envoyListenerState struct {
	Listener envoyListener `json:"listener"`
}

type envoyListener struct {
	FilterChains []envoyFilterChain `json:"filter_chains"`
}

type envoyFilterChain struct {
	FilterChainMatch struct {
		ServerNames []string `json:"server_names"`
	} `json:"filter_chain_match"`
	Filters         []envoyNetworkFilter `json:"filters"`
	TransportSocket *struct {
		TypedConfig json.RawMessage `json:"typed_config"`
	} `json:"transport_socket"`
}

type envoyNetworkFilter struct {
	Name        string          `json:"name"`
	TypedConfig json.RawMessage `json:"typed_config"`
}

type envoyEndpointsDump struct {
	DynamicEndpointConfigs []envoyDynamicEndpointConfig `json:"dynamic_endpoint_configs"`
}

type envoyDynamicEndpointConfig struct {
	EndpointConfig envoyClusterLoadAssignment `json:"endpoint_config"`
}

type envoyClusterLoadAssignment struct {
	ClusterName string `json:"cluster_name"`
	Endpoints   []struct {
		LBEndpoints []struct {
			Endpoint struct {
				Address struct {
					SocketAddress struct {
						Address   string `json:"address"`
						PortValue int32  `json:"port_value"`
					} `json:"socket_address"`
				} `json:"address"`
			} `json:"endpoint"`
		} `json:"lb_endpoints"`
	} `json:"endpoints"`
}

type envoyCertificates struct {
	Certificates []struct {
		CertChain []struct {
			Path            string `json:"path"`
			SerialNumber    string `json:"serial_number"`
			SubjectAltNames []struct {
				DNS string `json:"dns"`
			} `json:"subject_alt_names"`
			ValidFrom      string `json:"valid_from"`
			ExpirationTime string `json:"expiration_time"`
		} `json:"cert_chain"`
	} `json:"certificates"`
}

func validateEnvoySnapshot(
	snapshot EnvoyAdminSnapshot,
	expected networkPrerequisites,
	now time.Time,
) error {
	configuration := envoyConfigDump{}
	if err := json.Unmarshal(snapshot.ConfigDump, &configuration); err != nil ||
		len(configuration.Configs) == 0 {
		return errors.Join(errEnvoyUnknown, err)
	}

	var listeners *envoyListenersDump
	var endpoints *envoyEndpointsDump
	for _, raw := range configuration.Configs {
		header := envoyTypedConfig{}
		if err := json.Unmarshal(raw, &header); err != nil {
			return errors.Join(errEnvoyUnknown, err)
		}
		switch {
		case strings.HasSuffix(header.Type, "ListenersConfigDump"):
			value := &envoyListenersDump{}
			if err := json.Unmarshal(raw, value); err != nil {
				return errors.Join(errEnvoyUnknown, err)
			}
			listeners = value
		case strings.HasSuffix(header.Type, "EndpointsConfigDump"):
			value := &envoyEndpointsDump{}
			if err := json.Unmarshal(raw, value); err != nil {
				return errors.Join(errEnvoyUnknown, err)
			}
			endpoints = value
		}
	}
	if listeners == nil || endpoints == nil {
		return errEnvoyUnknown
	}

	activeChain, warming := findEnvoyFilterChain(listeners.DynamicListeners, expected.Hostname)
	if activeChain == nil {
		if warming {
			return errEnvoyPending
		}

		return errEnvoyPending
	}
	routeName := expected.RouteName
	if routeName == "" {
		routeName = strings.TrimSuffix(expected.BackendService, "-primary")
	}
	expectedCluster := fmt.Sprintf("tcproute/%s/%s/rule/-1", expected.BackendNamespace, routeName)
	if err := validateEnvoyFilterChain(*activeChain, expected, expectedCluster); err != nil {
		return err
	}
	if expected.BackendAddress != "" &&
		!envoyEndpointReady(*endpoints, expectedCluster, expected.BackendAddress, expected.BackendPort) {
		return errEnvoyPending
	}
	if err := validateEnvoyCertificates(snapshot.Certificates, expected, now); err != nil {
		return err
	}

	return nil
}

func findEnvoyFilterChain(
	listeners []envoyDynamicListener,
	hostname string,
) (*envoyFilterChain, bool) {
	warming := false
	for _, listener := range listeners {
		if listener.ActiveState != nil {
			for index := range listener.ActiveState.Listener.FilterChains {
				chain := &listener.ActiveState.Listener.FilterChains[index]
				if slices.Contains(chain.FilterChainMatch.ServerNames, hostname) {
					return chain, warming
				}
			}
		}
		if listener.WarmingState != nil {
			for _, chain := range listener.WarmingState.Listener.FilterChains {
				if slices.Contains(chain.FilterChainMatch.ServerNames, hostname) {
					warming = true
				}
			}
		}
	}

	return nil, warming
}

func validateEnvoyFilterChain(
	chain envoyFilterChain,
	expected networkPrerequisites,
	expectedCluster string,
) error {
	if chain.TransportSocket == nil ||
		!rawJSONContainsString(chain.TransportSocket.TypedConfig, expected.CertificateSecret) {
		return errEnvoyPending
	}

	var tcpProxy *envoyNetworkFilter
	var rbac *envoyNetworkFilter
	for index := range chain.Filters {
		switch chain.Filters[index].Name {
		case "envoy.filters.network.tcp_proxy":
			tcpProxy = &chain.Filters[index]
		case "envoy.filters.network.rbac":
			rbac = &chain.Filters[index]
		}
	}
	if tcpProxy == nil {
		return errEnvoyUnknown
	}
	proxy := struct {
		Cluster     string `json:"cluster"`
		IdleTimeout string `json:"idle_timeout"`
	}{}
	if err := json.Unmarshal(tcpProxy.TypedConfig, &proxy); err != nil {
		return errors.Join(errEnvoyUnknown, err)
	}
	if proxy.Cluster != expectedCluster || proxy.IdleTimeout != expected.IdleTimeout {
		return errEnvoyPending
	}
	if !expected.WhitelistEnabled {
		if rbac != nil {
			return errEnvoyPending
		}

		return nil
	}
	if rbac == nil {
		return errEnvoyPending
	}
	cidrs, hasDefaultDeny, hasAllow, err := envoyRBACState(rbac.TypedConfig)
	if err != nil {
		return err
	}
	wantCIDRs := slices.Clone(expected.CIDRs)
	slices.Sort(cidrs)
	slices.Sort(wantCIDRs)
	if !hasDefaultDeny || !slices.Equal(cidrs, wantCIDRs) ||
		len(wantCIDRs) > 0 && !hasAllow {
		return errEnvoyPending
	}

	return nil
}

func rawJSONContainsString(raw json.RawMessage, expected string) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	return anyContainsString(value, expected)
}

func anyContainsString(value any, expected string) bool {
	switch typed := value.(type) {
	case string:
		return typed == expected
	case []any:
		for _, item := range typed {
			if anyContainsString(item, expected) {
				return true
			}
		}
	case map[string]any:
		for _, item := range typed {
			if anyContainsString(item, expected) {
				return true
			}
		}
	}

	return false
}

func envoyRBACState(raw json.RawMessage) ([]string, bool, bool, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, false, false, errors.Join(errEnvoyUnknown, err)
	}
	cidrs := collectCIDRs(value)
	defaultValues := valuesForKey(value, "on_no_match")
	hasDefaultDeny := slices.ContainsFunc(defaultValues, func(value any) bool {
		return anyContainsString(value, "DENY")
	})
	hasAllow := anyContainsString(value, "ALLOW")
	if len(defaultValues) == 0 {
		return nil, false, false, errEnvoyUnknown
	}

	return cidrs, hasDefaultDeny, hasAllow, nil
}

func collectCIDRs(value any) []string {
	result := make([]string, 0)
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case []any:
			for _, item := range typed {
				walk(item)
			}
		case map[string]any:
			address, addressOK := typed["address_prefix"].(string)
			prefix, prefixOK := typed["prefix_len"].(float64)
			if addressOK && prefixOK {
				value, err := netip.ParsePrefix(fmt.Sprintf("%s/%d", address, int(prefix)))
				if err == nil {
					result = append(result, value.Masked().String())
				}
			}
			for _, item := range typed {
				walk(item)
			}
		}
	}
	walk(value)
	slices.Sort(result)
	return slices.Compact(result)
}

func valuesForKey(value any, key string) []any {
	result := make([]any, 0)
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case []any:
			for _, item := range typed {
				walk(item)
			}
		case map[string]any:
			for name, item := range typed {
				if name == key {
					result = append(result, item)
				}
				walk(item)
			}
		}
	}
	walk(value)
	return result
}

func envoyEndpointReady(
	dump envoyEndpointsDump,
	cluster string,
	address string,
	port int32,
) bool {
	for _, dynamic := range dump.DynamicEndpointConfigs {
		if dynamic.EndpointConfig.ClusterName != cluster {
			continue
		}
		for _, locality := range dynamic.EndpointConfig.Endpoints {
			for _, endpoint := range locality.LBEndpoints {
				socket := endpoint.Endpoint.Address.SocketAddress
				if socket.Address == address && socket.PortValue == port {
					return true
				}
			}
		}
	}

	return false
}

func validateEnvoyCertificates(
	raw []byte,
	expected networkPrerequisites,
	now time.Time,
) error {
	certificates := envoyCertificates{}
	if err := json.Unmarshal(raw, &certificates); err != nil {
		return errors.Join(errEnvoyUnknown, err)
	}
	for _, certificate := range certificates.Certificates {
		for _, chain := range certificate.CertChain {
			if normalizeSerial(chain.SerialNumber) != normalizeSerial(expected.CertificateSerial) {
				continue
			}
			validFrom, fromErr := time.Parse(time.RFC3339, chain.ValidFrom)
			expires, expiresErr := time.Parse(time.RFC3339, chain.ExpirationTime)
			if fromErr != nil || expiresErr != nil {
				return errEnvoyUnknown
			}
			if now.Before(validFrom) || !now.Before(expires) ||
				!expires.Equal(expected.CertificateExpiry) {
				return errEnvoyPending
			}
			names := make([]string, len(chain.SubjectAltNames))
			for index := range chain.SubjectAltNames {
				names[index] = chain.SubjectAltNames[index].DNS
			}
			if dnsNameMatches(names, expected.Hostname) {
				return nil
			}
		}
	}

	return errEnvoyPending
}

func dnsNameMatches(names []string, hostname string) bool {
	for _, name := range names {
		if name == hostname {
			return true
		}
		if strings.HasPrefix(name, "*.") {
			suffix := strings.TrimPrefix(name, "*")
			if strings.HasSuffix(hostname, suffix) &&
				strings.Count(hostname, ".") == strings.Count(name, ".") {
				return true
			}
		}
	}

	return false
}

func normalizeSerial(value string) string {
	value = strings.ToLower(strings.ReplaceAll(value, ":", ""))
	value = strings.TrimLeft(value, "0")
	if value == "" {
		return "0"
	}

	return value
}
