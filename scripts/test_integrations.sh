#!/usr/bin/env bash
set -Eeuo pipefail

repo_root=$(git rev-parse --show-toplevel)
diagnostics_dir=${DIAGNOSTICS_DIR:?DIAGNOSTICS_DIR не задан}
state_dir=${MV_STATE_DIR:?MV_STATE_DIR не задан}
child_pids=()
children_stopped=0
script_started_seconds=$SECONDS
timings_file="$diagnostics_dir/durations.tsv"

free_port() {
    python3 - <<'PY'
import socket

with socket.socket() as listener:
    listener.bind(("127.0.0.1", 0))
    print(listener.getsockname()[1])
PY
}

process_group_active() {
    local pid=$1

    ps -eo pgid=,stat= | awk -v group="$pid" '
        $1 == group && $2 !~ /^Z/ { active = 1 }
        END { exit !active }
    '
}

stop_children() {
    local deadline grace_seconds=${MV_INTEGRATION_STOP_GRACE_SECONDS:-30} pid stop_status=0

    ((children_stopped == 0)) || return 0
    children_stopped=1
    [[ "$grace_seconds" =~ ^[0-9]+$ ]] || {
        echo "integration: MV_INTEGRATION_STOP_GRACE_SECONDS должно быть целым неотрицательным числом" >&2
        return 1
    }
    for pid in "${child_pids[@]}"; do
        kill -TERM -- "-$pid" 2>/dev/null || true
    done
    deadline=$((SECONDS + grace_seconds))
    for pid in "${child_pids[@]}"; do
        while process_group_active "$pid"; do
            if ((SECONDS >= deadline)); then
                kill -KILL -- "-$pid" 2>/dev/null || true
                stop_status=1
                break
            fi
            sleep 0.1
        done
        wait "$pid" 2>/dev/null || true
    done

    return "$stop_status"
}

finish() {
    local status=$? stop_status=0

    trap - EXIT INT TERM
    stop_children || stop_status=$?
    if ((status == 0 && stop_status != 0)); then
        status=$stop_status
    fi
    exit "$status"
}

wait_http() {
    local name=$1 url=$2 deadline=$((SECONDS + ${MV_INTEGRATION_READY_TIMEOUT_SECONDS:-120})) pid

    while ((SECONDS < deadline)); do
        if curl --fail --silent --show-error "$url" >/dev/null 2>&1; then
            return 0
        fi
        for pid in "${child_pids[@]}"; do
            if ! process_group_active "$pid"; then
                echo "integration: процесс завершился до готовности $name" >&2
                return 1
            fi
        done
        sleep 0.2
    done
    echo "integration: $name не достиг готовности" >&2
    return 1
}

record_duration() {
    local name=$1 started_seconds=$2

    printf '%s\t%d\n' "$name" "$((SECONDS - started_seconds))" >>"$timings_file"
}

record_process() {
    local pid=$1 name=$2 process_start process_boot

    process_boot=$(tr -d '\n' </proc/sys/kernel/random/boot_id)
    if process_start=$("$repo_root/scripts/test_resources.sh" start-ticks "$pid"); then
        printf '%s\t%s\t%s\t%s\n' "$pid" "$name" "$process_start" "$process_boot" \
            >>"$state_dir/processes.tsv"
    else
        printf '%s\t%s\n' "$pid" "$name" >>"$state_dir/processes.tsv"
    fi
}

mkdir -p "$diagnostics_dir"
chmod 0700 "$diagnostics_dir"
: >"$timings_file"
chmod 0600 "$timings_file"
secret_values_file="$state_dir/integration-secret-values"
: >"$secret_values_file"
chmod 0600 "$secret_values_file"

trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

migration_started_seconds=$SECONDS
cd "$repo_root"
go tool goose -allow-missing -dir migrations postgres \
    "${TEST_DATABASE_URL:?TEST_DATABASE_URL не задан}" up
record_duration migrations "$migration_started_seconds"

api_port=$(free_port)
operator_port=$(free_port)
while [[ "$operator_port" == "$api_port" ]]; do
    operator_port=$(free_port)
