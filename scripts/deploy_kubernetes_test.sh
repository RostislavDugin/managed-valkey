#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT

fake_bin=$work_dir/bin
state_dir=$work_dir/state
admin_kubeconfig=$work_dir/admin.kubeconfig
api_kubeconfig=$work_dir/api.kubeconfig
mkdir -p "$fake_bin" "$state_dir/active"
printf '%s\n' 'apiVersion: v1' >"$admin_kubeconfig"
openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
    -subj /CN=managed-valkey-test-kubelet-ca \
    -keyout "$work_dir/kubelet-ca.key" \
    -out "$work_dir/kubelet-ca.crt" >/dev/null 2>&1
openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
    -subj /CN=managed-valkey-test-kubelet-ca-rotated \
    -keyout "$work_dir/kubelet-ca-rotated.key" \
    -out "$work_dir/kubelet-ca-rotated.crt" >/dev/null 2>&1
openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
    -subj /CN=managed-valkey-test-kubelet-ca-expiring \
    -keyout "$work_dir/kubelet-ca-expiring.key" \
    -out "$work_dir/kubelet-ca-expiring.crt" >/dev/null 2>&1
printf '%s\n' 'not a certificate' >"$work_dir/kubelet-ca-invalid.crt"
cp "$work_dir/kubelet-ca.crt" "$work_dir/kubelet-ca-rotation-bundle.crt"
printf '\n' >>"$work_dir/kubelet-ca-rotation-bundle.crt"
cat "$work_dir/kubelet-ca-rotated.crt" >>"$work_dir/kubelet-ca-rotation-bundle.crt"

cat >"$fake_bin/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

printf '%s\n' "$*" >>"$TEST_KUBECTL_LOG"

metrics_mode=false
if [[ $TEST_KUBECTL_MODE == metrics-* ]]; then
    metrics_mode=true
fi

if [[ $* == 'get --raw=/readyz' ]]; then
    exit 0
fi
if [[ ${1:-} == get && ${2:-} == statefulsets.apps ]]; then
    if [[ $TEST_KUBECTL_MODE == legacy-statefulset ]]; then
        printf '%s\n' tenant-a/valkey-a
    fi
    exit 0
fi
if [[ ${1:-} == get && ${2:-} == nodes ]]; then
    if [[ ${3:-} == -o && ${4:-} == json ]]; then
        printf '%s\n' '{"items":[{"metadata":{"name":"node-a"},"status":{"addresses":[{"type":"InternalIP","address":"10.17.0.35"}]}}]}'
    elif [[ $TEST_KUBECTL_MODE == broken-node-network ]]; then
        if [[ $* == *InternalIP* ]]; then
            printf '%s\n' 10.17.0.26 10.17.0.27
        else
            printf '%s\n' node-a node-b
        fi
    elif [[ $* == *InternalIP* ]]; then
        printf '%s\n' 10.17.0.35
    else
        printf '%s\n' node-a
    fi
    exit 0
fi
if [[ $* == '-n kube-system get pods -l k8s-app=cilium --field-selector=status.phase=Running -o name' ]]; then
    if [[ $TEST_KUBECTL_MODE == broken-node-network ]]; then
        printf '%s\n' pod/cilium-a pod/cilium-b
    else
        printf '%s\n' pod/cilium-a
    fi
    exit 0
fi
if [[ ${1:-} == -n && ${2:-} == kube-system && ${3:-} == exec ]]; then
    if [[ $TEST_KUBECTL_MODE == broken-node-network && ${4:-} == pod/cilium-a ]]; then
        printf '%s\n' 'Cluster health: 1/2 reachable'
    elif [[ $TEST_KUBECTL_MODE == broken-node-network ]]; then
        printf '%s\n' 'Cluster health: 2/2 reachable'
    else
        printf '%s\n' 'Cluster health: 1/1 reachable'
    fi
    exit 0
