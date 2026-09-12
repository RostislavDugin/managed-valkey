#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 3 ]]; then
    echo "использование: $0 <sha> <административный-kubeconfig> <kubeconfig-api>" >&2
    exit 2
fi

release_sha=$1
admin_kubeconfig=$2
api_kubeconfig=$3
repo_root=$(git rev-parse --show-toplevel)
operator_image="ghcr.io/rostislavdugin/managed-valkey-operator:$release_sha"
operator_placeholder=ghcr.io/rostislavdugin/managed-valkey-operator:0000000000000000000000000000000000000000
gateway_api_version=v1.5.1
envoy_gateway_version=v1.8.4
cert_manager_version=v1.21.1
metrics_server_chart_version=3.14.0
metrics_server_version=v0.9.0
metrics_max_age_seconds=60
metrics_server_values="$repo_root/deploy/prod/metrics-server-values.yaml"
metrics_server_kubelet_ca="$repo_root/deploy/prod/kubelet-ca.crt"
cert_manager_webhook_host=cert-manager-webhook.valkey.h3llo-demo.com
cloudflare_token_file=""
restart_check_pods=()

cleanup() {
    local pod

    for pod in "${restart_check_pods[@]}"; do
        kubectl -n default delete pod "$pod" --ignore-not-found --wait=true --timeout=60s >/dev/null 2>&1 || true
    done
    if [[ -n $cloudflare_token_file ]]; then
        rm -f -- "$cloudflare_token_file"
    fi
}

trap cleanup EXIT

if [[ ! $release_sha =~ ^[0-9a-f]{40}$ ]]; then
    echo "SHA должен состоять из 40 строчных шестнадцатеричных символов" >&2
    exit 2
fi
if [[ ! -s $admin_kubeconfig ]]; then
    echo "административный kubeconfig отсутствует или пуст" >&2
    exit 1
fi
if [[ -z ${CLOUDFLARE_API_TOKEN:-} ]]; then
    echo "не задан CLOUDFLARE_API_TOKEN" >&2
    exit 1
fi

for command in kubectl helm docker sed base64 getent awk sort date jq; do
    if ! command -v "$command" >/dev/null 2>&1; then
        echo "не установлена команда $command" >&2
        exit 1
    fi
done

export KUBECONFIG=$admin_kubeconfig
kubectl get --raw=/readyz >/dev/null
docker manifest inspect "$operator_image" >/dev/null

legacy_statefulsets=$(kubectl get statefulsets.apps --all-namespaces \
    -l app.kubernetes.io/name=valkey \
    -o jsonpath='{range .items[*]}{.metadata.namespace}{"/"}{.metadata.name}{"\n"}{end}')
if [[ -n $legacy_statefulsets ]]; then
    echo "обнаружены пользовательские StatefulSet прежнего оператора:" >&2
    printf '%s\n' "$legacy_statefulsets" >&2
    exit 1
fi

