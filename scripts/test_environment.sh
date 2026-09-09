#!/usr/bin/env bash
set -Eeuo pipefail

repo_root=$(git rev-parse --show-toplevel)
compose_file="$repo_root/docker-compose.dev.yml"
test_compose_file="$repo_root/docker-compose.test.yml"
state_root="$repo_root/tmp/k3s"

log() {
    echo "test-env: $*" >&2
}

fail() {
    log "$*"
    exit 1
}

require_command() {
    local command

    for command in "$@"; do
        command -v "$command" >/dev/null 2>&1 || fail "команда $command не установлена"
    done
}

load_local_env() {
    local container existing_env had_password=0 had_port=0 had_user=0
    local saved_password="" saved_port="" saved_user=""

    if [[ -v POSTGRES_PASSWORD ]]; then
        had_password=1
        saved_password=$POSTGRES_PASSWORD
    fi
    if [[ -v POSTGRES_PORT ]]; then
        had_port=1
        saved_port=$POSTGRES_PORT
    fi
    if [[ -v POSTGRES_USER ]]; then
        had_user=1
        saved_user=$POSTGRES_USER
    fi

    if [[ -f "$repo_root/.env" ]]; then
        set -a
        source "$repo_root/.env"
        set +a
    fi

    ((had_password == 0)) || POSTGRES_PASSWORD=$saved_password
    ((had_port == 0)) || POSTGRES_PORT=$saved_port
    ((had_user == 0)) || POSTGRES_USER=$saved_user

    POSTGRES_USER=${POSTGRES_USER:-managed_valkey}
    POSTGRES_PORT=${POSTGRES_PORT:-45432}
    POSTGRES_DB=${POSTGRES_DB:-managed_valkey}

    container=$(docker ps -aq \
        --filter label=com.docker.compose.project=managed-valkey-dev \
        --filter label=com.docker.compose.service=postgres | head -n 1)
    if [[ -n "$container" ]]; then
        existing_env=$(docker inspect "$container" \
            --format '{{range .Config.Env}}{{println .}}{{end}}')
        POSTGRES_USER=$(awk -F= '$1 == "POSTGRES_USER" { sub(/^[^=]*=/, ""); print; exit }' <<<"$existing_env")
        POSTGRES_PASSWORD=$(awk -F= '$1 == "POSTGRES_PASSWORD" { sub(/^[^=]*=/, ""); print; exit }' <<<"$existing_env")
        POSTGRES_DB=$(awk -F= '$1 == "POSTGRES_DB" { sub(/^[^=]*=/, ""); print; exit }' <<<"$existing_env")
        POSTGRES_PORT=$(docker port "$container" 5432/tcp 2>/dev/null |
            sed -n 's/.*://p' | head -n 1 || true)
        POSTGRES_PORT=${POSTGRES_PORT:-45432}
    fi
    export POSTGRES_USER POSTGRES_PASSWORD POSTGRES_DB POSTGRES_PORT
}

validate_suite() {
    case "$1" in
    api | operator | integration | e2e) ;;
    *) fail "неизвестное окружение $1" ;;
    esac
}

new_run_id() {
    printf '%s-%(%Y%m%d%H%M%S)T-%s-%s\n' "$1" -1 "$$" "$RANDOM"
}

validate_run_id() {
    [[ "$1" =~ ^[a-z0-9][a-z0-9-]{0,47}$ ]] ||
        fail "run ID должен состоять из строчных латинских букв, цифр и дефисов"
}

cidrs_overlap() {
    python3 - "$1" "$2" <<'PY'
import ipaddress
import sys

left = ipaddress.ip_network(sys.argv[1], strict=False)
right = ipaddress.ip_network(sys.argv[2], strict=False)
raise SystemExit(0 if left.overlaps(right) else 1)
PY
}

candidate_is_free() {
    local candidate=$1 existing

    while IFS= read -r existing; do
        [[ -z "$existing" || "$existing" == "default" ]] && continue
        if cidrs_overlap "$candidate" "$existing"; then
            return 1
        fi
    done <<<"$allocated_cidrs"

    return 0
}

