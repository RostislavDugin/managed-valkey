#!/usr/bin/env bash
set -Eeuo pipefail

repo_root=$(git rev-parse --show-toplevel)
child_pids=()
diagnostics_dir=${DIAGNOSTICS_DIR:?DIAGNOSTICS_DIR не задан}

free_port() {
    python3 - <<'PY'
import socket

with socket.socket() as listener:
    listener.bind(("127.0.0.1", 0))
    print(listener.getsockname()[1])
PY
}

stop_children() {
    local pid

    trap - EXIT INT TERM
    for pid in "${child_pids[@]}"; do
        kill -TERM -- "-$pid" 2>/dev/null || true
    done
    for pid in "${child_pids[@]}"; do
        wait "$pid" 2>/dev/null || true
    done
}

wait_http() {
    local name=$1 url=$2 attempt pid

    for attempt in {1..120}; do
        if curl --fail --silent --show-error "$url" >/dev/null 2>&1; then
            return 0
        fi
        for pid in "${child_pids[@]}"; do
            kill -0 "$pid" 2>/dev/null || {
                echo "e2e: процесс завершился до готовности $name" >&2
                return 1
            }
        done
        sleep 1
    done
    echo "e2e: $name не достиг готовности" >&2
    return 1
}

mkdir -p "$diagnostics_dir"
api_port=$(free_port)
web_port=$(free_port)
while [[ "$web_port" == "$api_port" ]]; do
    web_port=$(free_port)
done
jwt_secret=$(openssl rand -hex 32)
valkey_public_port=${MANAGED_VALKEY_PUBLIC_ADDRESS##*:}

trap stop_children EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

cd "$repo_root"
go tool goose -allow-missing -dir migrations postgres \
    "${TEST_DATABASE_URL:?TEST_DATABASE_URL не задан}" up

setsid env \
    APP_ENV=test \
    DATABASE_URL="$TEST_DATABASE_URL" \
    HTTP_ADDR="127.0.0.1:$api_port" \
    JWT_SECRET="$jwt_secret" \
    KUBECONFIG="${API_KUBECONFIG:?API_KUBECONFIG не задан}" \
    LOG_LEVEL=error \
    MANAGED_K8S_NODE_RAM_GB="${MANAGED_K8S_NODE_RAM_GB:-16}" \
    MANAGED_K8S_NODE_VCPU="${MANAGED_K8S_NODE_VCPU:-4}" \
    VALKEY_BASE_DOMAIN="${VALKEY_BASE_DOMAIN:?VALKEY_BASE_DOMAIN не задан}" \
    VALKEY_INSTANCE_MAX_RAM_GB="${VALKEY_INSTANCE_MAX_RAM_GB:-16}" \
    VALKEY_INSTANCE_MAX_VCPU="${VALKEY_INSTANCE_MAX_VCPU:-4}" \
    VALKEY_PUBLIC_PORT="$valkey_public_port" \
    go run ./api/cmd/api >"$diagnostics_dir/api.log" 2>&1 &
child_pids+=("$!")
wait_http API "http://127.0.0.1:$api_port/readyz"

setsid env API_PROXY_TARGET="http://127.0.0.1:$api_port" \
    pnpm --dir "$repo_root/web" dev --host 127.0.0.1 --port "$web_port" --strictPort \
    >"$diagnostics_dir/web.log" 2>&1 &
child_pids+=("$!")
wait_http frontend "http://127.0.0.1:$web_port/auth"

if [[ "${MANAGED_VALKEY_E2E_HEADED:-0}" == 1 ]]; then
    PLAYWRIGHT_BASE_URL="http://127.0.0.1:$web_port" \
        "$repo_root/scripts/run_playwright_e2e.sh" headed
else
    PLAYWRIGHT_BASE_URL="http://127.0.0.1:$web_port" \
        "$repo_root/scripts/run_playwright_e2e.sh" test
fi
