#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
catalog=$temporary/catalog.tsv
events=$temporary/events
active=$temporary/active
maximum=$temporary/maximum

printf '%s\n' \
    $'scenario\ttest_pattern\tnodes\tenvoy_replicas\tmemory_mib\testimated_seconds\ttimeout' \
    $'first\t^TestFirst$\t1\t1\t100\t40\t1m' \
    $'second\t^TestSecond$\t2\t1\t100\t30\t1m' \
    $'third\t^TestThird$\t3\t1\t100\t20\t1m' \
    $'fourth\t^TestFourth$\t4\t2\t100\t10\t1m' >"$catalog"
: >"$events"
printf '0\n' >"$active"
printf '0\n' >"$maximum"

write_script() {
    local path=$1
    shift
    printf '%s\n' '#!/usr/bin/env bash' 'set -euo pipefail' "$@" >"$path"
    chmod +x "$path"
}

write_script "$temporary/resources" \
    'case "$1" in' \
    'start-ticks) printf '\''1\n'\'' ;;' \
    'status) printf '\''budget_mib\t1000\nminimum_available_mib\t0\navailable_mib\t1000\nreserved_mib\t0\n'\'' ;;' \
    'reserve) printf '\''reserve\n'\'' >>"$MV_TEST_EVENTS" ;;' \
    'release) printf '\''release\n'\'' >>"$MV_TEST_EVENTS" ;;' \
    '*) exit 2 ;;' \
    'esac'
write_script "$temporary/cleanup" \
    'printf '\''cleanup\n'\'' >>"$MV_TEST_EVENTS"' \
    '[[ "${MV_FAIL_CLEANUP:-0}" != 1 ]]'
write_script "$temporary/validator" 'printf '\''validate\n'\'' >>"$MV_TEST_EVENTS"'
write_script "$temporary/prepare" \
    'printf '\''prepare:%s\n'\'' "$1" >>"$MV_TEST_EVENTS"' \
    '[[ "$1" != fast ]] || { printf '\''PASS\n'\'' >"$2/unit.log"; printf '\''PASS\n'\'' >"$2/envtest.log"; }' \
    'sleep 0.05'
write_script "$temporary/job" \
    'scenario=$3' \
    'results_root=${12}' \
    'exec 9>"$MV_TEST_COUNTER_LOCK"' \
    'flock 9' \
    'current=$(<"$MV_TEST_ACTIVE")' \
    'current=$((current + 1))' \
    'printf '\''%s\n'\'' "$current" >"$MV_TEST_ACTIVE"' \
    'peak=$(<"$MV_TEST_MAXIMUM")' \
    '((current <= peak)) || printf '\''%s\n'\'' "$current" >"$MV_TEST_MAXIMUM"' \
    'printf '\''start:%s\n'\'' "$scenario" >>"$MV_TEST_EVENTS"' \
    'flock -u 9' \
    'sleep 0.2' \
    'flock 9' \
    'current=$(<"$MV_TEST_ACTIVE")' \
    'printf '\''%s\n'\'' "$((current - 1))" >"$MV_TEST_ACTIVE"' \
    'flock -u 9' \
    'attempt="$results_root/$scenario/attempts/1"' \
    'mkdir -p "$attempt"' \
    'printf '\''pass\n'\'' >"$attempt/status"' \
    'printf -- '\''--- PASS: Test%s\n'\'' "${scenario^}" >"$attempt/log"' \
    'printf '\''1\n'\'' >"$attempt/duration"'
write_script "$temporary/network" \
    'exec 9>"$MV_TEST_COUNTER_LOCK"' \
    'flock 9' \
    'current=$(<"$MV_TEST_ACTIVE")' \
    'current=$((current + 1))' \
    'printf '\''%s\n'\'' "$current" >"$MV_TEST_ACTIVE"' \
    'peak=$(<"$MV_TEST_MAXIMUM")' \
    '((current <= peak)) || printf '\''%s\n'\'' "$current" >"$MV_TEST_MAXIMUM"' \
    'flock -u 9' \
    'sleep 0.2' \
    'flock 9' \
    'current=$(<"$MV_TEST_ACTIVE")' \
    'printf '\''%s\n'\'' "$((current - 1))" >"$MV_TEST_ACTIVE"' \
    'flock -u 9' \
    'printf '\''CT-12: pass\n'\'' >"$6/network-faults.log"'
write_script "$temporary/reporter" \
    '[[ "$2" == full ]]' \
    '[[ -s "$1/unit.log" && -s "$1/envtest.log" && -s "$1/network-faults.log" ]]' \
    '[[ $(find "$1/scenarios" -name status -exec cat {} \; | grep -c '^\''pass$'\'') == 4 ]]'

