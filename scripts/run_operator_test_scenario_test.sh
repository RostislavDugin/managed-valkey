#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
events=$temporary/events
resources=$temporary/resources

printf '%s\n' \
    '#!/usr/bin/env bash' \
    'set -euo pipefail' \
    'printf "%s\n" "$1" >>"$MV_TEST_EVENTS"' \
    'case "$1" in' \
    'start-ticks) printf "1\n" ;;' \
    'reserve) exit 9 ;;' \
    'release) exit 0 ;;' \
    '*) exit 2 ;;' \
    'esac' >"$resources"
chmod +x "$resources"
: >"$events"

export MV_TEST_EVENTS=$events
export MV_TEST_RESOURCES_SCRIPT=$resources
if "$repo_root/scripts/run_operator_test_scenario.sh" \
    run environment scenario '^TestScenario$' 1 1 100 1m \
    operator:image valkey:image binary "$temporary/results" archive; then
    echo "ошибка резерва была потеряна" >&2
    exit 1
else
    command_status=$?
fi
[[ "$command_status" == 9 ]]
[[ $(grep -c '^reserve$' "$events") == 1 ]]
! rg -q '^release$' "$events"

if "$repo_root/scripts/run_operator_network_checks.sh" \
    run environment 100 operator:image valkey:image "$temporary/results" archive; then
    echo "ошибка резерва сетевого теста была потеряна" >&2
    exit 1
else
    command_status=$?
fi
[[ "$command_status" == 9 ]]
[[ $(grep -c '^reserve$' "$events") == 2 ]]
! rg -q '^release$' "$events"
