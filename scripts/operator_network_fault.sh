#!/usr/bin/env bash
set -Eeuo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)

fail() {
    echo "operator-network-fault: $*" >&2
    exit 1
}

scenario_for_fault() {
    local fault_id=$1

    [[ "$fault_id" =~ ^([a-z]{2})([0-9]{2})- ]] || fail "ID отказа не содержит ID матрицы"
    printf '%s-%s\n' "${BASH_REMATCH[1]^^}" "${BASH_REMATCH[2]}"
}

record_event() {
    local fault_id=$1 action=$2 target=$3 scenario

    scenario=$(scenario_for_fault "$fault_id")
    "$script_dir/record_operator_fault.sh" "$MV_STATE_DIR" "$scenario" "$action" "$target"
}

load_state() {
    local target=$1

    [[ -f "$target/environment.env" ]] || fail "нет состояния $target"
    set -a
    source "$target/environment.env"
    set +a
    NETWORK_FAULTS_FILE=${NETWORK_FAULTS_FILE:-$target/network-faults.tsv}
    export NETWORK_FAULTS_FILE
    [[ "$MV_SUITE" == operator ]] || fail "сетевые отказы разрешены только стенду оператора"
    [[ "$MV_STATE_DIR" == "$target" ]] || fail "каталог состояния не совпал с окружением"
    [[ "$NETWORK_FAULTS_FILE" == "$target/network-faults.tsv" ]] ||
        fail "файл сетевых отказов не принадлежит стенду"
    touch "$NETWORK_FAULTS_FILE"
    chmod 0600 "$NETWORK_FAULTS_FILE"
}

validate_fault_id() {
    [[ "$1" =~ ^[a-z0-9][a-z0-9-]{0,63}$ ]] || fail "неверный ID сетевого отказа"
}

validate_service() {
    case "$1" in
    k3s-server | k3s-agent-1 | k3s-agent-2 | k3s-agent-3) ;;
    *) fail "неверная k3s-нода $1" ;;
    esac
}

validate_chain() {
    case "$1" in
    FORWARD | OUTPUT) ;;
    *) fail "неверная цепочка iptables $1" ;;
    esac
}

validate_cidr() {
    python3 - "$1" <<'PY'
import ipaddress
import sys

ipaddress.ip_network(sys.argv[1], strict=False)
PY
}