export MV_TEST_EVENTS=$events
export MV_TEST_ACTIVE=$active
export MV_TEST_MAXIMUM=$maximum
export MV_TEST_COUNTER_LOCK=$temporary/counter.lock
export MV_OPERATOR_TEST_CATALOG=$catalog
export MV_OPERATOR_TEST_VALIDATOR=$temporary/validator
export MV_TEST_RESOURCES_SCRIPT=$temporary/resources
export MV_TEST_CLEANUP_SCRIPT=$temporary/cleanup
export MV_OPERATOR_TEST_PREPARE_SCRIPT=$temporary/prepare
export MV_OPERATOR_TEST_SCENARIO_SCRIPT=$temporary/job
export MV_OPERATOR_TEST_NETWORK_SCRIPT=$temporary/network
export MV_OPERATOR_TEST_REPORTER=$temporary/reporter
export MV_OPERATOR_TEST_RUN_DIR=$temporary/run
export MV_OPERATOR_TEST_MAX_PARALLEL=3

"$repo_root/scripts/test_operator.sh" >/dev/null
[[ $(grep -c '^prepare:fast$' "$events") == 1 ]]
[[ $(grep -c '^prepare:artifacts$' "$events") == 1 ]]
[[ $(grep -c '^prepare:valkey$' "$events") == 1 ]]
[[ $(grep -c '^prepare:bundle$' "$events") == 1 ]]
[[ $(grep -c '^start:' "$events") == 4 ]]
[[ $(grep -c '^reserve$' "$events") == 1 ]]
[[ $(grep -c '^release$' "$events") == 1 ]]
[[ $(<"$maximum") -ge 2 ]]
[[ $(<"$maximum") -le 3 ]]
[[ $(sed -n '1p' "$events") == cleanup ]]

: >"$events"
write_script "$temporary/validator" 'exit 7'
export MV_OPERATOR_TEST_RUN_DIR=$temporary/validator-failure
if "$repo_root/scripts/test_operator.sh" >/dev/null 2>&1; then
    echo "ошибка проверки каталога была потеряна" >&2
    exit 1
fi
! rg -q '^prepare:' "$events"
! rg -q '^start:' "$events"

: >"$events"
write_script "$temporary/validator" 'printf '\''validate\n'\'' >>"$MV_TEST_EVENTS"'
export MV_FAIL_CLEANUP=1
export MV_OPERATOR_TEST_RUN_DIR=$temporary/cleanup-failure
if "$repo_root/scripts/test_operator.sh" >/dev/null 2>&1; then
    echo "ошибка начальной очистки была потеряна" >&2
    exit 1
fi
! rg -q '^validate$' "$events"
! rg -q '^prepare:' "$events"

unset MV_FAIL_CLEANUP
write_script "$temporary/reporter" 'exit 0'
write_script "$temporary/prepare" \
    'printf '\''prepare:%s\n'\'' "$1" >>"$MV_TEST_EVENTS"' \
    'if [[ "$1" == fast ]]; then' \
    '    printf '\''prepare-pid:%s:%s\n'\'' "$1" "$$" >>"$MV_TEST_EVENTS"' \
    '    trap '\''printf "stopped:%s\n" "$1" >>"$MV_TEST_EVENTS"; exit 143'\'' TERM' \
    '    while true; do sleep 1; done' \
    'fi' \
    '[[ "$1" != artifacts ]] || exit 7'
: >"$events"
export MV_OPERATOR_TEST_RUN_DIR=$temporary/preparation-failure
if "$repo_root/scripts/test_operator.sh" >"$temporary/preparation-failure.out" 2>&1; then
    echo "ошибка подготовки была потеряна" >&2
    exit 1
else
    command_status=$?
fi
[[ "$command_status" == 7 ]]
failed_fast_pid=$(awk -F: '$1 == "prepare-pid" && $2 == "fast" { print $3 }' "$events")
[[ -n "$failed_fast_pid" ]]
! kill -0 "$failed_fast_pid" 2>/dev/null
[[ $(grep -c '^stopped:fast$' "$events") == 1 ]]
[[ $(grep -c '^release$' "$events") == 1 ]]

write_script "$temporary/prepare" \
    'printf '\''prepare:%s\nprepare-pid:%s:%s\n'\'' "$1" "$1" "$$" >>"$MV_TEST_EVENTS"' \
    'if [[ "$1" == artifacts ]]; then' \
    '    trap '\'''\'' TERM' \
    'else' \
    '    trap '\''printf "stopped:%s\n" "$1" >>"$MV_TEST_EVENTS"; exit 143'\'' TERM' \
    'fi' \
    'while true; do sleep 1; done'
: >"$events"
export MV_OPERATOR_TEST_RUN_DIR=$temporary/preparation-cancel
export MV_OPERATOR_CANCEL_GRACE_SECONDS=1
"$repo_root/scripts/test_operator.sh" >"$temporary/preparation-cancel.out" 2>&1 &
operator_pid=$!
for _ in {1..100}; do
    [[ $(grep -c '^prepare-pid:' "$events" || true) == 3 ]] && break
    sleep 0.05
done
[[ $(grep -c '^prepare-pid:' "$events" || true) == 3 ]]
kill -TERM "$operator_pid"
set +e
wait "$operator_pid"
command_status=$?
set -e
[[ "$command_status" == 143 ]]
while IFS=: read -r _ _ preparation_pid; do
    ! kill -0 "$preparation_pid" 2>/dev/null
done < <(grep '^prepare-pid:' "$events")
[[ $(grep -c '^stopped:' "$events") == 2 ]]
[[ $(grep -c '^release$' "$events") == 1 ]]
