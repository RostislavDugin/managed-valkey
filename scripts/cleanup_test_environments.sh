#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
state_root=${MV_TEST_STATE_ROOT:-$repo_root/tmp/k3s}
environment_script=${MV_TEST_ENVIRONMENT_SCRIPT:-$repo_root/scripts/test_environment.sh}
suite_filter=${1:-}
current_boot=$(tr -d '\n' </proc/sys/kernel/random/boot_id)
failed=0

fail() {
    echo "cleanup-test-environments: $*" >&2
    failed=1
}

process_start_ticks() {
    local pid=$1

    [[ -r "/proc/$pid/stat" ]] || return 1
    sed -E 's/^.*\) //' "/proc/$pid/stat" | awk '{ print $20 }'
}

owner_is_active() {
    local state=$1 pid start_ticks boot actual_start

    pid=$(<"$state/owner.pid")
    start_ticks=$(<"$state/owner.start-ticks")
    boot=$(<"$state/owner.boot-id")
    [[ "$pid" =~ ^[1-9][0-9]*$ && "$start_ticks" =~ ^[0-9]+$ ]] || return 1
    [[ "$boot" == "$current_boot" ]] || return 1
    actual_start=$(process_start_ticks "$pid") || return 1
    [[ "$actual_start" == "$start_ticks" ]]
}

ownership_is_proven() {
    local state=$1 expected_suite=$2 expected_run_id=$3

    [[ -f "$state/environment.env" ]] || return 1
    (
        set -a
        source "$state/environment.env"
        set +a
        [[ "${MV_SUITE:-}" == "$expected_suite" ]]
        [[ "${MV_RUN_ID:-}" == "$expected_run_id" ]]
        [[ "${MV_STATE_DIR:-}" == "$state" ]]
        [[ "${MANAGED_VALKEY_COMPOSE_PROJECT:-}" == managed-valkey-"$expected_suite"-* ]]
        [[ "${MANAGED_VALKEY_COMPOSE_PROJECT:-}" != managed-valkey-dev ]]
    )
}

cleanup_state() {
    local state=$1 suite=$2 run_id=$3 status

    status=$(cat "$state/status" 2>/dev/null || true)
    [[ "$status" != cleaned ]] || return 0
    if [[ ! -f "$state/owner.pid" || ! -f "$state/owner.start-ticks" || ! -f "$state/owner.boot-id" ]]; then
        fail "не трогаю $state: нет полной записи владельца"
        return
    fi
    if owner_is_active "$state"; then
        echo "cleanup-test-environments: активный запуск сохранён: $state"
        return
    fi
    if ! ownership_is_proven "$state" "$suite" "$run_id"; then
        fail "не трогаю $state: принадлежность окружения не подтверждена"
        return
    fi
    echo "cleanup-test-environments: очищаю осиротевший запуск: $state"
    if ! "$environment_script" cleanup "$state"; then
        fail "штатная очистка завершилась ошибкой: $state"
        return
    fi
    if [[ "$(cat "$state/status" 2>/dev/null || true)" != cleaned ]]; then
        fail "после очистки остался незавершённый статус: $state"
    fi
}

if [[ -n "$suite_filter" && ! "$suite_filter" =~ ^(api|operator|integration|e2e)$ ]]; then
    echo "usage: $0 [api|operator|integration|e2e]" >&2
    exit 2
fi

[[ -d "$state_root" ]] || exit 0
while IFS= read -r -d '' state; do
    suite=$(basename "$(dirname "$state")")
    run_id=$(basename "$state")
    [[ -z "$suite_filter" || "$suite" == "$suite_filter" ]] || continue
    cleanup_state "$state" "$suite" "$run_id"
done < <(find "$state_root" -mindepth 2 -maxdepth 2 -type d -print0)

exit "$failed"
