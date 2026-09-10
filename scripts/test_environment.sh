#!/usr/bin/env bash
set -Eeuo pipefail

repo_root=$(git rev-parse --show-toplevel)
compose_file="$repo_root/docker-compose.dev.yml"
test_compose_file="$repo_root/docker-compose.test.yml"
state_root=${MV_TEST_STATE_ROOT:-$repo_root/tmp/k3s}
bootstrap_lock_fd=""
resource_reservation_owner=""

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

configure_environment_mode() {
    local suite=$1

    MV_TEST_K3S_ENABLED=${MV_TEST_K3S_ENABLED:-1}
    [[ "$MV_TEST_K3S_ENABLED" =~ ^[01]$ ]] || fail "MV_TEST_K3S_ENABLED должно быть 0 или 1"
    if [[ "$suite" != api && "$MV_TEST_K3S_ENABLED" == 0 ]]; then
        fail "окружение без k3s поддерживает только профиль api"
    fi
}

validate_cluster_profile() {
    if [[ ! "$MANAGED_VALKEY_K3S_NODES" =~ ^[1-4]$ ]]; then
        fail "MANAGED_VALKEY_K3S_NODES должно быть от 1 до 4"
    fi
    if [[ "$BOOTSTRAP_PROFILE" == api ]]; then
        ((MANAGED_VALKEY_K3S_NODES == 1)) || fail "профиль api поддерживает одну ноду"
        ((MANAGED_VALKEY_ENVOY_REPLICAS == 0)) || fail "профиль api не разворачивает Envoy"
        return
    fi
    if [[ ! "$MANAGED_VALKEY_ENVOY_REPLICAS" =~ ^[12]$ ]]; then
        fail "MANAGED_VALKEY_ENVOY_REPLICAS должно быть 1 или 2"
    fi
    if ((MANAGED_VALKEY_ENVOY_REPLICAS > MANAGED_VALKEY_K3S_NODES)); then
        fail "реплики Envoy нельзя разнести по $MANAGED_VALKEY_K3S_NODES нодам"
    fi
}

configure_cluster_profile() {
    local suite=$1

    if [[ "$suite" == api ]]; then
        BOOTSTRAP_PROFILE=api
        MANAGED_VALKEY_K3S_NODES=${MANAGED_VALKEY_K3S_NODES:-1}
        MANAGED_VALKEY_ENVOY_REPLICAS=${MANAGED_VALKEY_ENVOY_REPLICAS:-0}
    elif [[ "$suite" == integration ]]; then
        BOOTSTRAP_PROFILE=full
        MANAGED_VALKEY_K3S_NODES=${MANAGED_VALKEY_K3S_NODES:-1}
        MANAGED_VALKEY_ENVOY_REPLICAS=${MANAGED_VALKEY_ENVOY_REPLICAS:-1}
    else
        BOOTSTRAP_PROFILE=full
        MANAGED_VALKEY_K3S_NODES=${MANAGED_VALKEY_K3S_NODES:-3}
        MANAGED_VALKEY_ENVOY_REPLICAS=${MANAGED_VALKEY_ENVOY_REPLICAS:-2}
    fi
    K3S_EXPECTED_NODES=$MANAGED_VALKEY_K3S_NODES
    validate_cluster_profile
}

