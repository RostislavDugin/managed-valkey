#!/usr/bin/env bash
set -Eeuo pipefail

repo_root=$(git rev-parse --show-toplevel)
fault_script="$repo_root/scripts/operator_network_fault.sh"
environment_script="$repo_root/scripts/test_environment.sh"
FAULT_EVENTS_FILE=${FAULT_EVENTS_FILE:-$MV_STATE_DIR/fault-events.tsv}
nested_wrapper_pid=0
nested_state=""

cleanup_nested_run() {
    if ((nested_wrapper_pid > 0)); then
        kill -TERM "$nested_wrapper_pid" 2>/dev/null || true
        wait "$nested_wrapper_pid" 2>/dev/null || true
    fi
    if [[ -n "$nested_state" && -f "$nested_state/environment.env" &&
        "$(cat "$nested_state/status" 2>/dev/null || true)" != cleaned ]]; then
        MV_DIAGNOSTIC_SCENARIO=CT-12 \
            "$environment_script" diagnostics "$nested_state" >/dev/null 2>&1 || true
        "$environment_script" cleanup "$nested_state" >/dev/null 2>&1 || true
    fi
}

if [[ "${1:-}" == child ]]; then
    mode=$2
    fault_id=$3
    "$fault_script" add "$MV_STATE_DIR" "$fault_id" k3s-agent-3 OUTPUT \
        "$K3S_AGENT_3_IP/32" 192.0.2.1/32 9
    if [[ "$mode" == error ]]; then
        exit 23
    fi
    while true; do
        sleep 1
    done
fi

if [[ "${1:-}" == full-run-child ]]; then
    operator_image=$2
    valkey_image=$3
    "$environment_script" expand-operator "$MV_STATE_DIR" "$operator_image" "$valkey_image"
    set -a
    source "$MV_STATE_DIR/environment.env"
    set +a
    fault_id=ct12-full-run-interrupt
    "$fault_script" add "$MV_STATE_DIR" "$fault_id" k3s-agent-3 OUTPUT \
        "$K3S_AGENT_3_IP/32" 192.0.2.1/32 9
    : >"$MV_STATE_DIR/ct12-full-run-ready"
    while true; do
        sleep 1
    done
fi

failure_id=ct12-error-cleanup
unavailable_kubeconfig=$(mktemp "$MV_STATE_DIR/ct12-unavailable.XXXXXX.kubeconfig")
sed -E 's#server: https://.*#server: https://192.0.2.1:6443#' \
    "$ADMIN_KUBECONFIG" >"$unavailable_kubeconfig"
chmod 0600 "$unavailable_kubeconfig"
trap 'cleanup_nested_run; rm -f "$unavailable_kubeconfig"' EXIT

check_unavailable_diagnostics() {
    local started_at elapsed

    started_at=$(date +%s)
    MANAGED_VALKEY_DIAGNOSTICS_KUBECONFIG=$unavailable_kubeconfig \
        MV_DIAGNOSTIC_SCENARIO=CT-12 \
        "$environment_script" diagnostics "$MV_STATE_DIR"
    elapsed=$(($(date +%s) - started_at))
    ((elapsed <= 15)) || {
        echo "CT-12: диагностика без Kubernetes заняла ${elapsed}s" >&2
        exit 1
    }
    rg -q '^Kubernetes API недоступен; снимок не сохранён; status=' \
        "$DIAGNOSTICS_DIR/kubernetes-unavailable.txt"
}

set +e
"$fault_script" run "$MV_STATE_DIR" -- "$0" child error "$failure_id"
failure_status=$?
set -e
[[ "$failure_status" == 23 ]] || {
    echo "CT-12: команда с ошибкой вернула $failure_status вместо 23" >&2
    exit 1
}
"$fault_script" check "$MV_STATE_DIR" "$failure_id" absent
check_unavailable_diagnostics