validate_port() {
    [[ "$1" =~ ^[0-9]+$ ]] && ((1 <= 10#$1 && 10#$1 <= 65535)) ||
        fail "неверный TCP-порт $1"
}

node_container() {
    local service=$1 output

    output=$(docker ps -q \
        --filter "label=com.docker.compose.project=$MANAGED_VALKEY_COMPOSE_PROJECT" \
        --filter "label=com.docker.compose.service=$service")
    [[ $(wc -w <<<"$output") -eq 1 ]] || fail "не найден работающий контейнер $service"
    docker inspect "$output" --format '{{index .Config.Labels "com.docker.compose.project"}} {{index .Config.Labels "com.docker.compose.service"}} {{.State.Running}}' |
        grep -Fxq "$MANAGED_VALKEY_COMPOSE_PROJECT $service true" ||
        fail "контейнер $service не принадлежит стенду"
    printf '%s\n' "$output"
}

rule_arguments() {
    local operation=$1 chain=$2 source=$3 destination=$4 port=$5 comment=$6

    RULE_ARGUMENTS=("-$operation" "$chain" -p tcp -s "$source" -d "$destination" \
        --dport "$port" -m comment --comment "$comment" -j DROP)
}

record_fault() {
    local record=$1

    exec 9>>"$NETWORK_FAULTS_FILE.lock"
    flock 9
    grep -Fxq "$record" "$NETWORK_FAULTS_FILE" || printf '%s\n' "$record" >>"$NETWORK_FAULTS_FILE"
    flock -u 9
}

forget_fault() {
    local comment=$1 temporary

    exec 9>>"$NETWORK_FAULTS_FILE.lock"
    flock 9
    temporary=$(mktemp "${NETWORK_FAULTS_FILE}.XXXXXX")
    awk -F '\t' -v comment="$comment" '$6 != comment' "$NETWORK_FAULTS_FILE" >"$temporary"
    chmod 0600 "$temporary"
    mv "$temporary" "$NETWORK_FAULTS_FILE"
    flock -u 9
}

remove_record() {
    local service=$1 chain=$2 source=$3 destination=$4 port=$5 comment=$6 container fault_id

    [[ "$comment" == "managed-valkey:$MV_RUN_ID:"* ]] ||
        fail "правило $comment не принадлежит запуску"
    container=$(docker ps -q \
        --filter "label=com.docker.compose.project=$MANAGED_VALKEY_COMPOSE_PROJECT" \
        --filter "label=com.docker.compose.service=$service")
    if [[ $(wc -w <<<"$container") -eq 1 ]]; then
        rule_arguments C "$chain" "$source" "$destination" "$port" "$comment"
        if docker exec "$container" iptables -w 5 "${RULE_ARGUMENTS[@]}" >/dev/null 2>&1; then
            rule_arguments D "$chain" "$source" "$destination" "$port" "$comment"
            docker exec "$container" iptables -w 5 "${RULE_ARGUMENTS[@]}" >/dev/null
        fi
    fi
    fault_id=${comment##*:}
    record_event "$fault_id" network-removed "$service:$chain:$source>$destination:$port"
    forget_fault "$comment"
}

add_fault() {
    local target=$1 fault_id=$2 service=$3 chain=$4 source=$5 destination=$6 port=$7
    local comment container record

    load_state "$target"
    validate_fault_id "$fault_id"
    validate_service "$service"
    validate_chain "$chain"
    validate_cidr "$source"
    validate_cidr "$destination"
    validate_port "$port"
    comment="managed-valkey:$MV_RUN_ID:$fault_id"
    container=$(node_container "$service")
    record=$(printf '%s\t%s\t%s\t%s\t%s\t%s' \
        "$service" "$chain" "$source" "$destination" "$port" "$comment")
    record_fault "$record"
    rule_arguments C "$chain" "$source" "$destination" "$port" "$comment"
    if docker exec "$container" iptables -w 5 "${RULE_ARGUMENTS[@]}" >/dev/null 2>&1; then
        return
    fi
    rule_arguments I "$chain" "$source" "$destination" "$port" "$comment"
    docker exec "$container" iptables -w 5 "${RULE_ARGUMENTS[@]}" >/dev/null
    record_event "$fault_id" network-applied "$service:$chain:$source>$destination:$port"
}

remove_fault() {
    local target=$1 fault_id=$2 comment
    local -a records=()

    load_state "$target"
    validate_fault_id "$fault_id"
    comment="managed-valkey:$MV_RUN_ID:$fault_id"
    mapfile -t records < <(awk -F '\t' -v comment="$comment" '$6 == comment' "$NETWORK_FAULTS_FILE")
    for record in "${records[@]}"; do
        IFS=$'\t' read -r service chain source destination port stored_comment <<<"$record"
        remove_record "$service" "$chain" "$source" "$destination" "$port" "$stored_comment"
    done
}

check_fault() {
    local target=$1 fault_id=$2 expected=$3 comment container found=0

    load_state "$target"
    validate_fault_id "$fault_id"
    [[ "$expected" == present || "$expected" == absent ]] || fail "ожидалось present или absent"
    comment="managed-valkey:$MV_RUN_ID:$fault_id"
    while IFS=$'\t' read -r service chain source destination port stored_comment; do
        [[ "$stored_comment" == "$comment" ]] || continue
        container=$(node_container "$service")
        rule_arguments C "$chain" "$source" "$destination" "$port" "$stored_comment"
        if docker exec "$container" iptables -w 5 "${RULE_ARGUMENTS[@]}" >/dev/null 2>&1; then
            found=1
        fi
    done <"$NETWORK_FAULTS_FILE"
    if [[ "$expected" == present ]]; then
        ((found == 1)) || fail "сетевое ограничение $fault_id не установлено"
    else
        ((found == 0)) || fail "сетевое ограничение $fault_id осталось установлено"
    fi
}

cleanup_faults() {
    local target=$1
    local -a records=()

    load_state "$target"
    mapfile -t records <"$NETWORK_FAULTS_FILE"
    for record in "${records[@]}"; do
        [[ -n "$record" ]] || continue
        IFS=$'\t' read -r service chain source destination port comment <<<"$record"
        validate_service "$service"
        validate_chain "$chain"
        validate_cidr "$source"
        validate_cidr "$destination"
        validate_port "$port"
        remove_record "$service" "$chain" "$source" "$destination" "$port" "$comment"
    done
}

run_with_cleanup() {
    local target=$1 command_status=0 child_pid=0 signal_status=0
    shift
    [[ "${1:-}" == -- ]] || fail "после состояния ожидается --"
    shift
    (($# > 0)) || fail "команда не задана"
    load_state "$target"

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
    wait "$child_pid" || command_status=$?
    ((signal_status == 0)) || command_status=$signal_status
    trap - INT TERM
    cleanup_faults "$target"
    return "$command_status"
}

usage() {
    echo "usage: $0 add <state-dir> <id> <node> <FORWARD|OUTPUT> <source-cidr> <destination-cidr> <port>" >&2
    echo "       $0 remove|check <state-dir> <id> [present|absent]" >&2
    echo "       $0 cleanup <state-dir>" >&2
    echo "       $0 run <state-dir> -- <command> [args...]" >&2
    exit 2
}

command_name=${1:-}
case "$command_name" in
add)
    (($# == 8)) || usage
    add_fault "$2" "$3" "$4" "$5" "$6" "$7" "$8"
    ;;
remove)
    (($# == 3)) || usage
    remove_fault "$2" "$3"
    ;;
check)
    (($# == 4)) || usage
    check_fault "$2" "$3" "$4"
    ;;
cleanup)
    (($# == 2)) || usage
    cleanup_faults "$2"
    ;;
run)
    (($# >= 4)) || usage
    target=$2
    shift 2
    run_with_cleanup "$target" "$@"
    ;;
*) usage ;;
esac
