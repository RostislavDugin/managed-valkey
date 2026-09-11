#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
source "$repo_root/scripts/local_env.sh"
load_local_env "$repo_root"
state_dir="$repo_root/tmp/k3s/dev"
runtime_dir=${XDG_RUNTIME_DIR:-/tmp}
lock_file="$runtime_dir/managed-valkey-dev-${UID}.lock"
compose_file="$repo_root/docker-compose.dev.yml"
child_pids=()
dev_api_port=${MANAGED_VALKEY_DEV_API_PORT:-8080}
dev_operator_port=8081
dev_web_port=${MANAGED_VALKEY_DEV_WEB_PORT:-5173}
k3s_image=rancher/k3s:v1.35.7-k3s1

require_commands() {
    local command

    for command in "$@"; do
        command -v "$command" >/dev/null 2>&1 || {
            echo "dev: команда $command не установлена" >&2
            exit 1
        }
    done
}

load_dev_postgres() {
    local container existing_env

    POSTGRES_USER=${POSTGRES_USER:-managed_valkey}
    POSTGRES_PASSWORD=${POSTGRES_PASSWORD:-managed_valkey}
    POSTGRES_DB=${POSTGRES_DB:-managed_valkey}
    POSTGRES_PORT=${POSTGRES_PORT:-45432}

    container=$(docker ps -aq \
        --filter label=com.docker.compose.project=managed-valkey-dev \
        --filter label=com.docker.compose.service=postgres | head -n 1)
    if [[ -n "$container" ]]; then
        existing_env=$(docker inspect "$container" \
            --format '{{range .Config.Env}}{{println .}}{{end}}')
        POSTGRES_USER=$(awk -F= '$1 == "POSTGRES_USER" { sub(/^[^=]*=/, ""); print; exit }' \
            <<<"$existing_env")
        POSTGRES_PASSWORD=$(awk -F= '$1 == "POSTGRES_PASSWORD" { sub(/^[^=]*=/, ""); print; exit }' \
            <<<"$existing_env")
        POSTGRES_DB=$(awk -F= '$1 == "POSTGRES_DB" { sub(/^[^=]*=/, ""); print; exit }' \
            <<<"$existing_env")
        POSTGRES_PORT=$(docker port "$container" 5432/tcp 2>/dev/null |
            sed -n 's/.*://p' | head -n 1 || true)
        POSTGRES_PORT=${POSTGRES_PORT:-45432}
    fi
    [[ "$POSTGRES_PASSWORD" =~ ^[A-Za-z0-9._~-]+$ ]] || {
        echo "dev: POSTGRES_PASSWORD содержит символы, требующие кодирования в URL" >&2
        return 1
    }
    DATABASE_URL=${DATABASE_URL:-postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@127.0.0.1:${POSTGRES_PORT}/${POSTGRES_DB}?sslmode=disable}
    export POSTGRES_USER POSTGRES_PASSWORD POSTGRES_DB POSTGRES_PORT DATABASE_URL
}

active_test_database() {
    local suite status_file

    for suite in api integration e2e; do
        while IFS= read -r -d '' status_file; do
            case "$(cat "$status_file")" in
            allocated | ready) return 0 ;;
            esac
        done < <(find "$repo_root/tmp/k3s/$suite" -mindepth 2 -maxdepth 2 \
            -name status -print0 2>/dev/null)
    done
    return 1
}

cleanup_routes() {
    local destination gateway current routes_file="$state_dir/routes.tsv"

    [[ -f "$routes_file" ]] || return 0
    while IFS=$'\t' read -r destination gateway; do
        [[ -n "$destination" && -n "$gateway" ]] || continue
        current=$(ip route show exact "$destination" 2>/dev/null || true)
        if [[ "$current" == *"via $gateway"* ]]; then
            sudo ip route del "$destination" via "$gateway"
        fi
    done <"$routes_file"
}

down() {
    require_commands docker flock ip sudo
    exec 9>"$lock_file"
    flock -n 9 || {
        echo "dev: just run ещё работает" >&2
        exit 1
    }
    if active_test_database; then
        echo "dev: тестовый запуск использует общий PostgreSQL" >&2
        exit 1
    fi
    cleanup_routes
    docker compose -f "$compose_file" -p managed-valkey-dev down --remove-orphans
}