fi
if [[ ${1:-} == -n && ${3:-} == delete && ${4:-} == pod ]]; then
    rm -f -- "$TEST_KUBECTL_STATE/active/${5:-}"
    exit 0
fi
if [[ ${1:-} == apply && ${2:-} == -f && ${3:-} == - ]]; then
    payload=$TEST_KUBECTL_STATE/payload.$$
    cat >"$payload"
    printf '%s\n' '---' >>"$TEST_KUBECTL_YAML_LOG"
    cat "$payload" >>"$TEST_KUBECTL_YAML_LOG"
    if grep -Fq 'kind: Pod' "$payload"; then
        pod=$(awk '$1 == "name:" { print $2; exit }' "$payload")
        cp "$payload" "$TEST_KUBECTL_STATE/active/$pod"
    fi
    rm -f -- "$payload"
    exit 0
fi
if [[ $metrics_mode == true && ${1:-} == -n && ${2:-} == valkey-system &&
    ${3:-} == get && ${4:-} == pod && ${5:-} == operator-0 ]]; then
    printf '%s' 'ghcr.io/rostislavdugin/managed-valkey-operator:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
    exit 0
fi
if [[ ${1:-} == -n && ${3:-} == get && ${4:-} == pod ]]; then
    pod=${5:-}
    expected_exit_code=${pod##*-}
    actual_exit_code=$expected_exit_code
    if [[ $TEST_KUBECTL_MODE == wrong-exit && $pod == *-0-42 ]]; then
        actual_exit_code=17
    fi
    query=${*: -1}
    case "$query" in
    *restartCount*)
        if [[ $TEST_KUBECTL_MODE == restarted && $pod == *-0-42 ]]; then
            printf 1
        else
            printf 0
        fi
        ;;
    *status.phase*)
        if [[ $expected_exit_code == 0 ]]; then
            printf Succeeded
        else
            printf Failed
        fi
        ;;
    *terminated.exitCode*) printf '%s' "$actual_exit_code" ;;
    *terminated.reason*)
        if [[ $expected_exit_code == 0 ]]; then
            printf Completed
        else
            printf Error
        fi
        ;;
    *) exit 2 ;;
    esac
    exit 0
fi
if [[ ${1:-} == apply && ${2:-} == -f && ${3:-} == */deploy/prod/namespace.yaml ]]; then
    if [[ $metrics_mode == true ]]; then
        exit 0
    fi
    exit 73
fi
if [[ $metrics_mode == true && ${1:-} == wait && ${3:-} == namespace/valkey-system ]]; then
    exit 0
fi
if [[ $metrics_mode == true && ${1:-} == -n && ${2:-} == kube-system &&
    ${3:-} == patch && ${4:-} == deployment && ${5:-} == metrics-server ]]; then
    exit 0
fi
if [[ $metrics_mode == true && ${1:-} == patch &&
    ${2:-} == apiservice && ${3:-} == v1beta1.metrics.k8s.io ]]; then
    exit 0
fi
if [[ $metrics_mode == true && ${1:-} == -n && ${2:-} == kube-system &&
    ${3:-} == rollout && ${4:-} == status ]]; then
    [[ $TEST_KUBECTL_MODE != metrics-deployment-unavailable ]]
    exit
fi
if [[ $TEST_KUBECTL_MODE == metrics-deployment-unavailable ]]; then
    exit 0
fi
if [[ $metrics_mode == true && ${1:-} == wait && ${3:-} == apiservice/v1beta1.metrics.k8s.io ]]; then
    [[ $TEST_KUBECTL_MODE != metrics-api-unavailable ]]
    exit
fi
if [[ $metrics_mode == true && ${1:-} == wait ]]; then
    exit 0
fi
if [[ $TEST_KUBECTL_MODE == metrics-install* && ${1:-} == apply && ${2:-} == --server-side ]]; then
    exit 73