done
jwt_secret=$(openssl rand -hex 32)
printf '%s\n' "$jwt_secret" >>"$secret_values_file"

set -a
source "${OPERATOR_ENV:?OPERATOR_ENV не задан}"
set +a

processes_started_seconds=$SECONDS
setsid env \
    APP_ENV=test \
    DATABASE_URL="$TEST_DATABASE_URL" \
    HTTP_ADDR="127.0.0.1:$api_port" \
    JWT_SECRET="$jwt_secret" \
    KUBECONFIG="${API_KUBECONFIG:?API_KUBECONFIG не задан}" \
    LOG_LEVEL=error \
    MANAGED_K8S_NODE_RAM_GB="${MANAGED_K8S_NODE_RAM_GB:-8}" \
    MANAGED_K8S_NODE_VCPU="${MANAGED_K8S_NODE_VCPU:-4}" \
    VALKEY_BASE_DOMAIN="${VALKEY_BASE_DOMAIN:?VALKEY_BASE_DOMAIN не задан}" \
    VALKEY_INSTANCE_MAX_RAM_GB="${VALKEY_INSTANCE_MAX_RAM_GB:-16}" \
    VALKEY_INSTANCE_MAX_VCPU="${VALKEY_INSTANCE_MAX_VCPU:-4}" \
    VALKEY_PUBLIC_PORT=41379 \
    go run ./api/cmd/api >"$diagnostics_dir/api.log" 2>&1 &
api_pid=$!
child_pids+=("$api_pid")
record_process "$api_pid" api

setsid env \
    APP_ENV=test \
    KUBECONFIG="${OPERATOR_KUBECONFIG:?OPERATOR_KUBECONFIG не задан}" \
    LOG_LEVEL=error \
    OPERATOR_PROBE_ADDR="127.0.0.1:$operator_port" \
    VALKEY_BASE_DOMAIN="$VALKEY_BASE_DOMAIN" \
    VALKEY_ENVOY_PROCESSES=1 \
    VALKEY_OPERATOR_CIDRS="${VALKEY_OPERATOR_CIDRS:?VALKEY_OPERATOR_CIDRS не задан}" \
    go run -tags integration ./operator/cmd/operator >"$diagnostics_dir/operator.log" 2>&1 &
operator_pid=$!
child_pids+=("$operator_pid")
record_process "$operator_pid" operator

wait_http API "http://127.0.0.1:$api_port/readyz"
wait_http operator "http://127.0.0.1:$operator_port/readyz"
record_duration processes-ready "$processes_started_seconds"

tests_started_seconds=$SECONDS
set +e
MANAGED_VALKEY_INTEGRATION_API_URL="http://127.0.0.1:$api_port" \
    MANAGED_VALKEY_INTEGRATION_ADMIN_KUBECONFIG="${ADMIN_KUBECONFIG:?ADMIN_KUBECONFIG не задан}" \
    MANAGED_VALKEY_INTEGRATION_API_PID="$api_pid" \
    MANAGED_VALKEY_INTEGRATION_CA_FILE="${MANAGED_VALKEY_CA_FILE:?MANAGED_VALKEY_CA_FILE не задан}" \
    MANAGED_VALKEY_INTEGRATION_DIAGNOSTICS_DIR="$diagnostics_dir" \
    MANAGED_VALKEY_INTEGRATION_OPERATOR_PID="$operator_pid" \
    MANAGED_VALKEY_INTEGRATION_PUBLIC_ADDRESS="${MANAGED_VALKEY_PUBLIC_ADDRESS:?MANAGED_VALKEY_PUBLIC_ADDRESS не задан}" \
    MANAGED_VALKEY_INTEGRATION_RUN_ID="${MV_RUN_ID:?MV_RUN_ID не задан}" \
    MANAGED_VALKEY_INTEGRATION_SECRET_VALUES_FILE="$secret_values_file" \
    go test -tags integration -count=1 -parallel=4 -timeout=30m ./tests/integrations
test_status=$?
set -e
record_duration scenarios "$tests_started_seconds"
record_duration script "$script_started_seconds"
exit "$test_status"