read_allocated_cidrs() {
    local env_file

    {
        docker network ls -q | xargs -r docker network inspect \
            --format '{{range .IPAM.Config}}{{println .Subnet}}{{end}}' 2>/dev/null || true
        ip -o route show | awk '{
            for (field = 1; field <= NF; field++) {
                if ($field ~ /^[0-9]+\.[0-9]+\./ && $field ~ /\//) {
                    print $field
                    break
                }
            }
        }'
        while IFS= read -r -d '' env_file; do
            [[ "$(cat "$(dirname "$env_file")/status" 2>/dev/null || true)" != cleaned ]] || continue
            (
                source "$env_file"
                printf '%s\n%s\n%s\n' "$K3S_DOCKER_SUBNET" "$K3S_CLUSTER_CIDR" "$K3S_SERVICE_CIDR"
            )
        done < <(find "$state_root" -mindepth 3 -maxdepth 3 -name environment.env -print0 2>/dev/null)
    } | sed '/^$/d' | sort -u
}

write_value() {
    printf '%s=%q\n' "$1" "$2" >>"$environment_file"
}

allocate_environment() {
    local suite=$1 run_id=$2 slot short_id checksum

    state_dir="$state_root/$suite/$run_id"
    environment_file="$state_dir/environment.env"
    mkdir -p "$state_root/$suite"

    exec 9>"$state_root/.allocation.lock"
    flock 9
    [[ ! -e "$state_dir" ]] || fail "окружение $suite/$run_id уже существует"
    mkdir -m 0700 "$state_dir"
    allocated_cidrs=$(read_allocated_cidrs)

    for slot in $(seq 0 31); do
        K3S_DOCKER_SUBNET="172.28.${slot}.0/24"
        K3S_CLUSTER_CIDR="10.$((64 + slot)).0.0/16"
        K3S_SERVICE_CIDR="10.$((128 + slot)).0.0/16"
        if candidate_is_free "$K3S_DOCKER_SUBNET" &&
            candidate_is_free "$K3S_CLUSTER_CIDR" &&
            candidate_is_free "$K3S_SERVICE_CIDR"; then
            break
        fi
    done
    if ((slot == 31)) &&
        (! candidate_is_free "$K3S_DOCKER_SUBNET" ||
            ! candidate_is_free "$K3S_CLUSTER_CIDR" ||
            ! candidate_is_free "$K3S_SERVICE_CIDR"); then
        rmdir "$state_dir"
        fail "свободные сетевые диапазоны закончились"
    fi

    K3S_CLUSTER_DNS="10.$((128 + slot)).0.10"
    K3S_DOCKER_GATEWAY="172.28.${slot}.1"
    K3S_SERVER_IP="172.28.${slot}.10"
    K3S_AGENT_1_IP="172.28.${slot}.11"
    K3S_AGENT_2_IP="172.28.${slot}.12"
    short_id=${run_id:0:24}
    checksum=$(printf '%s' "$run_id" | cksum | awk '{ print $1 }')
    MANAGED_VALKEY_COMPOSE_PROJECT="managed-valkey-${suite}-${short_id}-${checksum}"
    MANAGED_VALKEY_COMPOSE_PROJECT=${MANAGED_VALKEY_COMPOSE_PROJECT:0:63}
    K3S_TOKEN=$(openssl rand -hex 24)
    ADMIN_KUBECONFIG="$state_dir/admin.kubeconfig"
    API_KUBECONFIG="$state_dir/api.kubeconfig"
    OPERATOR_KUBECONFIG="$state_dir/operator.kubeconfig"
    OPERATOR_ENV="$state_dir/operator.env"
    ROUTES_FILE="$state_dir/routes.tsv"
    DIAGNOSTICS_DIR="$state_dir/diagnostics"
    K3S_API_BINDING="127.0.0.1::6443"
    K3S_PUBLIC_BINDING="127.0.0.1::31379"
    VALKEY_BASE_DOMAIN="${suite}-${checksum}.valkey.localhost"
    MANAGED_VALKEY_CLIENT_ALLOWED="172.28.${slot}.101"
    MANAGED_VALKEY_CLIENT_BLOCKED="172.28.${slot}.102"
    MANAGED_VALKEY_CLIENT_WRONG_SNI="172.28.${slot}.103"
    MANAGED_VALKEY_CLIENT_PRE_READY="172.28.${slot}.104"
    MANAGED_VALKEY_CA_FILE="$state_dir/ca.crt"
    if [[ "$suite" == api ]]; then
        BOOTSTRAP_PROFILE=api
        K3S_EXPECTED_NODES=1
    else
        BOOTSTRAP_PROFILE=full
        K3S_EXPECTED_NODES=3
    fi

    umask 077
    : >"$environment_file"
    write_value MV_SUITE "$suite"
    write_value MV_RUN_ID "$run_id"
    write_value MV_STATE_DIR "$state_dir"
    write_value MANAGED_VALKEY_COMPOSE_PROJECT "$MANAGED_VALKEY_COMPOSE_PROJECT"
    write_value K3S_DOCKER_SUBNET "$K3S_DOCKER_SUBNET"
    write_value K3S_DOCKER_GATEWAY "$K3S_DOCKER_GATEWAY"
    write_value K3S_CLUSTER_CIDR "$K3S_CLUSTER_CIDR"
    write_value K3S_SERVICE_CIDR "$K3S_SERVICE_CIDR"
    write_value K3S_CLUSTER_DNS "$K3S_CLUSTER_DNS"
    write_value K3S_SERVER_IP "$K3S_SERVER_IP"
    write_value K3S_AGENT_1_IP "$K3S_AGENT_1_IP"
    write_value K3S_AGENT_2_IP "$K3S_AGENT_2_IP"
    write_value K3S_TOKEN "$K3S_TOKEN"
    write_value K3S_API_BINDING "$K3S_API_BINDING"
    write_value K3S_PUBLIC_BINDING "$K3S_PUBLIC_BINDING"
    write_value K3S_EXPECTED_NODES "$K3S_EXPECTED_NODES"
    write_value BOOTSTRAP_PROFILE "$BOOTSTRAP_PROFILE"
    write_value ADMIN_KUBECONFIG "$ADMIN_KUBECONFIG"
    write_value API_KUBECONFIG "$API_KUBECONFIG"
    write_value OPERATOR_KUBECONFIG "$OPERATOR_KUBECONFIG"
    write_value OPERATOR_ENV "$OPERATOR_ENV"
    write_value ROUTES_FILE "$ROUTES_FILE"
    write_value DIAGNOSTICS_DIR "$DIAGNOSTICS_DIR"
    write_value VALKEY_BASE_DOMAIN "$VALKEY_BASE_DOMAIN"
    write_value MANAGED_VALKEY_CLIENT_ALLOWED "$MANAGED_VALKEY_CLIENT_ALLOWED"
    write_value MANAGED_VALKEY_CLIENT_BLOCKED "$MANAGED_VALKEY_CLIENT_BLOCKED"
    write_value MANAGED_VALKEY_CLIENT_WRONG_SNI "$MANAGED_VALKEY_CLIENT_WRONG_SNI"
    write_value MANAGED_VALKEY_CLIENT_PRE_READY "$MANAGED_VALKEY_CLIENT_PRE_READY"
    write_value MANAGED_VALKEY_CA_FILE "$MANAGED_VALKEY_CA_FILE"
    chmod 0600 "$environment_file"
    printf 'allocated\n' >"$state_dir/status"
    flock -u 9
}