default_memory_mib_for_suite() {
    local suite=$1

    if [[ "$suite" == api && "${MV_TEST_K3S_ENABLED:-1}" == 0 ]]; then
        printf '512\n'
    elif [[ "$suite" == integration ]]; then
        printf '8192\n'
    else
        printf '3072\n'
    fi
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

replace_value() {
    local name=$1 value=$2 temporary line

    temporary=$(mktemp "${environment_file}.XXXXXX")
    while IFS= read -r line; do
        [[ "$line" == "$name="* ]] || printf '%s\n' "$line" >>"$temporary"
    done <"$environment_file"
    printf '%s=%q\n' "$name" "$value" >>"$temporary"
    chmod 0600 "$temporary"
    mv "$temporary" "$environment_file"
}

validate_environment_addresses() {
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
if len(set(addresses)) != len(addresses):
    raise SystemExit("адреса тестового окружения пересекаются")
if any(address not in network for address in addresses):
    raise SystemExit("адрес тестового окружения находится вне выделенной подсети")
PY
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
    K3S_AGENT_3_IP="172.28.${slot}.13"
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
    NETWORK_FAULTS_FILE="$state_dir/network-faults.tsv"
    FAULT_EVENTS_FILE="$state_dir/fault-events.tsv"
    DIAGNOSTICS_DIR="$state_dir/diagnostics"
    K3S_API_BINDING="127.0.0.1::6443"
    K3S_PUBLIC_BINDING="127.0.0.1::31379"
    VALKEY_BASE_DOMAIN="${suite}-${checksum}.valkey.localhost"
    MANAGED_VALKEY_CLIENT_ALLOWED="172.28.${slot}.101"
    MANAGED_VALKEY_CLIENT_BLOCKED="172.28.${slot}.102"
    MANAGED_VALKEY_CLIENT_WRONG_SNI="172.28.${slot}.103"
    MANAGED_VALKEY_CLIENT_PRE_READY="172.28.${slot}.104"
    MANAGED_VALKEY_CA_FILE="$state_dir/ca.crt"
    validate_environment_addresses
    umask 077
    : >"$environment_file"
    write_value MV_SUITE "$suite"
    write_value MV_RUN_ID "$run_id"
    write_value MV_STATE_DIR "$state_dir"
    write_value MV_TEST_K3S_ENABLED "$MV_TEST_K3S_ENABLED"
    write_value MANAGED_VALKEY_COMPOSE_PROJECT "$MANAGED_VALKEY_COMPOSE_PROJECT"
    write_value K3S_DOCKER_SUBNET "$K3S_DOCKER_SUBNET"
    write_value K3S_DOCKER_GATEWAY "$K3S_DOCKER_GATEWAY"
    write_value K3S_CLUSTER_CIDR "$K3S_CLUSTER_CIDR"
    write_value K3S_SERVICE_CIDR "$K3S_SERVICE_CIDR"
    write_value K3S_CLUSTER_DNS "$K3S_CLUSTER_DNS"
    write_value K3S_SERVER_IP "$K3S_SERVER_IP"
    write_value K3S_AGENT_1_IP "$K3S_AGENT_1_IP"
    write_value K3S_AGENT_2_IP "$K3S_AGENT_2_IP"
    write_value K3S_AGENT_3_IP "$K3S_AGENT_3_IP"
    write_value K3S_TOKEN "$K3S_TOKEN"
    write_value K3S_API_BINDING "$K3S_API_BINDING"
    write_value K3S_PUBLIC_BINDING "$K3S_PUBLIC_BINDING"
    write_value K3S_EXPECTED_NODES "$K3S_EXPECTED_NODES"
    write_value MANAGED_VALKEY_K3S_NODES "$MANAGED_VALKEY_K3S_NODES"
    write_value MANAGED_VALKEY_ENVOY_REPLICAS "$MANAGED_VALKEY_ENVOY_REPLICAS"
    write_value BOOTSTRAP_PROFILE "$BOOTSTRAP_PROFILE"
    write_value ADMIN_KUBECONFIG "$ADMIN_KUBECONFIG"
    write_value API_KUBECONFIG "$API_KUBECONFIG"
    write_value OPERATOR_KUBECONFIG "$OPERATOR_KUBECONFIG"
    write_value OPERATOR_ENV "$OPERATOR_ENV"
    write_value ROUTES_FILE "$ROUTES_FILE"
    write_value NETWORK_FAULTS_FILE "$NETWORK_FAULTS_FILE"
    write_value FAULT_EVENTS_FILE "$FAULT_EVENTS_FILE"
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
    local api_address index public_address public_node=k3s-server services=(k3s-server)

    for ((index = 1; index < K3S_EXPECTED_NODES; index++)); do
        services+=("k3s-agent-$index")
    done
    compose up -d --wait "${services[@]}"

    api_address=$(compose port k3s-server 6443)
    KUBERNETES_SERVER="https://${api_address}"
    MANAGED_VALKEY_DOCKER_NETWORK="${MANAGED_VALKEY_COMPOSE_PROJECT}_default"

    write_value KUBERNETES_SERVER "$KUBERNETES_SERVER"
    write_value MANAGED_VALKEY_DOCKER_NETWORK "$MANAGED_VALKEY_DOCKER_NETWORK"

    export ADMIN_KUBECONFIG API_KUBECONFIG OPERATOR_KUBECONFIG OPERATOR_ENV ROUTES_FILE
    export NETWORK_FAULTS_FILE FAULT_EVENTS_FILE
    export BOOTSTRAP_PROFILE K3S_EXPECTED_NODES KUBERNETES_SERVER
    export MANAGED_VALKEY_ENVOY_REPLICAS
    export MANAGED_VALKEY_COMPOSE_PROJECT MANAGED_VALKEY_DOCKER_NETWORK
    export MANAGED_VALKEY_CA_FILE VALKEY_BASE_DOMAIN
    "$repo_root/scripts/k3s_bootstrap.sh"

    if [[ "$BOOTSTRAP_PROFILE" == full ]]; then
        public_node=$(KUBECONFIG="$ADMIN_KUBECONFIG" kubectl -n envoy-gateway-system get pods \
            -l gateway.envoyproxy.io/owning-gateway-name=valkey \
            --field-selector=status.phase=Running \
            -o jsonpath='{.items[0].spec.nodeName}')
        case "$public_node" in
        k3s-server | k3s-agent-1 | k3s-agent-2 | k3s-agent-3) ;;
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

acquire_bootstrap_slot() {
    local candidate_fd index max_parallel=${MV_TEST_MAX_PARALLEL_BOOTSTRAPS:-6}

    [[ "$max_parallel" =~ ^[1-9][0-9]*$ ]] || fail "MV_TEST_MAX_PARALLEL_BOOTSTRAPS должно быть положительным числом"
    mkdir -p "$state_root/bootstrap-slots"
    while true; do
        for ((index = 0; index < max_parallel; index++)); do
            exec {candidate_fd}>"$state_root/bootstrap-slots/$index.lock"
            if flock -n "$candidate_fd"; then
                bootstrap_lock_fd=$candidate_fd
                return
            fi
            exec {candidate_fd}>&-
        done
        sleep 1
    done
}

release_bootstrap_slot() {
    [[ -n "$bootstrap_lock_fd" ]] || return 0
    flock -u "$bootstrap_lock_fd"
    exec {bootstrap_lock_fd}>&-
}

reserve_environment_resources() {
    local suite=$1 run_id=$2 memory_mib owner_pid=$$ owner_start

    [[ "${MV_TEST_RESOURCE_RESERVED:-0}" != 1 ]] || return 0
    memory_mib=${MV_TEST_MEMORY_MIB:-}
    if [[ -z "$memory_mib" ]]; then
        memory_mib=$(default_memory_mib_for_suite "$suite")
    fi
    [[ "$memory_mib" =~ ^[1-9][0-9]*$ ]] || fail "MV_TEST_MEMORY_MIB должно быть положительным числом"
    resource_reservation_owner="environment-${suite}-${run_id}"
    owner_start=$("$repo_root/scripts/test_resources.sh" start-ticks "$owner_pid")
    "$repo_root/scripts/test_resources.sh" reserve \
        "$resource_reservation_owner" "$suite" "$memory_mib" "$owner_pid" "$owner_start"
}

release_environment_resources() {
    [[ -n "$resource_reservation_owner" ]] || return 0
    "$repo_root/scripts/test_resources.sh" release "$resource_reservation_owner"
    resource_reservation_owner=""
}

report_integration_duration() {
    local started_seconds=$1 target_seconds=${MV_INTEGRATION_TARGET_SECONDS:-300}
    local elapsed_seconds exceeded=false

    [[ "$target_seconds" =~ ^[0-9]+$ ]] || fail "MV_INTEGRATION_TARGET_SECONDS должно быть целым неотрицательным числом"
    elapsed_seconds=$((SECONDS - started_seconds))
    if ((elapsed_seconds > target_seconds)); then
        exceeded=true
    fi
    mkdir -p "$DIAGNOSTICS_DIR"
    printf 'full\t%d\n' "$elapsed_seconds" >>"$DIAGNOSTICS_DIR/durations.tsv"
    log "integration duration: total_seconds=$elapsed_seconds target_seconds=$target_seconds target_exceeded=$exceeded"
}

register_environment_owner() {
    printf '%s\n' "$$" >"$state_dir/owner.pid"
    "$repo_root/scripts/test_resources.sh" start-ticks "$$" >"$state_dir/owner.start-ticks"
    tr -d '\n' </proc/sys/kernel/random/boot_id >"$state_dir/owner.boot-id"
}

prepare() {
    local suite=$1 run_id=$2

    handle_prepare_failure() {
        local status=$1

        trap - ERR INT TERM
        release_bootstrap_slot || true
        if [[ -n "${state_dir:-}" && -f "$state_dir/environment.env" && -f "$state_dir/status" ]]; then
            collect_diagnostics "$state_dir" || true
            cleanup "$state_dir" || true
        elif [[ -n "${state_dir:-}" && -d "$state_dir" ]]; then
            rm -f -- "$state_dir/environment.env" "$state_dir/status" \
                "$state_dir/owner.pid" "$state_dir/owner.start-ticks" "$state_dir/owner.boot-id"
            rmdir -- "$state_dir" 2>/dev/null || true
        fi
        exit "$status"
    }

    validate_suite "$suite"
    validate_run_id "$run_id"
    configure_environment_mode "$suite"
    configure_cluster_profile "$suite"
    require_command docker flock ip openssl python3 rg
    if [[ "$MV_TEST_K3S_ENABLED" == 1 ]]; then
        require_command kubectl
        "$repo_root/scripts/k3s_host_prerequisites.sh"
    fi
    load_local_env
    mkdir -p "$state_root"
    trap 'handle_prepare_failure $?' ERR
    trap 'handle_prepare_failure 130' INT
    trap 'handle_prepare_failure 143' TERM
    allocate_environment "$suite" "$run_id"
    register_environment_owner

    set -a
    source "$environment_file"
    set +a
    if [[ "$suite" == api || "$suite" == integration || "$suite" == e2e ]]; then
        prepare_postgres >&2
        set -a
        source "$environment_file"
        set +a
    fi
    if [[ "$MV_TEST_K3S_ENABLED" == 1 ]]; then
        acquire_bootstrap_slot
        prepare_cluster >&2
        release_bootstrap_slot
    fi
    printf 'ready\n' >"$state_dir/status"
    trap - ERR
    log "$suite готов: $state_dir"
    printf '%s\n' "$state_dir"
}

wait_operator_node_ready() {
    local node=$1

    for _ in {1..90}; do
        if KUBECONFIG="$ADMIN_KUBECONFIG" kubectl wait \
            --for=condition=Ready "node/$node" --timeout=1s >/dev/null 2>&1 &&
            KUBECONFIG="$ADMIN_KUBECONFIG" kubectl get "node/$node" \
            -o jsonpath='{.spec.podCIDR}{"\t"}{range .status.addresses[?(@.type=="InternalIP")]}{.address}{end}' |
            awk -F '\t' 'NF == 2 && $1 != "" && $2 != "" { found = 1 } END { exit !found }'; then
            return 0
        fi
        sleep 1
    done
    fail "у Node $node отсутствует Pod CIDR или InternalIP"
}

configure_operator_node_route() {
    local node=$1 route node_ip pod_cidr current temporary

    route=$(KUBECONFIG="$ADMIN_KUBECONFIG" kubectl get "node/$node" \
        -o jsonpath='{.spec.podCIDR}{"\t"}{range .status.addresses[?(@.type=="InternalIP")]}{.address}{end}')
    IFS=$'\t' read -r pod_cidr node_ip <<<"$route"
    [[ -n "$pod_cidr" && -n "$node_ip" ]] || fail "у Node $node отсутствует маршрут"
    current=$(ip route show exact "$pod_cidr" 2>/dev/null || true)
    if [[ -n "$current" && "$current" != *"via $node_ip"* ]]; then
        fail "маршрут $pod_cidr уже принадлежит другому gateway"
    fi
    sudo ip route replace "$pod_cidr" via "$node_ip"

    temporary=$(mktemp "${ROUTES_FILE}.XXXXXX")
    if [[ -f "$ROUTES_FILE" ]]; then
        awk -F '\t' -v destination="$pod_cidr" '$1 != destination' \
            "$ROUTES_FILE" >"$temporary"
    fi
    printf '%s\t%s\n' "$pod_cidr" "$node_ip" >>"$temporary"
    chmod 0600 "$temporary"
    mv "$temporary" "$ROUTES_FILE"
}

expand_operator_cluster() {
    local target=$1 operator_image=$2 valkey_image=$3 actual_gateway

    [[ -f "$target/environment.env" ]] || fail "нет состояния $target"
    state_dir=$target
    environment_file="$target/environment.env"
    set -a
    source "$environment_file"
    set +a
    require_command docker ip kubectl sudo
    [[ "$MV_SUITE" == operator ]] || fail "дополнительный agent разрешён только стенду оператора"
    [[ "$(cat "$target/status")" == ready ]] || fail "стенд оператора не готов"
    actual_gateway=$(docker network inspect "$MANAGED_VALKEY_DOCKER_NETWORK" \
        --format '{{(index .IPAM.Config 0).Gateway}}')
    [[ "$actual_gateway" == "$K3S_DOCKER_GATEWAY" ]] ||
        fail "gateway сети изменился до добавления agent"

    K3S_EXPECTED_NODES=4
    MANAGED_VALKEY_K3S_NODES=4
    export K3S_EXPECTED_NODES
    export MANAGED_VALKEY_K3S_NODES
    replace_value K3S_EXPECTED_NODES "$K3S_EXPECTED_NODES"
    replace_value MANAGED_VALKEY_K3S_NODES "$MANAGED_VALKEY_K3S_NODES"
    compose up -d --wait k3s-agent-3
    if [[ -n "${MANAGED_VALKEY_K3S_IMAGE_ARCHIVE:-}" ]]; then
        "$repo_root/scripts/load_k3s_image.sh" \
            "$MANAGED_VALKEY_COMPOSE_PROJECT" --archive "$MANAGED_VALKEY_K3S_IMAGE_ARCHIVE" k3s-agent-3
    else
        "$repo_root/scripts/load_k3s_image.sh" \
            "$MANAGED_VALKEY_COMPOSE_PROJECT" "$operator_image" k3s-agent-3
        "$repo_root/scripts/load_k3s_image.sh" \
            "$MANAGED_VALKEY_COMPOSE_PROJECT" "$valkey_image" k3s-agent-3
    fi
    wait_operator_node_ready k3s-agent-3
    configure_operator_node_route k3s-agent-3

    actual_gateway=$(docker network inspect "$MANAGED_VALKEY_DOCKER_NETWORK" \
        --format '{{(index .IPAM.Config 0).Gateway}}')
    [[ "$actual_gateway" == "$K3S_DOCKER_GATEWAY" ]] ||
        fail "gateway сети изменился после добавления agent"
    log "четвёртая нода оператора готова: $target"
}

refresh_operator_public_address() {
    local target=$1 public_node public_address

    [[ -f "$target/environment.env" ]] || fail "нет состояния $target"
    state_dir=$target
    environment_file="$target/environment.env"
    set -a
    source "$environment_file"
    set +a
    [[ "$MV_SUITE" == operator ]] || fail "публичный адрес обновляется только для стенда оператора"
    public_node=$(KUBECONFIG="$ADMIN_KUBECONFIG" kubectl -n envoy-gateway-system get pods \
        -l gateway.envoyproxy.io/owning-gateway-name=valkey \
        -o jsonpath='{range .items[*]}{.spec.nodeName}{"\t"}{.status.phase}{"\t"}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}' |
        awk '$2 == "Running" && $3 == "True" { print $1; exit }')
    case "$public_node" in
    k3s-server | k3s-agent-1 | k3s-agent-2 | k3s-agent-3) ;;
    *) fail "не найдена нода с готовым Envoy" ;;
    esac
    public_address=$(compose port "$public_node" 31379)
    replace_value MANAGED_VALKEY_PUBLIC_ADDRESS "$public_address"
    replace_value MANAGED_VALKEY_DOCKER_HOST "$public_node"
    printf '%s\t%s\n' "$public_address" "$public_node"
}