fi
if [[ $metrics_mode == true && ${1:-} == apply ]]; then
    exit 0
fi
if [[ $metrics_mode == true && ${1:-} == -n && ${2:-} == cert-manager &&
    ${3:-} == create && ${4:-} == secret ]]; then
    printf '%s\n' 'apiVersion: v1' 'kind: Secret' 'metadata:' '  name: cloudflare-api-token'
    exit 0
fi
if [[ $metrics_mode == true && ${1:-} == -n && ${2:-} == kube-system &&
    ${3:-} == create && ${4:-} == configmap && ${5:-} == metrics-server-kubelet-ca ]]; then
    printf '%s\n' 'apiVersion: v1' 'kind: ConfigMap' 'metadata:' '  name: metrics-server-kubelet-ca'
    exit 0
fi
if [[ $metrics_mode == true && ${1:-} == kustomize ]]; then
    printf '%s\n' 'apiVersion: v1' 'kind: List' 'items: []'
    exit 0
fi
if [[ $metrics_mode == true && ${1:-} == -n && ${2:-} == valkey-system &&
    ${3:-} == wait && ${5:-} == pod/operator-0 ]]; then
    exit 0
fi
if [[ $metrics_mode == true && ${1:-} == auth && ${2:-} == can-i &&
    $* == *pods.metrics.k8s.io* ]]; then
    if [[ $TEST_KUBECTL_MODE == metrics-rbac-denied ]]; then
        printf '%s\n' no
    else
        printf '%s\n' yes
    fi
    exit 0
fi
if [[ $metrics_mode == true && ${1:-} == --as=* && $* == *'get pods.metrics.k8s.io operator-0'* ]]; then
    if [[ $TEST_KUBECTL_MODE == metrics-timeout ]]; then
        printf '%s\t%s\n' '2000-01-01T00:00:00Z' '12500000n'
    else
        printf '%s\t%s\n' "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" '12500000n'
    fi
    exit 0
fi
if [[ $metrics_mode == true && ${1:-} == -n && ${2:-} == valkey-system &&
    ${3:-} == get && ${4:-} == secret && ${5:-} == managed-valkey-api-token ]]; then
    printf '%s' test-api-token
    exit 0
fi
if [[ $metrics_mode == true && ${1:-} == config && ${2:-} == view ]]; then
    if [[ $* == *certificate-authority-data* ]]; then
        printf '%s' dGVzdC1jYQ==
    else
        printf '%s' https://127.0.0.1:6443
    fi
    exit 0
fi
if [[ $metrics_mode == true && ${1:-} == --kubeconfig && ${3:-} == auth && ${4:-} == can-i ]]; then
    if [[ $* == *--subresource=status* ]]; then
        printf '%s\n' no
    else
        printf '%s\n' yes
    fi
    exit 0
fi
if [[ $metrics_mode == true && ${1:-} == --kubeconfig && ${3:-} == get &&
    ${4:-} == namespace ]]; then
    exit 0
fi
if [[ $metrics_mode == true && ${1:-} == -n && ${3:-} == wait ]]; then
    exit 0
fi
if [[ $metrics_mode == true && ${1:-} == -n && ${2:-} == envoy-gateway-system &&
    ${3:-} == get && ${4:-} == service ]]; then
    if [[ $* == *ingress*hostname* ]]; then
        exit 0
    fi
    printf '%s' 203.0.113.10
    exit 0
fi

echo "неожиданный вызов kubectl: $*" >&2
exit 2
EOF

cat >"$fake_bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ ${1:-} == manifest && ${2:-} == inspect ]]
EOF

cat >"$fake_bin/helm" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

printf '%s\n' "$*" >>"$TEST_HELM_LOG"

if [[ $TEST_KUBECTL_MODE != metrics-* ]]; then
    exit 2
