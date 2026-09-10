#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT

api_fast=$(just --justfile "$repo_root/api/Justfile" --dry-run test 2>&1)
[[ "$api_fast" == *'MV_TEST_K3S_ENABLED=0'* ]]
[[ "$api_fast" == *'scripts/test_api.sh fast'* ]]
api_full=$(just --justfile "$repo_root/api/Justfile" --dry-run test-full 2>&1)
[[ "$api_full" == *'MV_TEST_K3S_ENABLED=1'* ]]
[[ "$api_full" == *'scripts/test_api.sh full'* ]]
operator_recipes=$(just --justfile "$repo_root/operator/Justfile" --summary)
[[ " $operator_recipes " == *' test '* ]]
[[ " $operator_recipes " == *' test-full '* ]]
[[ " $operator_recipes " != *' test-ci '* ]]
root_recipes=$(just --justfile "$repo_root/Justfile" --summary)
[[ " $root_recipes " == *' test-e2e-prod '* ]]
[[ " $root_recipes " != *' test-prod '* ]]

create_commands() {
    local command_dir=$1 suite

    mkdir -p "$command_dir"
    for suite in api operator web; do
        printf '%s\n' \
            '#!/usr/bin/env bash' \
            'set -euo pipefail' \
            'printf '\''%s\n'\'' "$1" >"$MV_CAPTURE_DIR/'"$suite"'"' \
            'touch "$MV_CAPTURE_DIR/'"$suite"'.started"' \
            'for _ in $(seq 1 100); do' \
            '    [[ $(find "$MV_CAPTURE_DIR" -name '\''*.started'\'' | wc -l) == 3 ]] && exit 0' \
            '    sleep 0.05' \
            'done' \
            'exit 1' \
            >"$command_dir/$suite"
        chmod +x "$command_dir/$suite"
    done
}

run_mode() {
    local mode=$1 capture command_dir output run_id run_dir suite

    capture="$temporary/capture-$mode"
    command_dir="$temporary/commands-$mode"
    output="$temporary/$mode.out"
    run_id="routing-${mode//-/_}-$$"
    if [[ "$mode" == lint ]]; then
        run_dir="$repo_root/tmp/lint-runs/$run_id"
    else
        run_dir="$repo_root/tmp/test-runs/$run_id"
    fi

    mkdir -p "$capture"
    create_commands "$command_dir"
    MV_CAPTURE_DIR="$capture" \
        MANAGED_VALKEY_ROOT_RUN_ID="$run_id" \
        MANAGED_VALKEY_TEST_COMMAND_DIR="$command_dir" \
        timeout 10s "$repo_root/scripts/run_all_tests.sh" "$mode" >"$output"
    for suite in api operator web; do
        [[ "$(<"$capture/$suite")" == "$mode" ]]
    done
    ! rg -q 'e2e' "$output"
    if [[ "$mode" == test-full ]]; then
        rg -q 'integration: ещё не реализован' "$output"
    else
        ! rg -q 'integration' "$output"
    fi
    rm -rf -- "$run_dir"
}

run_mode lint
run_mode test
run_mode test-full

failure_commands="$temporary/failure-commands"
failure_capture="$temporary/failure-capture"
mkdir -p "$failure_capture"
create_commands "$failure_commands"
printf '%s\n' '#!/usr/bin/env bash' 'exit 17' >"$failure_commands/operator"
chmod +x "$failure_commands/operator"
if MV_CAPTURE_DIR="$failure_capture" \
    MANAGED_VALKEY_ROOT_RUN_ID="routing-failure-$$" \
    MANAGED_VALKEY_TEST_COMMAND_DIR="$failure_commands" \
    timeout 10s "$repo_root/scripts/run_all_tests.sh" lint >"$temporary/failure.out"; then
    echo "ошибка дочернего линтера была потеряна" >&2
    exit 1
fi
rg -q 'operator: ошибка 17' "$temporary/failure.out"
rm -rf -- "$repo_root/tmp/lint-runs/routing-failure-$$"

cancel_commands="$temporary/cancel-commands"
cancel_capture="$temporary/cancel-capture"
cancel_run_id="routing-cancel-$$"
cancel_run_dir="$repo_root/tmp/test-runs/$cancel_run_id"
mkdir -p "$cancel_commands" "$cancel_capture"
for suite in api operator web; do
    printf '%s\n' \
        '#!/usr/bin/env bash' \
        'set -euo pipefail' \
        'trap '\''exit 143'\'' TERM' \
        'touch "$MV_CAPTURE_DIR/'"$suite"'.started"' \
        'while true; do sleep 1; done' \
        >"$cancel_commands/$suite"
    chmod +x "$cancel_commands/$suite"
done
MV_CAPTURE_DIR="$cancel_capture" \
    MANAGED_VALKEY_ROOT_RUN_ID="$cancel_run_id" \
    MANAGED_VALKEY_TEST_COMMAND_DIR="$cancel_commands" \
    "$repo_root/scripts/run_all_tests.sh" test >"$temporary/cancel.out" &
runner_pid=$!
for _ in $(seq 1 100); do
    [[ $(find "$cancel_capture" -name '*.started' | wc -l) == 3 ]] && break
    sleep 0.05
done
[[ $(find "$cancel_capture" -name '*.started' | wc -l) == 3 ]]
kill -TERM "$runner_pid"
set +e
wait "$runner_pid"
cancel_status=$?
set -e
[[ "$cancel_status" == 130 ]]
while IFS=$'\t' read -r group _; do
    if ps -eo pgid=,stat= | awk -v group="$group" '$1 == group && $2 !~ /^Z/ { found = 1 } END { exit !found }'; then
        echo "после отмены осталась дочерняя группа $group" >&2
        exit 1
    fi
done <"$cancel_run_dir/processes.tsv"
rm -rf -- "$cancel_run_dir"
