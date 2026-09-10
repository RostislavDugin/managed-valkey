#!/usr/bin/env bash
set -euo pipefail

state_dir=${MV_TEST_RESOURCE_STATE_DIR:-${XDG_RUNTIME_DIR:-/tmp}/managed-valkey-test-resources-$UID}
budget_mib=${MV_TEST_MEMORY_BUDGET_MIB:-28672}
minimum_available_mib=${MV_TEST_MIN_AVAILABLE_MIB:-3072}
reservations_file="$state_dir/reservations.tsv"
lock_file="$state_dir/lock"

fail() {
    echo "test-resources: $*" >&2
    exit 2
}

validate_name() {
    [[ "$1" =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$ ]] || fail "недопустимое имя $1"
}

validate_number() {
    [[ "$2" =~ ^[0-9]+$ ]] || fail "$1 должно быть целым неотрицательным числом"
}

boot_id() {
    tr -d '\n' </proc/sys/kernel/random/boot_id
}

process_start_ticks() {
    local pid=$1

    [[ -r "/proc/$pid/stat" ]] || return 1
    sed -E 's/^.*\) //' "/proc/$pid/stat" | awk '{ print $20 }'
}

process_matches() {
    local pid=$1 expected_start=$2 expected_boot=$3 actual_start

    [[ "$expected_boot" == "$(boot_id)" ]] || return 1
    actual_start=$(process_start_ticks "$pid") || return 1
    [[ "$actual_start" == "$expected_start" ]]
}

available_mib() {
    awk '$1 == "MemAvailable:" { print int($2 / 1024); exit }' /proc/meminfo
}

initialize() {
    mkdir -p "$state_dir"
    chmod 0700 "$state_dir"
    touch "$reservations_file" "$lock_file"
    chmod 0600 "$reservations_file" "$lock_file"
}

reap_locked() {
    local temporary owner suite memory pid start_ticks owner_boot

    temporary=$(mktemp "$state_dir/reservations.XXXXXX")
    while IFS=$'\t' read -r owner suite memory pid start_ticks owner_boot; do
        [[ -n "$owner" ]] || continue
        if process_matches "$pid" "$start_ticks" "$owner_boot"; then
            printf '%s\t%s\t%s\t%s\t%s\t%s\n' \
                "$owner" "$suite" "$memory" "$pid" "$start_ticks" "$owner_boot" >>"$temporary"
        fi
    done <"$reservations_file"
    chmod 0600 "$temporary"
    mv "$temporary" "$reservations_file"
}

reserved_mib_locked() {
    awk -F '\t' '{ total += $3 } END { print total + 0 }' "$reservations_file"
}

reserve_once() {
    local owner=$1 suite=$2 memory=$3 pid=$4 start_ticks=$5 owner_boot=$6
    local total available existing

    exec 9>"$lock_file"
    flock 9
    reap_locked
    existing=$(awk -F '\t' -v owner="$owner" '$1 == owner { print; exit }' "$reservations_file")
    if [[ -n "$existing" ]]; then
        [[ "$existing" == "$owner"$'\t'"$suite"$'\t'"$memory"$'\t'"$pid"$'\t'"$start_ticks"$'\t'"$owner_boot" ]] ||
            fail "резерв $owner уже принадлежит другому процессу или имеет другой размер"
        return 0
    fi
    total=$(reserved_mib_locked)
    available=$(available_mib)
    if ((total + memory <= budget_mib && available >= memory + minimum_available_mib)); then
        printf '%s\t%s\t%s\t%s\t%s\t%s\n' \
            "$owner" "$suite" "$memory" "$pid" "$start_ticks" "$owner_boot" >>"$reservations_file"
        return 0
    fi
    return 1
}

reserve() {
    local owner=$1 suite=$2 memory=$3 pid=$4 start_ticks=$5 wait_seconds=${6:-600}
    local owner_boot deadline

    validate_name "$owner"
    validate_name "$suite"
    validate_number memory "$memory"
    validate_number pid "$pid"
    validate_number start_ticks "$start_ticks"
    validate_number wait_seconds "$wait_seconds"
    ((memory > 0)) || fail "memory должно быть больше нуля"
    ((memory <= budget_mib)) || fail "запрошено $memory MiB при бюджете $budget_mib MiB"
    process_matches "$pid" "$start_ticks" "$(boot_id)" || fail "процесс-владелец $pid не найден"
    owner_boot=$(boot_id)
    deadline=$((SECONDS + wait_seconds))
    while ! reserve_once "$owner" "$suite" "$memory" "$pid" "$start_ticks" "$owner_boot"; do
        if ((SECONDS >= deadline)); then
            echo "test-resources: не удалось выделить $memory MiB для $owner за ${wait_seconds}s" >&2
            return 1
        fi
        sleep 1
    done
}

release() {
    local owner=$1 temporary

    validate_name "$owner"
    exec 9>"$lock_file"
    flock 9
    reap_locked
    temporary=$(mktemp "$state_dir/reservations.XXXXXX")
    awk -F '\t' -v owner="$owner" '$1 != owner' "$reservations_file" >"$temporary"
    chmod 0600 "$temporary"
    mv "$temporary" "$reservations_file"
}

show_status() {
    local total available

    exec 9>"$lock_file"
    flock 9
    reap_locked
    total=$(reserved_mib_locked)
    available=$(available_mib)
    printf 'budget_mib\t%s\nminimum_available_mib\t%s\navailable_mib\t%s\nreserved_mib\t%s\n' \
        "$budget_mib" "$minimum_available_mib" "$available" "$total"
    cat "$reservations_file"
}

usage() {
    echo "usage: $0 reserve <owner> <suite> <memory-mib> <pid> <start-ticks> [wait-seconds]" >&2
    echo "       $0 release <owner>" >&2
    echo "       $0 reap|status|start-ticks [pid]" >&2
    exit 2
}

initialize
case ${1:-} in
reserve)
    (($# == 6 || $# == 7)) || usage
    reserve "$2" "$3" "$4" "$5" "$6" "${7:-600}"
    ;;
release)
    (($# == 2)) || usage
    release "$2"
    ;;
reap)
    (($# == 1)) || usage
    exec 9>"$lock_file"
    flock 9
    reap_locked
    ;;
status)
    (($# == 1)) || usage
    show_status
    ;;
start-ticks)
    (($# == 1 || $# == 2)) || usage
    process_start_ticks "${2:-$PPID}"
    ;;
*) usage ;;
esac