fi
if [[ ($TEST_KUBECTL_MODE == metrics-helm-retry ||
    $TEST_KUBECTL_MODE == metrics-helm-network-failure) &&
    $* == *'upgrade --install envoy-gateway '* ]]; then
    retry_count=$(<"$TEST_KUBECTL_STATE/helm-retry-count")
    retry_count=$((retry_count + 1))
    printf '%s\n' "$retry_count" >"$TEST_KUBECTL_STATE/helm-retry-count"
    if [[ $TEST_KUBECTL_MODE == metrics-helm-network-failure ]] || ((retry_count < 3)); then
        echo 'Error: failed to perform "Fetch" on source: connection reset by peer' >&2
        exit 1
    fi
fi
if [[ $TEST_KUBECTL_MODE == metrics-cert-manager-helm-retry &&
    $* == *'upgrade --install cert-manager '* ]]; then
    retry_count=$(<"$TEST_KUBECTL_STATE/helm-retry-count")
    retry_count=$((retry_count + 1))
    printf '%s\n' "$retry_count" >"$TEST_KUBECTL_STATE/helm-retry-count"
    if ((retry_count < 2)); then
        echo 'Error: failed to perform "Resolve" on source: i/o timeout' >&2
        exit 1
    fi
fi
if [[ $TEST_KUBECTL_MODE == metrics-helm-permanent-failure &&
    $* == *'upgrade --install envoy-gateway '* ]]; then
    echo 'Error: rendered manifests contain a resource that already exists' >&2
    exit 1
fi
if [[ $* == 'repo add metrics-server https://kubernetes-sigs.github.io/metrics-server/ --force-update' ]]; then
    exit 0
fi
if [[ ${1:-} == upgrade && ${2:-} == --install ]]; then
    exit 0
fi
exit 2
EOF

cat >"$fake_bin/getent" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ ${1:-} == ahostsv4 ]]
if [[ ${2:-} == probe.valkey.h3llo-demo.com ]]; then
    printf '%s\n' '203.0.113.10 STREAM probe'
    exit 0
fi
[[ ${2:-} == cert-manager-webhook.valkey.h3llo-demo.com ]]
if [[ $TEST_KUBECTL_MODE == broken-node-network ]]; then
    printf '%s\n' '10.17.0.26 STREAM webhook' '10.17.0.27 STREAM webhook'
else
    printf '%s\n' '10.17.0.35 STREAM webhook'
fi
if [[ $TEST_KUBECTL_MODE == stale-webhook-dns ]]; then
    printf '%s\n' '10.17.0.36 STREAM webhook'
fi
EOF

cat >"$fake_bin/sleep" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

chmod 0755 \
    "$fake_bin/kubectl" "$fake_bin/docker" "$fake_bin/getent" "$fake_bin/helm" "$fake_bin/sleep"

run_case() {
    local mode=$1
    local output=$2
    local kubelet_ca=${3:-$work_dir/kubelet-ca.crt}

    rm -rf -- "$state_dir/active"
    mkdir -p "$state_dir/active"
    : >"$state_dir/kubectl.log"
    : >"$state_dir/helm.log"
    : >"$state_dir/pods.yaml"
    printf '0\n' >"$state_dir/helm-retry-count"

    set +e
    PATH="$fake_bin:$PATH" \
        TEST_KUBECTL_MODE="$mode" \
        TEST_KUBECTL_LOG="$state_dir/kubectl.log" \
        TEST_HELM_LOG="$state_dir/helm.log" \
        TEST_KUBECTL_STATE="$state_dir" \
        TEST_KUBECTL_YAML_LOG="$state_dir/pods.yaml" \
        CLOUDFLARE_API_TOKEN=test-token \
        "$repo_root/scripts/deploy_kubernetes.sh" \
        aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
        "$admin_kubeconfig" \
        "$api_kubeconfig" \
        "$kubelet_ca" >"$output" 2>&1
    case_status=$?
    set -e
}

