#!/usr/bin/env bash
set -uo pipefail

repo_root=$(git rev-parse --show-toplevel)
recipe=${1:-test}
case "$recipe" in
lint | test | test-full) ;;
*)
    echo "usage: $0 [lint|test|test-full]" >&2
    exit 2
    ;;
esac
run_id=${MANAGED_VALKEY_ROOT_RUN_ID:-root-${recipe//-/_}-$(date -u +%Y%m%d%H%M%S)-$$-$RANDOM}
if [[ "$recipe" == lint ]]; then
    run_dir="$repo_root/tmp/lint-runs/$run_id"
else
    run_dir="$repo_root/tmp/test-runs/$run_id"
fi
suites=(api operator web)
future_suites=()
[[ "$recipe" != test-full ]] || future_suites=(integration)
declare -A pids statuses
interrupted=0
command_dir=${MANAGED_VALKEY_TEST_COMMAND_DIR:-}

mkdir -p "$run_dir"
chmod 0700 "$run_dir"

suite_justfile() {
    printf '%s/%s/Justfile\n' "$repo_root" "$1"
}

stop_suites() {
    local suite

    interrupted=130
    for suite in "${suites[@]}"; do
        if [[ -n "${pids[$suite]:-}" ]]; then
            kill -TERM -- "-${pids[$suite]}" 2>/dev/null || true
        fi
    done
}

trap stop_suites INT TERM

for suite in "${suites[@]}"; do
    echo "$suite: запущен, журнал $run_dir/$suite.log"
    if [[ -n "$command_dir" ]]; then
        command="$command_dir/$suite"
        if [[ ! -s "$command" || ! -x "$command" ]]; then
            statuses[$suite]=2
            echo "$suite: команда $recipe отсутствует или пуста" >"$run_dir/$suite.log"
            continue
        fi
        setsid bash -c 'exec "$1" "$2" >"$3" 2>&1' _ \
            "$command" "$recipe" "$run_dir/$suite.log" &
    else
        justfile=$(suite_justfile "$suite")
        if [[ ! -s "$justfile" ]] ||
            ! just --justfile "$justfile" --summary | tr ' ' '\n' | grep -qx "$recipe"; then
            statuses[$suite]=2
            echo "$suite: команда $recipe не подключена" >"$run_dir/$suite.log"
            continue
        fi
        setsid bash -c 'exec just --justfile "$1" "$2" >"$3" 2>&1' _ \
            "$justfile" "$recipe" "$run_dir/$suite.log" &
    fi
    pids[$suite]=$!
    printf '%s\t%s\n' "${pids[$suite]}" "$suite" >>"$run_dir/processes.tsv"
done

for suite in "${suites[@]}"; do
    if [[ -n "${pids[$suite]:-}" ]]; then
        wait "${pids[$suite]}"
        statuses[$suite]=$?
    fi
done
trap - INT TERM

result=$interrupted
echo
echo "Результаты $recipe:"
for suite in "${suites[@]}"; do
    status=${statuses[$suite]:-2}
    if ((status == 0)); then
        echo "  $suite: пройден, $run_dir/$suite.log"
    else
        echo "  $suite: ошибка $status, $run_dir/$suite.log"
        ((result != 0)) || result=$status
    fi
done
for suite in "${future_suites[@]}"; do
    echo "  $suite: ещё не реализован"
done
echo "  журналы: $run_dir"

exit "$result"