interrupt_id=ct12-interrupt-cleanup
"$fault_script" run "$MV_STATE_DIR" -- "$0" child wait "$interrupt_id" &
wrapper_pid=$!
fault_ready=0
for _ in $(seq 1 100); do
    if "$fault_script" check "$MV_STATE_DIR" "$interrupt_id" present >/dev/null 2>&1; then
        fault_ready=1
        break
    fi
    sleep 0.05
done
if ((fault_ready == 0)); then
    kill -TERM "$wrapper_pid" 2>/dev/null || true
    wait "$wrapper_pid" 2>/dev/null || true
    echo "CT-12: сетевое ограничение не появилось" >&2
    exit 1
fi
kill -TERM "$wrapper_pid"
set +e
wait "$wrapper_pid"
interrupt_status=$?
set -e
[[ "$interrupt_status" == 143 ]] || {
    echo "CT-12: прерванная команда вернула $interrupt_status вместо 143" >&2
    exit 1
}
"$fault_script" check "$MV_STATE_DIR" "$interrupt_id" absent
check_unavailable_diagnostics

canary=$(openssl rand -hex 24)
KUBECONFIG="$ADMIN_KUBECONFIG" kubectl -n valkey-system create secret generic \
    ct12-diagnostic-canary --from-literal=value="$canary" >/dev/null
cleanup_canary() {
    KUBECONFIG="$ADMIN_KUBECONFIG" kubectl -n valkey-system delete secret \
        ct12-diagnostic-canary --ignore-not-found >/dev/null 2>&1 || true
}
trap 'cleanup_canary; cleanup_nested_run; rm -f "$unavailable_kubeconfig"' EXIT
MV_DIAGNOSTIC_SCENARIO=CT-12 "$environment_script" diagnostics "$MV_STATE_DIR"
if rg --no-ignore --hidden -Fq -e "$canary" "$DIAGNOSTICS_DIR" ||
    rg --no-ignore --hidden -Fq -e "$K3S_TOKEN" "$DIAGNOSTICS_DIR"; then
    echo "CT-12: диагностика содержит чувствительные данные" >&2
    exit 1
fi
rg -q '^  kind: Node$' "$DIAGNOSTICS_DIR/resources.yaml"
rg -q $'\tCT-12\tnetwork-applied\t' "$FAULT_EVENTS_FILE"
rg -q $'\tCT-12\tnetwork-removed\t' "$FAULT_EVENTS_FILE"
cleanup_canary

project_snapshot() {
    local project=$1

    {
        docker ps -aq --filter "label=com.docker.compose.project=$project"
        docker network ls -q --filter "label=com.docker.compose.project=$project"
    } | sort -u
}

assert_snapshot_preserved() {
    local name=$1 before=$2 after=$3 resource

    while IFS= read -r resource; do
        [[ -z "$resource" ]] || rg -qx "$resource" <<<"$after" || {
            echo "CT-12: $name потерял ресурс $resource" >&2
            exit 1
        }
    done <<<"$before"
}

outer_project=$MANAGED_VALKEY_COMPOSE_PROJECT
outer_snapshot=$(project_snapshot "$outer_project")
outer_nodes=$(KUBECONFIG="$ADMIN_KUBECONFIG" kubectl get nodes \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.uid}{"\n"}{end}' | sort)
dev_snapshot=$(project_snapshot managed-valkey-dev)
nested_run_id="ct12-full-$(date +%s)-$$-$RANDOM"
nested_state="$repo_root/tmp/k3s/operator/$nested_run_id"
nested_ready_seconds=${MV_CT12_NESTED_READY_SECONDS:-900}
[[ "$nested_ready_seconds" =~ ^[1-9][0-9]*$ ]] || {
    echo "CT-12: MV_CT12_NESTED_READY_SECONDS должно быть положительным числом" >&2
    exit 1
}

