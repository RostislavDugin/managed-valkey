#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
catalog=$repo_root/scripts/operator_test_scenarios.tsv
checker=$repo_root/scripts/validate_operator_test_scenarios.sh
reporter=$repo_root/scripts/report_operator_test_matrix.sh
work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT

discovered=$work_dir/discovered.txt
sed -n -E 's/^[^\t]+\t\^([^$]+)\$\t.*$/\1/p' "$catalog" >"$discovered"
"$checker" "$catalog" "$discovered" >/dev/null

expect_catalog_failure() {
    local name=$1 expected=$2
    if "$checker" "$work_dir/$name.tsv" "$discovered" >"$work_dir/$name.out" 2>&1; then
        echo "проверка каталога $name завершилась успешно" >&2
        exit 1
    fi
    grep -Fq "$expected" "$work_dir/$name.out"
}

cp "$catalog" "$work_dir/unknown.tsv"
printf '%s\n' 'TestUnknown	^TestUnknown$	1	1	2048	60	2m' >>"$work_dir/unknown.tsv"
expect_catalog_failure unknown 'неизвестный тест в каталоге: TestUnknown'

sed '2d' "$catalog" >"$work_dir/missing.tsv"
expect_catalog_failure missing 'тест не назначен:'

cp "$catalog" "$work_dir/duplicate.tsv"
sed -n '2p' "$catalog" >>"$work_dir/duplicate.tsv"
expect_catalog_failure duplicate 'назначен повторно'

sed -n '1p' "$catalog" >"$work_dir/empty.tsv"
expect_catalog_failure empty 'нет назначенных сценариев'

cp "$catalog" "$work_dir/pattern.tsv"
sed -i '2s/\^Test/^(Test/' "$work_dir/pattern.tsv"
expect_catalog_failure pattern 'выражение должно точно выбирать один Test'