mapfile -t worker_nodes < <(
    kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'
)
if ((${#worker_nodes[@]} == 0)); then
    echo "в кластере нет рабочих нод" >&2
    exit 1
fi

node_internal_ips=$(kubectl get nodes \
    -o jsonpath='{range .items[*].status.addresses[?(@.type=="InternalIP")]}{.address}{"\n"}{end}' |
    sort -u)
webhook_dns_ips=$(getent ahostsv4 "$cert_manager_webhook_host" 2>/dev/null |
    awk '{print $1}' | sort -u || true)
if [[ -z $node_internal_ips || $webhook_dns_ips != "$node_internal_ips" ]]; then
    echo "DNS $cert_manager_webhook_host должен содержать внутренние адреса всех рабочих нод" >&2
    exit 1
fi

mapfile -t cilium_pods < <(
    kubectl -n kube-system get pods \
        -l k8s-app=cilium \
        --field-selector=status.phase=Running \
        -o name
)
if ((${#cilium_pods[@]} != ${#worker_nodes[@]})); then
    echo "число работающих агентов Cilium не совпадает с числом рабочих нод" >&2
    exit 1
fi
expected_reachability="${#worker_nodes[@]}/${#worker_nodes[@]} reachable"
for cilium_pod in "${cilium_pods[@]}"; do
    cilium_health=$(kubectl -n kube-system exec "$cilium_pod" -- \
        cilium-health status --succinct 2>/dev/null || true)
    if [[ $cilium_health != *"Cluster health:"*"$expected_reachability"* ]]; then
        echo "межнодовая сеть Cilium недоступна из $cilium_pod" >&2
        exit 1
    fi
done

restart_check_index=0
for node in "${worker_nodes[@]}"; do
    for expected_exit_code in 0 42; do
        restart_check_pod="managed-valkey-restart-check-${release_sha:0:8}-$restart_check_index-$expected_exit_code"
        restart_check_pods+=("$restart_check_pod")
        kubectl -n default delete pod "$restart_check_pod" \
            --ignore-not-found --wait=true --timeout=60s >/dev/null
        kubectl apply -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $restart_check_pod
  namespace: default
spec:
  nodeName: $node
  automountServiceAccountToken: false
  restartPolicy: Never
  containers:
    - name: check
      image: valkey/valkey:8.1.9
      command: ["/bin/sh", "-c", "exit $expected_exit_code"]
      resources:
        requests:
          cpu: 10m
          memory: 16Mi
        limits:
          memory: 64Mi
YAML
        if [[ $expected_exit_code == 0 ]]; then
            expected_phase=Succeeded
            expected_reason=Completed
        else
            expected_phase=Failed
            expected_reason=Error
        fi

        restart_check_completed=false
        for _ in {1..90}; do
            restart_count=$(kubectl -n default get pod "$restart_check_pod" \
                -o jsonpath='{.status.containerStatuses[0].restartCount}' 2>/dev/null || true)
            if [[ -n $restart_count && $restart_count != 0 ]]; then
                echo "Pod $restart_check_pod перезапустил контейнер на ноде $node" >&2
                exit 1
            fi
            restart_check_phase=$(kubectl -n default get pod "$restart_check_pod" \
                -o jsonpath='{.status.phase}' 2>/dev/null || true)
            if [[ $restart_check_phase == Succeeded || $restart_check_phase == Failed ]]; then
                restart_check_exit_code=$(kubectl -n default get pod "$restart_check_pod" \
                    -o jsonpath='{.status.containerStatuses[0].state.terminated.exitCode}' 2>/dev/null || true)
                restart_check_reason=$(kubectl -n default get pod "$restart_check_pod" \
                    -o jsonpath='{.status.containerStatuses[0].state.terminated.reason}' 2>/dev/null || true)
                if [[ $restart_check_phase != "$expected_phase" ||
                    $restart_check_exit_code != "$expected_exit_code" ||
                    $restart_check_reason != "$expected_reason" ]]; then
                    echo "Pod $restart_check_pod завершился с неожиданным состоянием на ноде $node" >&2
                    exit 1
                fi
                restart_check_completed=true
                break
            fi
            sleep 2
        done
        if [[ $restart_check_completed != true ]]; then
            echo "Pod $restart_check_pod не завершился на ноде $node" >&2
            exit 1
        fi

        sleep 3
        restart_check_phase=$(kubectl -n default get pod "$restart_check_pod" \
            -o jsonpath='{.status.phase}')
        restart_count=$(kubectl -n default get pod "$restart_check_pod" \
            -o jsonpath='{.status.containerStatuses[0].restartCount}')
        restart_check_exit_code=$(kubectl -n default get pod "$restart_check_pod" \
            -o jsonpath='{.status.containerStatuses[0].state.terminated.exitCode}')
        restart_check_reason=$(kubectl -n default get pod "$restart_check_pod" \
            -o jsonpath='{.status.containerStatuses[0].state.terminated.reason}')
        if [[ $restart_check_phase != "$expected_phase" || $restart_count != 0 ||
            $restart_check_exit_code != "$expected_exit_code" ||
            $restart_check_reason != "$expected_reason" ]]; then
            echo "Pod $restart_check_pod изменил состояние после завершения на ноде $node" >&2
            exit 1
        fi
        kubectl -n default delete pod "$restart_check_pod" --wait=true --timeout=60s >/dev/null
    done
    restart_check_index=$((restart_check_index + 1))
done

kubectl apply -f "$repo_root/deploy/prod/namespace.yaml"
kubectl wait --for=jsonpath='{.status.phase}'=Active namespace/valkey-system --timeout=60s

kubectl -n kube-system create configmap metrics-server-kubelet-ca \
    --from-file="ca.crt=$metrics_server_kubelet_ca" \
    --dry-run=client \
    -o yaml | kubectl apply -f -
helm repo add metrics-server https://kubernetes-sigs.github.io/metrics-server/ --force-update
helm upgrade --install metrics-server metrics-server/metrics-server \
    --version "$metrics_server_chart_version" \
    --namespace kube-system \
    --set replicas=1 \
    --set-string image.tag="$metrics_server_version" \
    --values "$metrics_server_values"
metrics_server_host_aliases=$(kubectl get nodes -o json | jq -c '
    [.items[] |
        {
            ip: ([.status.addresses[] | select(.type == "InternalIP") | .address][0]),
            hostnames: [.metadata.name]
        } |
        select(.ip != null)
    ]
')
if [[ $(jq length <<<"$metrics_server_host_aliases") -ne ${#worker_nodes[@]} ]]; then
    echo "не для всех рабочих нод найден InternalIP" >&2
    exit 1
fi
metrics_server_patch=$(jq -cn \
    --argjson host_aliases "$metrics_server_host_aliases" \
    '{spec: {template: {spec: {hostAliases: $host_aliases}}}}')
kubectl -n kube-system patch deployment metrics-server \
    --type=merge \
    --patch "$metrics_server_patch"
if ! kubectl -n kube-system rollout status deployment/metrics-server --timeout=180s; then
    kubectl -n kube-system get deployment,pods \
        -l app.kubernetes.io/instance=metrics-server \
        -o wide >&2 || true
    kubectl -n kube-system describe deployment metrics-server >&2 || true
    kubectl -n kube-system logs deployment/metrics-server \
        --all-containers=true \
        --tail=200 >&2 || true
    kubectl describe apiservice v1beta1.metrics.k8s.io >&2 || true
    echo "Deployment metrics-server не достиг состояния Available" >&2
    exit 1
fi
if ! kubectl wait --for=condition=Available \
    apiservice/v1beta1.metrics.k8s.io \
    --timeout=180s; then
    kubectl -n kube-system get service,endpoints,endpointslices \
        -l app.kubernetes.io/instance=metrics-server \
        -o wide >&2 || true
    kubectl describe apiservice v1beta1.metrics.k8s.io >&2 || true
    kubectl -n kube-system logs \
        -l app.kubernetes.io/instance=metrics-server \
        --all-containers=true \
        --prefix=true \
        --tail=200 >&2 || true
    echo "API metrics.k8s.io не достиг состояния Available" >&2
    exit 1
fi

kubectl apply --server-side --force-conflicts -f \
    "https://github.com/kubernetes-sigs/gateway-api/releases/download/${gateway_api_version}/experimental-install.yaml"

helm upgrade --install envoy-gateway oci://docker.io/envoyproxy/gateway-helm \
    --version "$envoy_gateway_version" \
    --namespace envoy-gateway-system \
    --create-namespace \
    --values "$repo_root/deploy/prod/envoy-gateway-values.yaml" \
    --wait \
    --timeout 10m

helm upgrade --install cert-manager oci://quay.io/jetstack/charts/cert-manager \
    --version "$cert_manager_version" \
    --namespace cert-manager \
    --create-namespace \
    --values "$repo_root/deploy/prod/cert-manager-values.yaml" \
    --wait \
    --timeout 10m

cloudflare_token_file=$(mktemp)
chmod 0600 "$cloudflare_token_file"
printf '%s' "$CLOUDFLARE_API_TOKEN" >"$cloudflare_token_file"
kubectl -n cert-manager create secret generic cloudflare-api-token \
    --from-file="api-token=$cloudflare_token_file" \
    --dry-run=client \
    -o yaml | kubectl apply -f -

kubectl apply --server-side --force-conflicts -k "$repo_root/operator/config/crd"
kubectl wait --for=condition=Established \
    crd/valkeyinstances.valkey.h3llo-demo.com \
    --timeout=120s
kubectl apply -f "$repo_root/operator/config/rbac/role.yaml"

current_operator_image=$(kubectl -n valkey-system get pod operator-0 \
    -o jsonpath='{.spec.containers[?(@.name=="operator")].image}' 2>/dev/null || true)
kubectl kustomize "$repo_root/deploy/prod" |
    sed "s|$operator_placeholder|$operator_image|g" |
    kubectl apply --server-side --force-conflicts -f -

if [[ -n $current_operator_image && $current_operator_image != "$operator_image" ]]; then
    kubectl -n valkey-system delete pod operator-0 --wait=true
fi
kubectl -n valkey-system wait --for=condition=Ready pod/operator-0 --timeout=300s
running_operator_image=$(kubectl -n valkey-system get pod operator-0 \
    -o jsonpath='{.spec.containers[?(@.name=="operator")].image}')
if [[ $running_operator_image != "$operator_image" ]]; then
    echo "оператор запущен с образом $running_operator_image вместо $operator_image" >&2
    exit 1
fi

operator_identity=system:serviceaccount:valkey-system:managed-valkey-operator
if [[ $(kubectl auth can-i \
    --as="$operator_identity" \
    get pods.metrics.k8s.io \
    --all-namespaces) != yes ]]; then
    echo "учётная запись оператора не может читать pods.metrics.k8s.io" >&2
    exit 1
fi

operator_cpu_ready=false
for _ in {1..60}; do
    operator_metrics=$(kubectl \
        --as="$operator_identity" \
        -n valkey-system \
        get pods.metrics.k8s.io operator-0 \
        -o jsonpath='{.timestamp}{"\t"}{.containers[?(@.name=="operator")].usage.cpu}{"\n"}' \
        2>/dev/null || true)
    IFS=$'\t' read -r metric_timestamp metric_cpu <<<"$operator_metrics"
    metric_epoch=$(date -d "$metric_timestamp" +%s 2>/dev/null || true)
    now_epoch=$(date +%s)
    if [[ -n $metric_epoch &&
        $metric_cpu =~ ^[0-9]+([.][0-9]+)?([numkKMGTPE]|[EPTGMK]i|[eE][+-]?[0-9]+)?$ &&
        $metric_epoch -le $now_epoch &&
        $((now_epoch - metric_epoch)) -le $metrics_max_age_seconds ]]; then
        operator_cpu_ready=true
        break
    fi
    sleep 2
done
if [[ $operator_cpu_ready != true ]]; then
    echo "metrics.k8s.io не вернул свежий неотрицательный CPU operator-0" >&2
    exit 1
fi

api_token=""
for _ in {1..60}; do
    api_token=$(kubectl -n valkey-system get secret managed-valkey-api-token \
        -o go-template='{{index .data "token" | base64decode}}' 2>/dev/null || true)
    [[ -z $api_token ]] || break
    sleep 2
done
if [[ -z $api_token ]]; then
    echo "Kubernetes не выдал токен учётной записи API" >&2
    exit 1
fi

kubernetes_server=$(kubectl config view --raw --minify --flatten \
    -o jsonpath='{.clusters[0].cluster.server}')
certificate_authority=$(kubectl config view --raw --minify --flatten \
    -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
if [[ -z $kubernetes_server || -z $certificate_authority ]]; then
    echo "административный kubeconfig не содержит адрес или сертификат центра сертификации" >&2
    exit 1
fi

umask 077
mkdir -p "$(dirname "$api_kubeconfig")"
cat >"$api_kubeconfig" <<YAML
apiVersion: v1
kind: Config
clusters:
  - name: production
    cluster:
      server: $kubernetes_server
      certificate-authority-data: $certificate_authority
users:
  - name: managed-valkey-api
    user:
      token: $api_token
contexts:
  - name: production
    context:
      cluster: production
      user: managed-valkey-api
      namespace: valkey-system
current-context: production
YAML
chmod 0600 "$api_kubeconfig"

if [[ $(kubectl --kubeconfig "$api_kubeconfig" auth can-i create namespaces) != yes ]] ||
    [[ $(kubectl --kubeconfig "$api_kubeconfig" auth can-i update valkeyinstances.valkey.h3llo-demo.com) != yes ]] ||
    [[ $(kubectl --kubeconfig "$api_kubeconfig" auth can-i update \
        valkeyinstances.valkey.h3llo-demo.com --subresource=status) != no ]]; then
    echo "права учётной записи API не соответствуют ожидаемым" >&2
    exit 1
fi
kubectl --kubeconfig "$api_kubeconfig" get namespace valkey-system >/dev/null

kubectl -n valkey-system wait --for=condition=Ready certificate/valkey-wildcard --timeout=10m
kubectl -n valkey-system wait --for=condition=Programmed gateway/valkey --timeout=10m

gateway_address=""
for _ in {1..120}; do
    gateway_address=$(kubectl -n envoy-gateway-system get service \
        -l gateway.envoyproxy.io/owning-gateway-name=valkey \
        -o jsonpath='{.items[0].status.loadBalancer.ingress[0].ip}' 2>/dev/null || true)
    if [[ -z $gateway_address ]]; then
        gateway_address=$(kubectl -n envoy-gateway-system get service \
            -l gateway.envoyproxy.io/owning-gateway-name=valkey \
            -o jsonpath='{.items[0].status.loadBalancer.ingress[0].hostname}' 2>/dev/null || true)
    fi
    [[ -z $gateway_address ]] || break
    sleep 5
done
if [[ -z $gateway_address ]]; then
    echo "сервис Envoy не получил внешний адрес" >&2
    exit 1
fi

gateway_ips=$(getent ahostsv4 "$gateway_address" 2>/dev/null | awk '{print $1}' | sort -u || true)
if [[ -z $gateway_ips && $gateway_address =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    gateway_ips=$gateway_address
fi
dns_ips=$(getent ahostsv4 probe.valkey.h3llo-demo.com 2>/dev/null | awk '{print $1}' | sort -u || true)
dns_matches=false
while IFS= read -r dns_ip; do
    [[ -n $dns_ip ]] || continue
    while IFS= read -r gateway_ip; do
        if [[ -n $gateway_ip && $dns_ip == "$gateway_ip" ]]; then
            dns_matches=true
        fi
    done <<<"$gateway_ips"
done <<<"$dns_ips"
if [[ $dns_matches != true ]]; then
    echo "DNS *.valkey.h3llo-demo.com не указывает на адрес Envoy $gateway_address" >&2
    exit 1
fi

echo "Kubernetes подготовлен, адрес Envoy: $gateway_address"