compose() {
    docker compose -f "$compose_file" -f "$test_compose_file" \
        -p "$MANAGED_VALKEY_COMPOSE_PROJECT" "$@"
}

prepare_postgres() {
    local database_name database_suffix

    [[ -n "${POSTGRES_PASSWORD:-}" ]] || fail "задайте POSTGRES_PASSWORD в .env или окружении"
    [[ "$POSTGRES_PASSWORD" =~ ^[A-Za-z0-9._~-]+$ ]] ||
        fail "POSTGRES_PASSWORD содержит символы, требующие кодирования в URL"

    exec 8>"$state_root/.postgres.lock"
    flock 8
    MANAGED_VALKEY_COMPOSE_PROJECT=managed-valkey-dev \
        docker compose -f "$compose_file" -p managed-valkey-dev up -d --wait postgres
    flock -u 8

    database_suffix=$(printf '%s_%s' "$MV_SUITE" "$MV_RUN_ID" | tr '-' '_' | cut -c1-42)
    database_name="managed_valkey_test_${database_suffix}_$(printf '%s' "$MV_RUN_ID" | cksum | awk '{ print $1 }')"
    database_name=${database_name:0:63}
    [[ "$database_name" != "$POSTGRES_DB" ]] || fail "тестовая база совпала с dev-базой"

    if ! docker compose -f "$compose_file" -p managed-valkey-dev exec -T postgres \
        psql --username "$POSTGRES_USER" --dbname postgres --tuples-only --no-align \
        --command "SELECT 1 FROM pg_database WHERE datname = '$database_name'" | grep -qx 1; then
        docker compose -f "$compose_file" -p managed-valkey-dev exec -T postgres \
            createdb --username "$POSTGRES_USER" "$database_name"
    fi

    TEST_DATABASE_URL="postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@127.0.0.1:${POSTGRES_PORT}/${database_name}?sslmode=disable"
    DATABASE_URL=$TEST_DATABASE_URL
    write_value TEST_DATABASE_NAME "$database_name"
    write_value TEST_DATABASE_URL "$TEST_DATABASE_URL"
    write_value DATABASE_URL "$DATABASE_URL"
    write_value POSTGRES_USER "$POSTGRES_USER"
    write_value POSTGRES_PORT "$POSTGRES_PORT"
    write_value POSTGRES_DB "$POSTGRES_DB"
    write_value POSTGRES_PASSWORD "$POSTGRES_PASSWORD"
}

