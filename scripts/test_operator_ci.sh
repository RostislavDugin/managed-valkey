#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
envtest_version=${MANAGED_VALKEY_ENVTEST_VERSION:-1.35.0}
if [[ -n ${GITHUB_RUN_ID:-} ]]; then
    run_id="github-${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT:-1}"
else
    run_id="local-$(date -u +%Y%m%d%H%M%S)-$$-$RANDOM"
fi
results_dir=${MANAGED_VALKEY_OPERATOR_CI_RESULTS_DIR:-"$repo_root/tmp/operator/ci/$run_id"}
assets_file="$results_dir/envtest-assets"

run_logged() {
    local name=$1 required_pass=$2 started_at status
    shift 2

    started_at=$(date +%s)
    set +e
    "$@" 2>&1 | tee "$results_dir/$name.log"
    status=${PIPESTATUS[0]}
    set -e
    if ((status == 0)) && [[ -n "$required_pass" ]] &&
        ! grep -Eq -- "$required_pass" "$results_dir/$name.log"; then
        status=1
    fi
    if ((status != 0)); then
        printf '%s\n' "--- FAIL: $name (exit $status)" |
            tee -a "$results_dir/$name.log"
        printf 'fail\n' >"$results_dir/$name.status"
    else
        printf 'pass\n' >"$results_dir/$name.status"
    fi
    printf '%s\t%s\t%s\n' "$name" "$started_at" "$(($(date +%s) - started_at))" \
        >>"$results_dir/durations.tsv"
    return "$status"
}

prepare_envtest() {
    local assets

    if ! assets=$(go tool setup-envtest use "$envtest_version" \
        --bin-dir "$repo_root/bin/envtest" -p path); then
        return 1
    fi
    if [[ -z "$assets" ]]; then
        printf 'setup-envtest не вернул путь к файлам\n' >&2
        return 1
    fi
    printf '%s\n' "$assets" >"$assets_file"
}

finish() {
    local command_status=$? report_status

    trap - EXIT
    set +e
    "$repo_root/scripts/report_operator_test_matrix.sh" "$results_dir" ci 2>&1 |
        tee "$results_dir/matrix-report.log"
    report_status=${PIPESTATUS[0]}
    set -e
    if ((command_status == 0 && report_status == 0)); then
        printf 'status=passed\n' >>"$results_dir/summary.txt"
    else
        printf 'status=failed\n' >>"$results_dir/summary.txt"
    fi
    printf 'Результаты ограниченного режима: %s\n' "$results_dir"
    if ((command_status != 0)); then
        exit "$command_status"
    fi
    exit "$report_status"
}

cd "$repo_root"
mkdir -p "$results_dir"
chmod 0700 "$results_dir"
: >"$results_dir/durations.tsv"
printf '%s\n' \
    'mode=ci-unit-envtest' \
    'scope=unit,envtest' \
    'k3s_matrix=not-run' \
    >"$results_dir/summary.txt"
printf '%s\n' \
    'Режим тестов оператора: ci-unit-envtest' \
    'Выполняются unit/envtest; полная k3s-матрица не запускается'
trap finish EXIT

run_logged prepare-envtest '' prepare_envtest
run_logged unit '^--- PASS: Test' go test -v -count=1 ./operator/... ./internal/...
run_logged envtest '^--- PASS: TestEnvtest' env \
    KUBEBUILDER_ASSETS="$(<"$assets_file")" \
    go test -v -tags=envtest -count=1 -run '^TestEnvtest' ./operator/... ./internal/...