run_case success "$work_dir/success.log"
[[ $case_status == 73 ]]
grep -Fq "apply -f $repo_root/deploy/prod/namespace.yaml" "$state_dir/kubectl.log"
[[ $(grep -c '^  restartPolicy: Never$' "$state_dir/pods.yaml") == 2 ]]
[[ $(grep -c 'command: \["/bin/sh", "-c", "exit 0"\]' "$state_dir/pods.yaml") == 1 ]]
[[ $(grep -c 'command: \["/bin/sh", "-c", "exit 42"\]' "$state_dir/pods.yaml") == 1 ]]
[[ $(grep -c '^  nodeName: node-a$' "$state_dir/pods.yaml") == 2 ]]
if grep -Eq '^      restartPolicy:' "$state_dir/pods.yaml"; then
    echo "политика перезапуска задана контейнеру" >&2
    exit 1
fi
[[ -z $(find "$state_dir/active" -type f -print -quit) ]]

run_case metrics-install "$work_dir/metrics-install.log"
[[ $case_status == 73 ]]
grep -Fq 'repo add metrics-server https://kubernetes-sigs.github.io/metrics-server/ --force-update' \
    "$state_dir/helm.log"
grep -Fq 'upgrade --install metrics-server metrics-server/metrics-server --version 3.14.0' \
    "$state_dir/helm.log"
grep -Fq -- '--namespace kube-system --set replicas=1 --set-string image.tag=v0.9.0' \
    "$state_dir/helm.log"
grep -Fq -- "--values $repo_root/deploy/prod/metrics-server-values.yaml" \
    "$state_dir/helm.log"
if grep -Fq -- '--wait' "$state_dir/helm.log"; then
    echo "Helm ожидает Deployment без диагностики" >&2
    exit 1
fi
if grep -Fq -- '--kubelet-insecure-tls' "$state_dir/helm.log"; then
    echo "проверка TLS kubelet отключена" >&2
    exit 1
fi
grep -Fxq '  - --kubelet-preferred-address-types=Hostname,InternalDNS,InternalIP,ExternalDNS,ExternalIP' \
    "$repo_root/deploy/prod/metrics-server-values.yaml"
grep -Fxq '  - --kubelet-certificate-authority=/etc/kubelet-ca/ca.crt' \
    "$repo_root/deploy/prod/metrics-server-values.yaml"
grep -Fxq 'containerPort: 4443' "$repo_root/deploy/prod/metrics-server-values.yaml"
grep -Fxq 'hostNetwork:' "$repo_root/deploy/prod/metrics-server-values.yaml"
grep -Fxq '  enabled: true' "$repo_root/deploy/prod/metrics-server-values.yaml"
grep -Fxq 'updateStrategy:' "$repo_root/deploy/prod/metrics-server-values.yaml"
grep -Fxq '    maxSurge: 0' "$repo_root/deploy/prod/metrics-server-values.yaml"
grep -Fxq '    maxUnavailable: 1' "$repo_root/deploy/prod/metrics-server-values.yaml"
if rg -Fq -- '--kubelet-insecure-tls' "$repo_root/deploy/prod/metrics-server-values.yaml"; then
    echo "проверка TLS kubelet отключена в значениях chart" >&2
    exit 1
fi
grep -Fq -- "-n kube-system create configmap metrics-server-kubelet-ca --from-file=ca.crt=$work_dir/kubelet-ca.crt --dry-run=client -o yaml" \
    "$state_dir/kubectl.log"
grep -Fq -- '-n kube-system patch deployment metrics-server --type=merge --patch' \
    "$state_dir/kubectl.log"
grep -Fq -- '"hostAliases":[{"ip":"10.17.0.35","hostnames":["node-a"]}]' \
    "$state_dir/kubectl.log"