prepare_cluster() {
    local api_address public_address public_node=k3s-server services=(k3s-server)

    if ((K3S_EXPECTED_NODES == 3)); then
        services+=(k3s-agent-1 k3s-agent-2)
    fi
    compose up -d --wait "${services[@]}"

    api_address=$(compose port k3s-server 6443)
    KUBERNETES_SERVER="https://${api_address}"
    MANAGED_VALKEY_DOCKER_NETWORK="${MANAGED_VALKEY_COMPOSE_PROJECT}_default"

    write_value KUBERNETES_SERVER "$KUBERNETES_SERVER"
    write_value MANAGED_VALKEY_DOCKER_NETWORK "$MANAGED_VALKEY_DOCKER_NETWORK"

    export ADMIN_KUBECONFIG API_KUBECONFIG OPERATOR_KUBECONFIG OPERATOR_ENV ROUTES_FILE
    export BOOTSTRAP_PROFILE K3S_EXPECTED_NODES KUBERNETES_SERVER
    export MANAGED_VALKEY_COMPOSE_PROJECT MANAGED_VALKEY_DOCKER_NETWORK
    export MANAGED_VALKEY_CA_FILE VALKEY_BASE_DOMAIN
    "$repo_root/scripts/k3s_bootstrap.sh"

    if [[ "$BOOTSTRAP_PROFILE" == full ]]; then
        public_node=$(KUBECONFIG="$ADMIN_KUBECONFIG" kubectl -n envoy-gateway-system get pods \
            -l gateway.envoyproxy.io/owning-gateway-name=valkey \
            --field-selector=status.phase=Running \
            -o jsonpath='{.items[0].spec.nodeName}')
        case "$public_node" in
        k3s-server | k3s-agent-1 | k3s-agent-2) ;;
        *) fail "не найдена нода с готовым Envoy" ;;
        esac
    fi
    public_address=$(compose port "$public_node" 31379)
    MANAGED_VALKEY_PUBLIC_ADDRESS=$public_address
    MANAGED_VALKEY_DOCKER_HOST=$public_node
    MANAGED_VALKEY_DOCKER_PORT=31379
    write_value MANAGED_VALKEY_PUBLIC_ADDRESS "$MANAGED_VALKEY_PUBLIC_ADDRESS"
    write_value MANAGED_VALKEY_DOCKER_HOST "$MANAGED_VALKEY_DOCKER_HOST"
    write_value MANAGED_VALKEY_DOCKER_PORT "$MANAGED_VALKEY_DOCKER_PORT"
}