collect_diagnostics() {
    local target=$1 diagnostic_kubeconfig kubernetes_status=0 operator_log

    [[ -f "$target/environment.env" ]] || fail "нет состояния $target"
    set -a
    source "$target/environment.env"
    set +a
    operator_log="$DIAGNOSTICS_DIR/operator.log"
    if [[ "$MV_SUITE" == integration ]]; then
        operator_log="$DIAGNOSTICS_DIR/operator-kubernetes.log"
    fi
    mkdir -p "$DIAGNOSTICS_DIR"
    chmod 0700 "$DIAGNOSTICS_DIR"
    rm -f \
        "$DIAGNOSTICS_DIR/compose-ps.txt" \
        "$DIAGNOSTICS_DIR/k3s.log" \
        "$DIAGNOSTICS_DIR/kubernetes-unavailable.txt" \
        "$operator_log" \
        "$DIAGNOSTICS_DIR/resources.yaml" \
        "$DIAGNOSTICS_DIR/events.yaml" \
        "$DIAGNOSTICS_DIR/metadata.tsv"

    printf '%(%Y-%m-%dT%H:%M:%SZ)T\t%s\t%s\n' \
        -1 "$MV_RUN_ID" "${MV_DIAGNOSTIC_SCENARIO:-environment}" \
        >"$DIAGNOSTICS_DIR/metadata.tsv"

    timeout 10s docker compose -f "$compose_file" -f "$test_compose_file" \
        -p "$MANAGED_VALKEY_COMPOSE_PROJECT" ps --all \
        >"$DIAGNOSTICS_DIR/compose-ps.txt" 2>&1 || true
    timeout 10s docker compose -f "$compose_file" -f "$test_compose_file" \
        -p "$MANAGED_VALKEY_COMPOSE_PROJECT" logs --no-color --tail 500 \
        k3s-server k3s-agent-1 k3s-agent-2 k3s-agent-3 \
        >"$DIAGNOSTICS_DIR/k3s.log" 2>&1 || true
    diagnostic_kubeconfig=${MANAGED_VALKEY_DIAGNOSTICS_KUBECONFIG:-$ADMIN_KUBECONFIG}
    if [[ -f "$diagnostic_kubeconfig" ]]; then
        timeout 5s env KUBECONFIG="$diagnostic_kubeconfig" \
            kubectl --request-timeout=3s get \
            nodes,pods,services,statefulsets,deployments,endpointslices,valkeyinstances.valkey.h3llo-demo.com \
            -A -o yaml >"$DIAGNOSTICS_DIR/resources.yaml" 2>/dev/null || kubernetes_status=$?
        if ((kubernetes_status == 0)); then
            timeout 5s env KUBECONFIG="$diagnostic_kubeconfig" \
                kubectl --request-timeout=3s get events -A -o yaml \
                >"$DIAGNOSTICS_DIR/events.yaml" 2>/dev/null || true
            timeout 5s env KUBECONFIG="$diagnostic_kubeconfig" \
                kubectl --request-timeout=3s logs -n valkey-system \
                -l 'app.kubernetes.io/name in (managed-valkey-operator,operator-integration)' \
                --all-containers --prefix --tail=500 \
                >"$operator_log" 2>/dev/null || true
        else
            rm -f "$DIAGNOSTICS_DIR/resources.yaml"
            printf 'Kubernetes API недоступен; снимок не сохранён; status=%d\n' "$kubernetes_status" \
                >"$DIAGNOSTICS_DIR/kubernetes-unavailable.txt"
        fi
    else
        printf 'Kubeconfig диагностики отсутствует; снимок не сохранён\n' \
            >"$DIAGNOSTICS_DIR/kubernetes-unavailable.txt"
    fi
    log "диагностика: $DIAGNOSTICS_DIR"
}