initial_ca_sha256=$(sha256sum "$work_dir/kubelet-ca.crt" | awk '{print $1}')
grep -Fq -- "\"managed-valkey.io/kubelet-ca-sha256\":\"$initial_ca_sha256\"" \
    "$state_dir/kubectl.log"
grep -Fq '  name: metrics-server-control-plane' "$state_dir/pods.yaml"
grep -Fq '  externalName: cert-manager-webhook.valkey.h3llo-demo.com' \
    "$state_dir/pods.yaml"
grep -Fq -- 'patch apiservice v1beta1.metrics.k8s.io --type=merge --patch {"spec":{"service":{"name":"metrics-server-control-plane","namespace":"kube-system","port":4443}}}' \
    "$state_dir/kubectl.log"

run_case metrics-install-rotated "$work_dir/metrics-install-rotated.log" \
    "$work_dir/kubelet-ca-rotation-bundle.crt"
[[ $case_status == 73 ]]
rotated_ca_sha256=$(sha256sum "$work_dir/kubelet-ca-rotation-bundle.crt" | awk '{print $1}')
grep -Fq -- "-n kube-system create configmap metrics-server-kubelet-ca --from-file=ca.crt=$work_dir/kubelet-ca-rotation-bundle.crt --dry-run=client -o yaml" \
    "$state_dir/kubectl.log"
grep -Fq -- "\"managed-valkey.io/kubelet-ca-sha256\":\"$rotated_ca_sha256\"" \
    "$state_dir/kubectl.log"
if grep -Fq -- "$initial_ca_sha256" "$state_dir/kubectl.log"; then
    echo "ротация CA сохранила прежнюю контрольную сумму" >&2
    exit 1
fi

run_case invalid-kubelet-ca "$work_dir/invalid-kubelet-ca.log" \
    "$work_dir/kubelet-ca-invalid.crt"
[[ $case_status == 1 ]]
grep -Fq 'набор CA kubelet должен содержать только PEM-сертификаты' \
    "$work_dir/invalid-kubelet-ca.log"
[[ ! -s $state_dir/kubectl.log ]]

run_case expiring-kubelet-ca "$work_dir/expiring-kubelet-ca.log" \
    "$work_dir/kubelet-ca-expiring.crt"
[[ $case_status == 1 ]]
grep -Fq 'CA kubelet истёк или истечёт менее чем через 30 дней' \
    "$work_dir/expiring-kubelet-ca.log"
[[ ! -s $state_dir/kubectl.log ]]

run_case metrics-deployment-unavailable "$work_dir/metrics-deployment-unavailable.log"
[[ $case_status == 1 ]]
grep -Fq 'rollout status deployment/metrics-server --timeout=180s' \
    "$state_dir/kubectl.log"
grep -Fq 'get deployment,pods -l app.kubernetes.io/instance=metrics-server -o wide' \
    "$state_dir/kubectl.log"
grep -Fq 'describe deployment metrics-server' "$state_dir/kubectl.log"
grep -Fq 'logs -l app.kubernetes.io/instance=metrics-server --all-containers=true --prefix=true --tail=200' \
    "$state_dir/kubectl.log"
grep -Fq 'describe apiservice v1beta1.metrics.k8s.io' "$state_dir/kubectl.log"
grep -Fq 'Deployment metrics-server не достиг состояния Available' \
    "$work_dir/metrics-deployment-unavailable.log"

run_case metrics-success "$work_dir/metrics-success.log"
[[ $case_status == 0 ]]
grep -Fq 'wait --for=condition=Available apiservice/v1beta1.metrics.k8s.io --timeout=180s' \
    "$state_dir/kubectl.log"
grep -Fq 'auth can-i --as=system:serviceaccount:valkey-system:managed-valkey-operator get pods.metrics.k8s.io --all-namespaces' \
    "$state_dir/kubectl.log"
grep -Fq -- '--as=system:serviceaccount:valkey-system:managed-valkey-operator -n valkey-system get pods.metrics.k8s.io operator-0' \
    "$state_dir/kubectl.log"