prepare() {
    local suite=$1 run_id=$2

    handle_prepare_failure() {
        local status=$1

        trap - ERR INT TERM
        collect_diagnostics "$state_dir" || true
        cleanup "$state_dir" || true
        exit "$status"
    }

    validate_suite "$suite"
    validate_run_id "$run_id"
    require_command docker flock ip kubectl openssl python3
    "$repo_root/scripts/k3s_host_prerequisites.sh"
    load_local_env
    mkdir -p "$state_root"
    allocate_environment "$suite" "$run_id"

    set -a
    source "$environment_file"
    set +a
    trap 'handle_prepare_failure $?' ERR
    trap 'handle_prepare_failure 130' INT
    trap 'handle_prepare_failure 143' TERM
    if [[ "$suite" == api || "$suite" == integration || "$suite" == e2e ]]; then
        prepare_postgres >&2
        set -a
        source "$environment_file"
        set +a
    fi
    prepare_cluster >&2
    printf 'ready\n' >"$state_dir/status"
    trap - ERR INT TERM
    log "$suite готов: $state_dir"
    printf '%s\n' "$state_dir"
}

collect_diagnostics() {
    local target=$1

    [[ -f "$target/environment.env" ]] || fail "нет состояния $target"
    set -a
    source "$target/environment.env"
    set +a
    mkdir -p "$DIAGNOSTICS_DIR"
    chmod 0700 "$DIAGNOSTICS_DIR"

    compose ps --all >"$DIAGNOSTICS_DIR/compose-ps.txt" 2>&1 || true
    compose logs --no-color --tail 500 k3s-server k3s-agent-1 k3s-agent-2 \
        >"$DIAGNOSTICS_DIR/k3s.log" 2>&1 || true
    if [[ -f "$ADMIN_KUBECONFIG" ]]; then
        KUBECONFIG="$ADMIN_KUBECONFIG" kubectl get nodes -o wide \
            >"$DIAGNOSTICS_DIR/nodes.txt" 2>&1 || true
        KUBECONFIG="$ADMIN_KUBECONFIG" kubectl get namespaces,pods,services,statefulsets,deployments \
            -A -o wide >"$DIAGNOSTICS_DIR/resources.txt" 2>&1 || true
        KUBECONFIG="$ADMIN_KUBECONFIG" kubectl get events -A --sort-by=.metadata.creationTimestamp \
            >"$DIAGNOSTICS_DIR/events.txt" 2>&1 || true
    fi
    log "диагностика: $DIAGNOSTICS_DIR"
}

cleanup_routes() {
    local destination gateway current

    [[ -f "$ROUTES_FILE" ]] || return 0
    while IFS=$'\t' read -r destination gateway; do
        [[ -n "$destination" && -n "$gateway" ]] || continue
        current=$(ip route show exact "$destination" 2>/dev/null || true)
        if [[ "$current" == *"via $gateway"* ]]; then
            sudo ip route del "$destination" via "$gateway"
        fi
    done <"$ROUTES_FILE"
}