wait_http() {
    local name=$1 url=$2 attempt pid

    for attempt in {1..120}; do
        if curl --fail --silent --show-error "$url" >/dev/null 2>&1; then
            return 0
        fi
        for pid in "${child_pids[@]}"; do
            kill -0 "$pid" 2>/dev/null || {
                echo "dev: процесс завершился до готовности $name" >&2
                return 1
            }
        done
        sleep 1
    done
    echo "dev: $name не достиг готовности" >&2
    return 1
}

require_free_ports() {
    local port

    for port in "$dev_api_port" "$dev_operator_port" "$dev_web_port"; do
        if ss -ltn "sport = :$port" | tail -n +2 | grep -q .; then
            echo "dev: порт $port уже занят" >&2
            return 1
        fi
    done
}

load_dev_k3s_token() {
    local token volume=managed-valkey-dev_k3s-server-data

    docker volume inspect "$volume" >/dev/null 2>&1 || return 0
    token=$(docker run --rm --entrypoint /bin/sh \
        --volume "$volume:/data:ro" "$k3s_image" \
        -c 'cat /data/server/token' 2>/dev/null || true)
    [[ -z "$token" ]] || export K3S_TOKEN=$token
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
    rm -f "$repo_root/tmp/run/dev.pid"
}

remove_pid_file() {
    rm -f "$repo_root/tmp/run/dev.pid"
}

run() {
    local status

    require_commands docker flock go helm ip just kubectl mkcert openssl pnpm sudo curl setsid ss
    "$repo_root/scripts/k3s_host_prerequisites.sh"
    mkdir -p "$state_dir" "$repo_root/tmp/run"
    exec 9>"$lock_file"
    flock -n 9 || {
        echo "dev: just run уже работает" >&2
        exit 1
    }
    require_free_ports
    printf '%s\n' "$$" >"$repo_root/tmp/run/dev.pid"
    trap remove_pid_file EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM

    load_dev_postgres
    load_dev_k3s_token
    docker compose -f "$compose_file" -p managed-valkey-dev up -d --wait \
        postgres k3s-server k3s-agent-1 k3s-agent-2
    MV_STATE_DIR=$state_dir \
        ADMIN_KUBECONFIG="$state_dir/admin.kubeconfig" \
        API_KUBECONFIG="$state_dir/api.kubeconfig" \
        OPERATOR_KUBECONFIG="$state_dir/operator.kubeconfig" \
        OPERATOR_ENV="$state_dir/operator.env" \
        ROUTES_FILE="$state_dir/routes.tsv" \
        "$repo_root/scripts/k3s_bootstrap.sh"
    just --justfile "$repo_root/api/Justfile" migration-up
    pnpm --dir "$repo_root/web" install --frozen-lockfile

    trap stop_children EXIT
    trap 'stop_children; exit 130' INT
    trap 'stop_children; exit 143' TERM
    setsid just --justfile "$repo_root/operator/Justfile" run &
    child_pids+=("$!")
    setsid env HTTP_ADDR=":$dev_api_port" just --justfile "$repo_root/api/Justfile" _run-server &
    child_pids+=("$!")
    setsid env API_PROXY_TARGET="http://127.0.0.1:$dev_api_port" \
        just --justfile "$repo_root/web/Justfile" run 127.0.0.1 "$dev_web_port" &
    child_pids+=("$!")

    wait_http operator "http://127.0.0.1:$dev_operator_port/readyz"
    wait_http API "http://127.0.0.1:$dev_api_port/readyz"
    wait_http frontend "http://127.0.0.1:$dev_web_port"
    echo "dev: оператор, API и frontend готовы"

    set +e
    wait -n "${child_pids[@]}"
    status=$?
    set -e
    if ((status == 0)); then
        echo "dev: один из процессов завершился" >&2
        return 1
    fi
    return "$status"
}

case "${1:-run}" in
run) run ;;
down) down ;;
*)
    echo "usage: $0 [run|down]" >&2
    exit 2
    ;;
esac