grep -Fq 'Kubernetes подготовлен, адрес Envoy: 203.0.113.10' \
    "$work_dir/metrics-success.log"

run_case metrics-helm-retry "$work_dir/metrics-helm-retry.log"
[[ $case_status == 0 ]]
[[ $(grep -Fc 'upgrade --install envoy-gateway oci://docker.io/envoyproxy/gateway-helm' \
    "$state_dir/helm.log") == 3 ]]
grep -Fq 'временная ошибка загрузки OCI chart, повтор через 10 с (1/4)' \
    "$work_dir/metrics-helm-retry.log"
grep -Fq 'временная ошибка загрузки OCI chart, повтор через 10 с (2/4)' \
    "$work_dir/metrics-helm-retry.log"

run_case metrics-cert-manager-helm-retry "$work_dir/metrics-cert-manager-helm-retry.log"
[[ $case_status == 0 ]]
[[ $(grep -Fc 'upgrade --install cert-manager oci://quay.io/jetstack/charts/cert-manager' \
    "$state_dir/helm.log") == 2 ]]
grep -Fq 'временная ошибка загрузки OCI chart, повтор через 10 с (1/4)' \
    "$work_dir/metrics-cert-manager-helm-retry.log"

run_case metrics-helm-network-failure "$work_dir/metrics-helm-network-failure.log"
[[ $case_status == 1 ]]
[[ $(grep -Fc 'upgrade --install envoy-gateway oci://docker.io/envoyproxy/gateway-helm' \
    "$state_dir/helm.log") == 5 ]]
grep -Fq 'временная ошибка загрузки OCI chart, повтор через 10 с (4/4)' \
    "$work_dir/metrics-helm-network-failure.log"

run_case metrics-helm-permanent-failure "$work_dir/metrics-helm-permanent-failure.log"
[[ $case_status == 1 ]]
[[ $(grep -Fc 'upgrade --install envoy-gateway oci://docker.io/envoyproxy/gateway-helm' \
    "$state_dir/helm.log") == 1 ]]
if grep -Fq 'повтор через 10 с' "$work_dir/metrics-helm-permanent-failure.log"; then
    echo "Helm повторил постоянную ошибку" >&2
    exit 1
fi

run_case metrics-api-unavailable "$work_dir/metrics-api-unavailable.log"
[[ $case_status == 1 ]]
grep -Fq 'API metrics.k8s.io не достиг состояния Available' \
    "$work_dir/metrics-api-unavailable.log"
grep -Fq 'get service,endpoints,endpointslices -l app.kubernetes.io/instance=metrics-server -o wide' \
    "$state_dir/kubectl.log"
grep -Fq 'describe apiservice v1beta1.metrics.k8s.io' "$state_dir/kubectl.log"
grep -Fq 'logs -l app.kubernetes.io/instance=metrics-server --all-containers=true --prefix=true --tail=200' \
    "$state_dir/kubectl.log"
if grep -Fq 'auth can-i --as=system:serviceaccount:valkey-system:managed-valkey-operator' \
    "$state_dir/kubectl.log"; then
    echo "проверка продолжилась при недоступном metrics.k8s.io" >&2
    exit 1
fi

run_case metrics-rbac-denied "$work_dir/metrics-rbac-denied.log"
[[ $case_status == 1 ]]
grep -Fq 'учётная запись оператора не может читать pods.metrics.k8s.io' \
    "$work_dir/metrics-rbac-denied.log"
if grep -Fq -- '--as=system:serviceaccount:valkey-system:managed-valkey-operator -n valkey-system get pods.metrics.k8s.io operator-0' \
    "$state_dir/kubectl.log"; then
    echo "получение CPU началось после отказа RBAC" >&2
    exit 1
fi