remove_test_clients() {
    local network=${MANAGED_VALKEY_DOCKER_NETWORK:-${MANAGED_VALKEY_COMPOSE_PROJECT}_default}
    local -a containers=()

    mapfile -t containers < <(docker ps -aq \
        --filter "label=com.h3llo-demo.managed-valkey.test-network=$network")
    ((${#containers[@]} == 0)) || docker rm -f "${containers[@]}" >/dev/null
}

drop_test_database() {
    [[ -n "${TEST_DATABASE_NAME:-}" ]] || return 0
    [[ "$TEST_DATABASE_NAME" =~ ^managed_valkey_test_[a-z0-9_]+$ ]] ||
        { log "отказ удалить базу с недопустимым именем $TEST_DATABASE_NAME"; return 1; }
    [[ "$TEST_DATABASE_NAME" != "$POSTGRES_DB" ]] ||
        { log "отказ удалить dev-базу $TEST_DATABASE_NAME"; return 1; }

    docker compose -f "$compose_file" -p managed-valkey-dev exec -T postgres \
        psql --username "$POSTGRES_USER" --dbname postgres \
        --command "DROP DATABASE IF EXISTS \"$TEST_DATABASE_NAME\" WITH (FORCE)"
}

cleanup() {
    local target=$1 cleanup_status=0 network

    [[ -f "$target/environment.env" ]] || fail "нет состояния $target"
    set -a
    source "$target/environment.env"
    set +a

    cleanup_routes || cleanup_status=1
    remove_test_clients || cleanup_status=1
    compose down --volumes --remove-orphans || cleanup_status=1
    network=${MANAGED_VALKEY_DOCKER_NETWORK:-${MANAGED_VALKEY_COMPOSE_PROJECT}_default}
    if docker network inspect "$network" >/dev/null 2>&1; then
        docker network rm "$network" >/dev/null || cleanup_status=1
    fi
    drop_test_database || cleanup_status=1
    if ((cleanup_status == 0)); then
        printf 'cleaned\n' >"$target/status"
        log "очищено: $target"
    else
        printf 'cleanup-failed\n' >"$target/status"
        log "не удалось полностью очистить $target"
    fi
    return "$cleanup_status"
}

run_in_environment() {
    local suite=$1 run_id=$2 state command_status=0 cleanup_status=0 child_pid=0 signal_status=0
    shift 2
    [[ "${1:-}" == -- ]] || fail "после окружения ожидается --"
    shift
    (($# > 0)) || fail "команда тестов не задана"

    prepare "$suite" "$run_id"
    state="$state_root/$suite/$run_id"
    set -a
    source "$state/environment.env"
    set +a

    stop_child() {
        local status=$1

        signal_status=$status
        if ((child_pid > 0)); then
            kill -TERM -- "-$child_pid" 2>/dev/null || true
        fi
    }
    trap 'stop_child 130' INT
    trap 'stop_child 143' TERM

    setsid "$@" &
    child_pid=$!
    printf '%s\t%s\n' "$child_pid" "$*" >>"$state/processes.tsv"
    wait "$child_pid" || command_status=$?
    ((signal_status == 0)) || command_status=$signal_status
    trap - INT TERM

    if ((command_status != 0)); then
        collect_diagnostics "$state" || true
    fi
    cleanup "$state" || cleanup_status=$?
    log "журнал окружения: $state"

    if ((command_status != 0)); then
        return "$command_status"
    fi
    return "$cleanup_status"
}

usage() {
    echo "usage: $0 prepare <api|operator|integration|e2e> [run-id]" >&2
    echo "       $0 cleanup|diagnostics <state-dir>" >&2
    echo "       $0 exec <api|operator|integration|e2e> [run-id] -- <command>" >&2
    exit 2
}

command_name=${1:-}
case "$command_name" in
prepare)
    (($# == 2 || $# == 3)) || usage
    suite=$2
    run_id=${3:-${MANAGED_VALKEY_RUN_ID:-$(new_run_id "$suite")}}
    prepare "$suite" "$run_id"
    ;;
cleanup)
    (($# == 2)) || usage
    cleanup "$2"
    ;;
diagnostics)
    (($# == 2)) || usage
    collect_diagnostics "$2"
    ;;
exec)
    (($# >= 4)) || usage
    suite=$2
    shift 2
    if [[ "$1" == -- ]]; then
        run_id=${MANAGED_VALKEY_RUN_ID:-$(new_run_id "$suite")}
    else
        run_id=$1
        shift
    fi
    run_in_environment "$suite" "$run_id" "$@"
    ;;
*) usage ;;
esac
