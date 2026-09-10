#!/usr/bin/env bash
set -euo pipefail

results_dir=${1:?задайте каталог результатов тестов}

if [[ ! -d "$results_dir" ]]; then
    echo "каталог результатов тестов не найден: $results_dir" >&2
    exit 1
fi

shopt -s nullglob
unit_logs=("$results_dir"/unit.log)
envtest_logs=("$results_dir"/envtest.log)
k3s_logs=("$results_dir"/k3s-*.log)
network_logs=("$results_dir"/network-faults.log)
environment_logs=("$results_dir"/environment-*.log)
all_logs=("$results_dir"/*.log)

if ((${#all_logs[@]} == 0)); then
    echo "в $results_dir нет журналов тестов" >&2
    exit 1
fi

if rg --no-filename '^[[:space:]]*--- SKIP:' "${all_logs[@]}"; then
    echo "обязательный набор содержит пропущенные тесты" >&2
    exit 1
fi

collect_go_ids() {
    if (($# == 0)); then
        return
    fi
    {
        rg --no-filename '^--- PASS: Test' "$@" 2>/dev/null || true
    } | rg -o '(HA|FP|ND|NT|OP|RZ|PW|DL|CT)-?[0-9]{2}' | \
        sed -E 's/^([A-Z]+)([0-9]{2})$/\1-\2/' | sort -u
}

collect_marker_ids() {
    if (($# == 0)); then
        return
    fi
    {
        rg --no-filename '^CT-[0-9]{2}([[:space:]]+CT-[0-9]{2})*:' "$@" 2>/dev/null || true
    } | rg -o 'CT-[0-9]{2}' | sort -u
}

mapfile -t unit_ids < <(collect_go_ids "${unit_logs[@]}")
mapfile -t envtest_ids < <(collect_go_ids "${envtest_logs[@]}")
mapfile -t k3s_ids < <(collect_go_ids "${k3s_logs[@]}")
mapfile -t network_ids < <(collect_marker_ids "${network_logs[@]}")
mapfile -t environment_ids < <(collect_marker_ids "${environment_logs[@]}")

declare -A unit_seen=()
declare -A envtest_seen=()
declare -A k3s_seen=()
declare -A network_seen=()
declare -A environment_seen=()

for id in "${unit_ids[@]}"; do unit_seen["$id"]=1; done
for id in "${envtest_ids[@]}"; do envtest_seen["$id"]=1; done
for id in "${k3s_ids[@]}"; do k3s_seen["$id"]=1; done
for id in "${network_ids[@]}"; do network_seen["$id"]=1; done
for id in "${environment_ids[@]}"; do environment_seen["$id"]=1; done

passed=0
failed=0

report_id() {
    local id=$1
    local source=$2
    local found=0

    case "$source" in
        unit) [[ -v unit_seen["$id"] ]] && found=1 ;;
        envtest) [[ -v envtest_seen["$id"] ]] && found=1 ;;
        unit+envtest)
            [[ -v unit_seen["$id"] && -v envtest_seen["$id"] ]] && found=1
            ;;
        environment+k3s)
            [[ -v environment_seen["$id"] && -v k3s_seen["$id"] ]] && found=1
            ;;
        k3s) [[ -v k3s_seen["$id"] ]] && found=1 ;;
        network) [[ -v network_seen["$id"] ]] && found=1 ;;
        environment) [[ -v environment_seen["$id"] ]] && found=1 ;;
        *) echo "неизвестный источник матрицы: $source" >&2; exit 1 ;;
    esac

    if ((found == 1)); then
        printf 'MATRIX %-5s PASS (%s)\n' "$id" "$source"
        ((passed += 1))
    else
        printf 'MATRIX %-5s FAIL: нет успешного результата в %s\n' "$id" "$source" >&2
        ((failed += 1))
    fi
}

report_range() {
    local prefix=$1
    local last=$2
    local source=$3
    local number id

    for ((number = 1; number <= last; number++)); do
        printf -v id '%s-%02d' "$prefix" "$number"
        report_id "$id" "$source"
    done
}

report_range HA 8 k3s
report_range FP 12 k3s
report_range ND 9 k3s
report_range NT 6 k3s
report_range OP 8 k3s
report_range RZ 11 k3s
report_range PW 10 k3s
report_range DL 4 k3s
for number in 1 2 6 7; do
    printf -v id 'CT-%02d' "$number"
    report_id "$id" unit+envtest
done
for number in 3 4 5 8; do
    printf -v id 'CT-%02d' "$number"
    report_id "$id" envtest
done
for number in 9 10 11; do
    printf -v id 'CT-%02d' "$number"
    report_id "$id" k3s
done
report_id CT-12 network
report_id CT-13 environment
report_id CT-14 environment+k3s

printf 'MATRIX TOTAL %d/82 PASS\n' "$passed"

printf '%s\n' 'Самые долгие Go-тесты:'
{
    sed -n -E 's/^[[:space:]]*--- PASS: (Test[^ ]+) \(([0-9.]+)s\)$/\2\t\1/p' \
        "${unit_logs[@]}" "${envtest_logs[@]}" "${k3s_logs[@]}" 2>/dev/null || true
} | sort -nr | sed -n '1,15p'

if [[ -s "$results_dir/durations.tsv" ]]; then
    printf '%s\n' 'Длительности этапов:'
    sort -t $'\t' -k3,3nr "$results_dir/durations.tsv" |
        awk -F $'\t' '{ printf "%s\t%ss\n", $1, $3 }'
fi

((failed == 0))
