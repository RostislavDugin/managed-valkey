#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
expected_nodes=${1:?задайте ожидаемое число нод}

if [[ "$expected_nodes" != 3 && "$expected_nodes" != 4 ]]; then
    echo "ожидаемое число нод должно быть 3 или 4" >&2
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

echo "CT-13 CT-14: стенд оператора на $expected_nodes нодах проверен"