run_case metrics-timeout "$work_dir/metrics-timeout.log"
[[ $case_status == 1 ]]
grep -Fq 'metrics.k8s.io не вернул свежий неотрицательный CPU operator-0' \
    "$work_dir/metrics-timeout.log"
[[ $(grep -Fc 'get pods.metrics.k8s.io operator-0' "$state_dir/kubectl.log") == 60 ]]

run_case legacy-statefulset "$work_dir/legacy.log"
[[ $case_status == 1 ]]
grep -Fq 'обнаружены пользовательские StatefulSet прежнего оператора' "$work_dir/legacy.log"
grep -Fq 'tenant-a/valkey-a' "$work_dir/legacy.log"
if grep -Fq 'get nodes' "$state_dir/kubectl.log"; then
    echo "проверка продолжилась при наличии прежнего StatefulSet" >&2
    exit 1
fi

run_case stale-webhook-dns "$work_dir/stale-webhook-dns.log"
[[ $case_status == 1 ]]
grep -Fq 'должен содержать внутренние адреса всех рабочих нод' \
    "$work_dir/stale-webhook-dns.log"
if grep -Fq 'managed-valkey-restart-check' "$state_dir/kubectl.log"; then
    echo "проверка продолжилась при неверном DNS cert-manager-webhook" >&2
    exit 1
fi

run_case broken-node-network "$work_dir/broken-node-network.log"
[[ $case_status == 1 ]]
grep -Fq 'межнодовая сеть Cilium недоступна' "$work_dir/broken-node-network.log"
if grep -Fq 'managed-valkey-restart-check' "$state_dir/kubectl.log"; then
    echo "проверка продолжилась при недоступной межнодовой сети" >&2
    exit 1
fi

run_case wrong-exit "$work_dir/wrong-exit.log"
[[ $case_status == 1 ]]
grep -Fq 'завершился с неожиданным состоянием' "$work_dir/wrong-exit.log"
[[ -z $(find "$state_dir/active" -type f -print -quit) ]]
if grep -Fq "apply -f $repo_root/deploy/prod/namespace.yaml" "$state_dir/kubectl.log"; then
    echo "подготовка кластера продолжилась после неверного кода завершения" >&2
    exit 1
fi

run_case restarted "$work_dir/restarted.log"
[[ $case_status == 1 ]]
grep -Fq 'перезапустил контейнер' "$work_dir/restarted.log"
[[ -z $(find "$state_dir/active" -type f -print -quit) ]]

if rg -n 'ContainerRestartRules|restartPolicy: Always' "$repo_root/scripts/deploy_kubernetes.sh"; then
    echo "в сценарии осталась зависимость от ContainerRestartRules" >&2
    exit 1
fi
if ! rg -Fq 'valkeyinstances.valkey.h3llo-demo.com --subresource=status' \
    "$repo_root/scripts/deploy_kubernetes.sh"; then
    echo "проверка прав API не указывает status как дочерний ресурс" >&2
    exit 1
fi
if ! rg -q '^podDnsPolicy: Default$' "$repo_root/deploy/prod/cert-manager-values.yaml"; then
    echo "контроллер cert-manager не использует DNS рабочей ноды" >&2
    exit 1
fi
if ! rg -q '^  replicaCount: 1$' "$repo_root/deploy/prod/cert-manager-values.yaml" ||
    ! rg -q '^    minAvailable: 1$' "$repo_root/deploy/prod/cert-manager-values.yaml"; then
    echo "cert-manager-webhook не настроен для одной рабочей ноды" >&2
    exit 1
fi
if ! rg -q 'preferredDuringSchedulingIgnoredDuringExecution:' "$repo_root/deploy/prod/envoy.yaml" ||
    rg -q 'requiredDuringSchedulingIgnoredDuringExecution:' "$repo_root/deploy/prod/envoy.yaml"; then
    echo "Envoy требует разные рабочие ноды" >&2
    exit 1
fi

echo "проверка политики перезапуска Pod прошла"