cleanup_routes() {
    local destination gateway current cleanup_status=0

    [[ -f "$ROUTES_FILE" ]] || return 0
    while IFS=$'\t' read -r destination gateway; do
        [[ -n "$destination" && -n "$gateway" ]] || continue
        current=$(ip route show exact "$destination" 2>/dev/null || true)
        if [[ "$current" == *"via $gateway"* ]]; then
            sudo ip route del "$destination" via "$gateway" || cleanup_status=1
        fi
    done <"$ROUTES_FILE"
    return "$cleanup_status"
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

report_remaining_compose_resources() {
    local kind=$1 id actual_project name
    local -a ids=()

    case "$kind" in
    container)
        mapfile -t ids < <(docker ps -aq \
            --filter "label=com.docker.compose.project=$MANAGED_VALKEY_COMPOSE_PROJECT" 2>/dev/null || true)
        ;;
    network)
        mapfile -t ids < <(docker network ls -q \
            --filter "label=com.docker.compose.project=$MANAGED_VALKEY_COMPOSE_PROJECT" 2>/dev/null || true)
        ;;
    volume)
        mapfile -t ids < <(docker volume ls -q \
            --filter "label=com.docker.compose.project=$MANAGED_VALKEY_COMPOSE_PROJECT" 2>/dev/null || true)
        ;;
    esac
    for id in "${ids[@]}"; do
        [[ -n "$id" ]] || continue
        case "$kind" in
        container)
            actual_project=$(docker inspect "$id" \
                --format '{{index .Config.Labels "com.docker.compose.project"}}' 2>/dev/null || true)
            name=$(docker inspect "$id" --format '{{.Name}}' 2>/dev/null || true)
            ;;
        network)
            actual_project=$(docker network inspect "$id" \
                --format '{{index .Labels "com.docker.compose.project"}}' 2>/dev/null || true)
            name=$(docker network inspect "$id" --format '{{.Name}}' 2>/dev/null || true)
            ;;
        volume)
            actual_project=$(docker volume inspect "$id" \
                --format '{{index .Labels "com.docker.compose.project"}}' 2>/dev/null || true)
            name=$(docker volume inspect "$id" --format '{{.Name}}' 2>/dev/null || true)
            ;;
        esac
        [[ "$actual_project" == "$MANAGED_VALKEY_COMPOSE_PROJECT" ]] || continue
        log "остаток $kind: id=$id name=${name:-unknown}"
        remaining_resources=$((remaining_resources + 1))
    done
}