results=$work_dir/results
scenario=matrix
attempt=$results/scenarios/$scenario/attempts/1
mkdir -p "$attempt"
printf '%s\n' '--- PASS: TestCT01CT02CT06CT07' >"$results/unit.log"
printf '%s\n' '--- PASS: TestCT01CT02CT03CT04CT05CT06CT07CT08' >"$results/envtest.log"
printf '%s\n' pass >"$results/unit.status"
printf '%s\n' pass >"$results/envtest.status"
printf '%s\n' 'CT-12: pass' >"$results/network-faults.log"
printf '%s\n' pass >"$results/network-faults.status"
printf '%s\n' $'unit\t100\t3' $'envtest\t103\t5' >"$results/durations.tsv"
{
    printf '%s' '--- PASS: Test'
    for prefix_and_last in HA:8 FP:12 ND:9 NT:6 OP:8 RZ:11 PW:10 DL:4; do
        prefix=${prefix_and_last%:*}
        last=${prefix_and_last#*:}
        for ((number = 1; number <= last; number++)); do
            printf '%s%02d' "$prefix" "$number"
        done
    done
    printf '%s\n' 'CT09CT10CT11CT14'
    printf '%s\n' 'CT-13 CT-14: pass'
    printf '%s\n' '--- PASS: TestMatrix'
    printf '%s\n' 'journal-sentinel-must-stay-separate'
} >"$attempt/log"
printf '%s\n' pass >"$attempt/status"
printf '%s\n' 12.5 >"$attempt/duration"
printf '%s\n' \
    $'scenario\ttest_pattern\tnodes\tenvoy_replicas\tmemory_mib\testimated_seconds\ttimeout' \
    $'matrix\t^TestMatrix$\t1\t1\t2048\t60\t2m' >"$work_dir/report-catalog.tsv"

"$reporter" "$results" full "$work_dir/report-catalog.tsv" >"$work_dir/report-pass.out" 2>&1
grep -Fq 'OPERATOR TEST MODE full' "$work_dir/report-pass.out"
grep -Fq 'RESULT network-faults PASS' "$work_dir/report-pass.out"
grep -Fq $'envtest\t5s' "$work_dir/report-pass.out"
grep -Fq 'MATRIX TOTAL 82/82 PASS' "$work_dir/report-pass.out"
if grep -Fq 'journal-sentinel-must-stay-separate' "$work_dir/report-pass.out"; then
    echo 'отчёт смешал журнал сценария со сводкой' >&2
    exit 1
fi

printf '%s\n' fail >"$results/unit.status"
if "$reporter" "$results" full "$work_dir/report-catalog.tsv" >"$work_dir/report-unit-status-fail.out" 2>&1; then
    echo 'отчёт принял неуспешный статус unit' >&2
    exit 1
fi
grep -Fq 'RESULT unit FAIL status=fail' "$work_dir/report-unit-status-fail.out"
printf '%s\n' pass >"$results/unit.status"

mv "$results/envtest.status" "$work_dir/envtest.status"
if "$reporter" "$results" full "$work_dir/report-catalog.tsv" >"$work_dir/report-envtest-status-missing.out" 2>&1; then
    echo 'отчёт принял envtest без статуса' >&2
    exit 1
fi
grep -Fq 'RESULT envtest MISSING' "$work_dir/report-envtest-status-missing.out"
mv "$work_dir/envtest.status" "$results/envtest.status"

printf '%s\n' fail >"$results/network-faults.status"
if "$reporter" "$results" full "$work_dir/report-catalog.tsv" >"$work_dir/report-network-fail.out" 2>&1; then
    echo 'отчёт принял неуспешную проверку CT-12' >&2
    exit 1
fi
grep -Fq 'RESULT network-faults FAIL status=fail' "$work_dir/report-network-fail.out"
grep -Fq 'MATRIX CT-12 FAIL' "$work_dir/report-network-fail.out"

printf '%s\n' pass >"$results/network-faults.status"
mv "$results/network-faults.status" "$work_dir/network-faults.status"
if "$reporter" "$results" full "$work_dir/report-catalog.tsv" >"$work_dir/report-network-status-missing.out" 2>&1; then
    echo 'отчёт принял CT-12 без статуса' >&2
    exit 1
fi
grep -Fq 'RESULT network-faults MISSING' "$work_dir/report-network-status-missing.out"
grep -Fq 'MATRIX CT-12 FAIL' "$work_dir/report-network-status-missing.out"
mv "$work_dir/network-faults.status" "$results/network-faults.status"

mv "$results/network-faults.log" "$work_dir/network-faults.log"
if "$reporter" "$results" full "$work_dir/report-catalog.tsv" >"$work_dir/report-network-missing.out" 2>&1; then
    echo 'отчёт принял CT-12 без журнала' >&2
    exit 1
fi
grep -Fq 'RESULT network-faults MISSING' "$work_dir/report-network-missing.out"
grep -Fq 'MATRIX CT-12 FAIL' "$work_dir/report-network-missing.out"
mv "$work_dir/network-faults.log" "$results/network-faults.log"

printf '%s\n' 'CT-13: pass' >"$results/network-faults.log"
if "$reporter" "$results" full "$work_dir/report-catalog.tsv" >"$work_dir/report-network-marker.out" 2>&1; then
    echo 'отчёт принял CT-12 без маркера' >&2
    exit 1
fi
grep -Fq 'RESULT network-faults INVALID: нет маркера CT-12' "$work_dir/report-network-marker.out"
grep -Fq 'MATRIX CT-12 FAIL' "$work_dir/report-network-marker.out"
printf '%s\n' 'CT-12: pass' >"$results/network-faults.log"

cp "$attempt/log" "$work_dir/log"
sed -i '/TestMatrix/d' "$attempt/log"
if "$reporter" "$results" full "$work_dir/report-catalog.tsv" >"$work_dir/report-no-pass.out" 2>&1; then
    echo 'отчёт принял результат без PASS назначенного теста' >&2
    exit 1
fi
grep -Fq 'нет PASS для TestMatrix' "$work_dir/report-no-pass.out"
mv "$work_dir/log" "$attempt/log"

printf '%s\n' fail >"$attempt/status"
if "$reporter" "$results" full "$work_dir/report-catalog.tsv" >"$work_dir/report-fail.out" 2>&1; then
    echo 'отчёт принял упавший сценарий' >&2
    exit 1
fi
grep -Fq 'RESULT matrix FAIL' "$work_dir/report-fail.out"

printf '%s\n' skip >"$attempt/status"
if "$reporter" "$results" full "$work_dir/report-catalog.tsv" >"$work_dir/report-skip.out" 2>&1; then
    echo 'отчёт принял пропущенный сценарий' >&2
    exit 1
fi
grep -Fq 'RESULT matrix SKIP' "$work_dir/report-skip.out"

printf '%s\n' pass >"$attempt/status"
mv "$attempt/duration" "$work_dir/duration"
if "$reporter" "$results" full "$work_dir/report-catalog.tsv" >"$work_dir/report-incomplete.out" 2>&1; then
    echo 'отчёт принял неполный результат' >&2
    exit 1
fi
grep -Fq 'RESULT matrix INVALID' "$work_dir/report-incomplete.out"
mv "$work_dir/duration" "$attempt/duration"

printf '%s\n' fail >"$attempt/status"
retry=$results/scenarios/$scenario/attempts/2
mkdir -p "$retry"
cp "$attempt/log" "$retry/log"
printf '%s\n' pass >"$retry/status"
printf '%s\n' 10 >"$retry/duration"
if "$reporter" "$results" full "$work_dir/report-catalog.tsv" >"$work_dir/report-retry.out" 2>&1; then
    echo 'отчёт скрыл исходный сбой после повтора' >&2
    exit 1
fi
grep -Fq 'original-failure-preserved' "$work_dir/report-retry.out"

rm -rf "$results/scenarios/$scenario"
if "$reporter" "$results" full "$work_dir/report-catalog.tsv" >"$work_dir/report-missing.out" 2>&1; then
    echo 'отчёт принял отсутствующий результат' >&2
    exit 1
fi
grep -Fq 'RESULT matrix MISSING' "$work_dir/report-missing.out"

"$reporter" "$results" ci "$work_dir/report-catalog.tsv" >"$work_dir/report-ci.out" 2>&1
grep -Fq 'OPERATOR TEST MODE ci' "$work_dir/report-ci.out"
grep -Fq 'MATRIX SCOPE unit/envtest only' "$work_dir/report-ci.out"
if grep -Fq 'MATRIX TOTAL' "$work_dir/report-ci.out"; then
    echo 'ограниченный отчёт заявил полную матрицу' >&2
    exit 1
fi

echo 'проверки каталога и отчёта оператора прошли'