MV_DIAGNOSTIC_SCENARIO=CT-12 \
    env -u MANAGED_VALKEY_K3S_NODES -u MANAGED_VALKEY_ENVOY_REPLICAS \
    "$environment_script" exec operator "$nested_run_id" -- \
    "$0" full-run-child "$MANAGED_VALKEY_OPERATOR_IMAGE" "$MANAGED_VALKEY_VALKEY_IMAGE" &
nested_wrapper_pid=$!

nested_ready=0
deadline=$((SECONDS + nested_ready_seconds))
while ((SECONDS < deadline)); do
    if [[ -f "$nested_state/ct12-full-run-ready" ]]; then
        nested_ready=1
        break
    fi
    if ! kill -0 "$nested_wrapper_pid" 2>/dev/null; then
        break
    fi
    sleep 0.5
done
if ((nested_ready == 0)); then
    kill -TERM "$nested_wrapper_pid" 2>/dev/null || true
    set +e
    wait "$nested_wrapper_pid"
    nested_status=$?
    set -e
    nested_wrapper_pid=0
    echo "CT-12: вложенный полный запуск не дошёл до сетевого отказа за ${nested_ready_seconds}s, status=$nested_status" >&2
    exit 1
fi

nested_environment="$nested_state/environment.env"
nested_project=$(
    source "$nested_environment"
    printf '%s' "$MANAGED_VALKEY_COMPOSE_PROJECT"
)
nested_network=$(
    source "$nested_environment"
    printf '%s' "$MANAGED_VALKEY_DOCKER_NETWORK"
)
nested_token=$(
    source "$nested_environment"
    printf '%s' "$K3S_TOKEN"
)
nested_faults=$(
    source "$nested_environment"
    printf '%s' "$NETWORK_FAULTS_FILE"
)

docker ps -q \
    --filter "label=com.docker.compose.project=$nested_project" \
    --filter label=com.docker.compose.service=k3s-agent-3 | rg -q .
"$fault_script" check "$nested_state" ct12-full-run-interrupt present

kill -TERM "$nested_wrapper_pid"
set +e
wait "$nested_wrapper_pid"
nested_status=$?
set -e
nested_wrapper_pid=0
[[ "$nested_status" == 143 ]] || {
    echo "CT-12: прерванный полный запуск вернул $nested_status вместо 143" >&2
    exit 1
}
[[ "$(cat "$nested_state/status")" == cleaned ]] || {
    echo "CT-12: прерванный полный запуск не очистил окружение" >&2
    exit 1
}
[[ ! -s "$nested_faults" ]] || {
    echo "CT-12: прерванный полный запуск сохранил сетевое ограничение" >&2
    exit 1
}
[[ -s "$nested_state/diagnostics/metadata.tsv" ]]
rg -q $'\tCT-12$' "$nested_state/diagnostics/metadata.tsv"
if rg --no-ignore --hidden -Fq -e "$nested_token" "$nested_state/diagnostics"; then
    echo "CT-12: диагностика прерванного запуска содержит токен k3s" >&2
    exit 1
fi
if docker ps -aq --filter "label=com.docker.compose.project=$nested_project" | rg -q . ||
    docker network inspect "$nested_network" >/dev/null 2>&1; then
    echo "CT-12: прерванный полный запуск не удалил четвёртый agent или сеть" >&2
    exit 1
fi

assert_snapshot_preserved \
    "соседний запуск оператора" "$outer_snapshot" "$(project_snapshot "$outer_project")"
[[ "$outer_nodes" == "$(KUBECONFIG="$ADMIN_KUBECONFIG" kubectl get nodes \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.uid}{"\n"}{end}' | sort)" ]] || {
    echo "CT-12: изменились ноды соседнего запуска оператора" >&2
    exit 1
}
assert_snapshot_preserved \
    "dev" "$dev_snapshot" "$(project_snapshot managed-valkey-dev)"

echo "CT-12: прерванный полный запуск сохранил диагностику и соседние ресурсы, четвёртый agent удалён"

echo "CT-12: сетевые ограничения сняты после ошибки и прерывания"