report_remaining_routes() {
    local destination gateway current

    [[ -f "$ROUTES_FILE" ]] || return 0
    while IFS=$'\t' read -r destination gateway; do
        [[ -n "$destination" && -n "$gateway" ]] || continue
        current=$(ip route show exact "$destination" 2>/dev/null || true)
        if [[ "$current" == *"via $gateway"* ]]; then
            log "остаток route: destination=$destination gateway=$gateway"
            remaining_resources=$((remaining_resources + 1))
        fi
    done <"$ROUTES_FILE"
}

report_remaining_processes() {
    local target=$1 pid command start_ticks process_boot actual_start current_boot

    [[ -f "$target/processes.tsv" ]] || return 0
    current_boot=$(tr -d '\n' </proc/sys/kernel/random/boot_id)
    while IFS=$'\t' read -r pid command start_ticks process_boot; do
        [[ "$pid" =~ ^[1-9][0-9]*$ && "$start_ticks" =~ ^[0-9]+$ ]] || continue
        [[ "$process_boot" == "$current_boot" ]] || continue
        actual_start=$("$repo_root/scripts/test_resources.sh" start-ticks "$pid" 2>/dev/null || true)
        [[ "$actual_start" == "$start_ticks" ]] || continue
        log "остаток process: pid=$pid command=${command:-unknown}"
        remaining_resources=$((remaining_resources + 1))
    done <"$target/processes.tsv"
}

