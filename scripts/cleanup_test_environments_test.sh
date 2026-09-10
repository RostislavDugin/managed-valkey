#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
state_root=$temporary/state
cleanup_log=$temporary/cleanup.log
fake_cleanup=$temporary/test-environment
boot_id=$(tr -d '\n' </proc/sys/kernel/random/boot_id)
owner_start=$("$repo_root/scripts/test_resources.sh" start-ticks "$$")

mkdir -p "$state_root/operator"
printf '%s\n' '#!/usr/bin/env bash' \
    'set -euo pipefail' \
    '[[ "$1" == cleanup ]]' \
    'printf "%s\n" "$2" >>"$MV_CLEANUP_LOG"' \
    '[[ "${MV_CLEANUP_FAIL_FOR:-}" != "$(basename "$2")" ]]' \
    'printf "cleaned\n" >"$2/status"' >"$fake_cleanup"
chmod +x "$fake_cleanup"

make_state() {
    local run_id=$1 pid=${2:-99999999} start_ticks=${3:-1} with_owner=${4:-1}
    local state="$state_root/operator/$run_id"

    mkdir -p "$state"
    printf 'allocated\n' >"$state/status"
    printf 'MV_SUITE=operator\nMV_RUN_ID=%q\nMV_STATE_DIR=%q\nMANAGED_VALKEY_COMPOSE_PROJECT=%q\n' \
        "$run_id" "$state" "managed-valkey-operator-$run_id" >"$state/environment.env"
    if ((with_owner == 1)); then
        printf '%s\n' "$pid" >"$state/owner.pid"
        printf '%s\n' "$start_ticks" >"$state/owner.start-ticks"
        printf '%s\n' "$boot_id" >"$state/owner.boot-id"
    fi
}

make_state active "$$" "$owner_start"
make_state stale
make_state reused "$$" "$((owner_start + 1))"
make_state uncertain 99999999 1 0

export MV_TEST_STATE_ROOT=$state_root
export MV_TEST_ENVIRONMENT_SCRIPT=$fake_cleanup
export MV_CLEANUP_LOG=$cleanup_log

if "$repo_root/scripts/cleanup_test_environments.sh" operator \
    >"$temporary/first.out" 2>"$temporary/first.err"; then
    echo "неопределённое окружение не привело к ошибке" >&2
    exit 1
fi
[[ "$(<"$state_root/operator/active/status")" == allocated ]]
[[ "$(<"$state_root/operator/stale/status")" == cleaned ]]
[[ "$(<"$state_root/operator/reused/status")" == cleaned ]]
[[ "$(<"$state_root/operator/uncertain/status")" == allocated ]]
rg -q 'активный запуск сохранён' "$temporary/first.out"
rg -q 'нет полной записи владельца' "$temporary/first.err"
[[ $(wc -l <"$cleanup_log") == 2 ]]

make_state cleanup-failure
export MV_CLEANUP_FAIL_FOR=cleanup-failure
if "$repo_root/scripts/cleanup_test_environments.sh" operator 2>"$temporary/failure.err"; then
    echo "ошибка штатной очистки была потеряна" >&2
    exit 1
fi
[[ "$(<"$state_root/operator/cleanup-failure/status")" == allocated ]]
rg -q 'штатная очистка завершилась ошибкой' "$temporary/failure.err"
