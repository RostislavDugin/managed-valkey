#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
expected_nodes=${1:?задайте ожидаемое число нод}
expected_envoy_replicas=${2:-${MANAGED_VALKEY_ENVOY_REPLICAS:-2}}

if [[ ! "$expected_nodes" =~ ^[1-4]$ ]]; then
    echo "ожидаемое число нод должно быть от 1 до 4" >&2
    exit 1
fi
if [[ "$expected_envoy_replicas" != 1 && "$expected_envoy_replicas" != 2 ]]; then
    echo "ожидаемое число реплик Envoy должно быть 1 или 2" >&2
    exit 1
fi

mapfile -t dev_nodes < <(
    docker compose -f "$repo_root/docker-compose.dev.yml" config --services | rg '^k3s-'
)
if ((${#dev_nodes[@]} != 3)) || printf '%s\n' "${dev_nodes[@]}" | rg -qx k3s-agent-3; then
    echo "обычный Compose должен содержать три k3s-ноды" >&2
    exit 1
fi

if K3S_DOCKER_GATEWAY= docker compose \
    -f "$repo_root/docker-compose.dev.yml" \
    -f "$repo_root/docker-compose.test.yml" \
    -p "$MANAGED_VALKEY_COMPOSE_PROJECT" config >/dev/null 2>&1; then
    echo "тестовый Compose принял пустой K3S_DOCKER_GATEWAY" >&2
    exit 1
fi

actual_gateway=$(docker network inspect "$MANAGED_VALKEY_DOCKER_NETWORK" \
    --format '{{(index .IPAM.Config 0).Gateway}}')
if [[ "$actual_gateway" != "$K3S_DOCKER_GATEWAY" ]]; then
    echo "gateway Compose не совпал с окружением" >&2
    exit 1
fi

if ! rg -qx "VALKEY_OPERATOR_CIDRS=${K3S_DOCKER_GATEWAY}/32" "$OPERATOR_ENV"; then
    echo "operator.env не содержит gateway Compose" >&2
    exit 1
fi

node_count=$(KUBECONFIG="$ADMIN_KUBECONFIG" kubectl get nodes -o name | wc -l)
if ((node_count != expected_nodes)); then
    echo "ожидалось нод: $expected_nodes, получено: $node_count" >&2
    exit 1
fi
KUBECONFIG="$ADMIN_KUBECONFIG" kubectl wait \
    --for=condition=Ready node --all --timeout=60s >/dev/null

assert_k3s_argument() {
    local service=$1 expected=$2 container arguments

    container=$(docker ps -q \
        --filter "label=com.docker.compose.project=$MANAGED_VALKEY_COMPOSE_PROJECT" \
        --filter "label=com.docker.compose.service=$service" | head -n 1)
    if [[ -z "$container" ]]; then
        echo "не найден контейнер $service" >&2
        exit 1
    fi
    arguments=$(docker inspect "$container" --format '{{range .Args}}{{println .}}{{end}}')
    if ! rg -Fxq -- "$expected" <<<"$arguments"; then
        echo "$service запущен без $expected" >&2
        exit 1
    fi
}

assert_k3s_argument k3s-server \
    --kube-apiserver-arg=feature-gates=ContainerRestartRules=false
for ((node_index = 0; node_index < expected_nodes; node_index++)); do
    if ((node_index == 0)); then
        node_service=k3s-server
    else
        node_service="k3s-agent-$node_index"
    fi
    assert_k3s_argument "$node_service" \
        --kubelet-arg=feature-gates=ContainerRestartRules=false
done

mapfile -t envoy_nodes < <(
    KUBECONFIG="$ADMIN_KUBECONFIG" kubectl -n envoy-gateway-system get pods \
        -l gateway.envoyproxy.io/owning-gateway-name=valkey \
        --field-selector=status.phase=Running \
        -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}'
)
if ((${#envoy_nodes[@]} != expected_envoy_replicas)); then
    echo "ожидалось реплик Envoy: $expected_envoy_replicas, получено: ${#envoy_nodes[@]}" >&2
    exit 1
fi
if ((expected_envoy_replicas == 2)) && [[ "${envoy_nodes[0]}" == "${envoy_nodes[1]}" ]]; then
    echo "две реплики Envoy размещены на одной ноде ${envoy_nodes[0]}" >&2
    exit 1
fi

python3 - \
    "$K3S_DOCKER_SUBNET" \
    "$K3S_DOCKER_GATEWAY" \
    "$K3S_SERVER_IP" \
    "$K3S_AGENT_1_IP" \
    "$K3S_AGENT_2_IP" \
    "$K3S_AGENT_3_IP" \
    "$MANAGED_VALKEY_CLIENT_ALLOWED" \
    "$MANAGED_VALKEY_CLIENT_BLOCKED" \
    "$MANAGED_VALKEY_CLIENT_WRONG_SNI" \
    "$MANAGED_VALKEY_CLIENT_PRE_READY" <<'PY'
import ipaddress
import sys

network = ipaddress.ip_network(sys.argv[1])
addresses = [ipaddress.ip_address(value) for value in sys.argv[2:]]
if len(addresses) != len(set(addresses)):
    raise SystemExit("адреса нод, клиентов и gateway пересекаются")
if any(address not in network for address in addresses):
    raise SystemExit("адрес тестового стенда находится вне подсети Compose")
PY

echo "CT-13 CT-14: стенд оператора проверен: ноды=$expected_nodes, Envoy=$expected_envoy_replicas"