report_remaining_resources() {
    local target=$1

    remaining_resources=0
    report_remaining_compose_resources container
    report_remaining_compose_resources network
    report_remaining_compose_resources volume
    report_remaining_routes
    report_remaining_processes "$target"
    if ((remaining_resources == 0)); then
        log "проверка не нашла оставшихся ресурсов запуска: $target"
    fi
}

cleanup() {
    local target=$1 cleanup_status=0 network
    local -a compose_options=()

    [[ -f "$target/environment.env" ]] || fail "нет состояния $target"
    set -a
    source "$target/environment.env"
    set +a

    if [[ "$MV_SUITE" == operator ]]; then
        "$repo_root/scripts/operator_network_fault.sh" cleanup "$target" || cleanup_status=1
    fi
    cleanup_routes || cleanup_status=1
    remove_test_clients || cleanup_status=1
    if [[ "$MV_SUITE" == operator ]]; then
        compose_options+=(--profile operator-four-node)
    fi
    compose "${compose_options[@]}" down --volumes --remove-orphans || cleanup_status=1
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
        report_remaining_resources "$target" || true
    fi
    return "$cleanup_status"
}

run_in_environment() {
    local suite=$1 run_id=$2 state command_status=0 cleanup_status=0 child_pid=0 child_start
    local signal_status=0 group_status=0 process_boot run_started_seconds=$SECONDS
    shift 2
    [[ "${1:-}" == -- ]] || fail "после окружения ожидается --"
    shift
    (($# > 0)) || fail "команда тестов не задана"

    require_command ps setsid
    reserve_environment_resources "$suite" "$run_id"
    trap 'release_environment_resources' EXIT
    prepare "$suite" "$run_id"
    trap - ERR
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

    process_group_active() {
        ps -eo pgid=,stat= | awk -v group="$child_pid" '
            $1 == group && $2 !~ /^Z/ { active = 1 }
            END { exit !active }
        '
    }

    wait_for_child_group() {
        local grace_seconds=${MV_TEST_CANCEL_GRACE_SECONDS:-30} deadline killed=0

        [[ "$grace_seconds" =~ ^[0-9]+$ ]] || fail "MV_TEST_CANCEL_GRACE_SECONDS должно быть целым неотрицательным числом"
        deadline=$((SECONDS + grace_seconds))
        while process_group_active; do
            if ((SECONDS >= deadline && killed == 0)); then
                kill -KILL -- "-$child_pid" 2>/dev/null || true
                killed=1
            fi
            sleep 0.1
        done
        wait "$child_pid" 2>/dev/null || true
        ((killed == 0))
    }

    trap 'stop_child 130' INT
    trap 'stop_child 143' TERM

    setsid "$@" &
    child_pid=$!
    if ((signal_status != 0)); then
        kill -TERM -- "-$child_pid" 2>/dev/null || true
    fi
    process_boot=$(tr -d '\n' </proc/sys/kernel/random/boot_id)
    if child_start=$("$repo_root/scripts/test_resources.sh" start-ticks "$child_pid"); then
        printf '%s\t%s\t%s\t%s\n' \
            "$child_pid" "${1##*/}" "$child_start" "$process_boot" >>"$state/processes.tsv"
    else
        printf '%s\t%s\n' "$child_pid" "${1##*/}" >>"$state/processes.tsv"
    fi
    wait "$child_pid" || command_status=$?
    wait_for_child_group || group_status=$?
    ((group_status == 0)) || command_status=$group_status
    ((signal_status == 0)) || command_status=$signal_status

    if ((command_status != 0)); then
        collect_diagnostics "$state" || true
    fi
    cleanup "$state" || cleanup_status=$?
    release_environment_resources
    if [[ "$suite" == integration ]]; then
        report_integration_duration "$run_started_seconds" || cleanup_status=$?
    fi
    trap - EXIT
    ((signal_status == 0)) || command_status=$signal_status
    trap - INT TERM
    log "журнал окружения: $state"

    if ((command_status != 0)); then
        return "$command_status"
    fi
    return "$cleanup_status"
}

usage() {
    echo "usage: $0 prepare <api|operator|integration|e2e> [run-id]" >&2
    echo "       $0 expand-operator <state-dir> <operator-image> <valkey-image>" >&2
    echo "       $0 refresh-operator-public <state-dir>" >&2
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
    trap - INT TERM
    ;;
expand-operator)
    (($# == 4)) || usage
    expand_operator_cluster "$2" "$3" "$4"
    ;;
refresh-operator-public)
    (($# == 2)) || usage
    refresh_operator_public_address "$2"
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
