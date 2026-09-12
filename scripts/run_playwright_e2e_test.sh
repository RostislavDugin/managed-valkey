#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
temporary=$(mktemp -d)
artifact_dirs=()
cleanup() {
    local path

    rm -rf -- "$temporary"
    for path in "${artifact_dirs[@]}"; do
        rm -rf -- "$path"
    done
}
trap cleanup EXIT

mock_bin="$temporary/bin"
mkdir -p "$mock_bin"
printf '%s\n' \
    '#!/usr/bin/env bash' \
    'set -euo pipefail' \
    'printf '\''%s\n'\'' "$*" >"$MV_E2E_CAPTURE.args"' \
    'env >"$MV_E2E_CAPTURE.env"' \
    'mkdir -p "$MV_E2E_ARTIFACT_DIR/report"' \
    'printf '\''account=%s dynamic=%s\n'\'' "$MANAGED_VALKEY_E2E_ACCOUNT_PASSWORD" "$MV_E2E_DYNAMIC_SECRET" >"$MV_E2E_ARTIFACT_DIR/report/index.html"' \
    'exit "${MV_E2E_PNPM_STATUS:-0}"' \
    >"$mock_bin/pnpm"
chmod +x "$mock_bin/pnpm"

run_mode() {
    local mode=$1 group=$2 command=$3 delay=$4 scroll_pause=$5
    local run_id="runner-${mode}-${group}-${RANDOM}" capture="$temporary/$mode-$group"
    local secret_values_file="$temporary/$mode-$group-secret-values"
    local dynamic_secret="dynamic-${mode}-${group}-${RANDOM}"
    local artifact_dir="$repo_root/tmp/playwright/e2e/$run_id"

    artifact_dirs+=("$artifact_dir")
    printf '%s\n' "$dynamic_secret" >"$secret_values_file"
    chmod 0600 "$secret_values_file"
    MANAGED_VALKEY_E2E_SECRET_VALUES_FILE="$secret_values_file" \
        MV_E2E_ARTIFACT_DIR="$artifact_dir" \
        MV_E2E_CAPTURE="$capture" \
        MV_E2E_DYNAMIC_SECRET="$dynamic_secret" \
        MV_RUN_ID="$run_id" \
        PATH="$mock_bin:$PATH" \
        "$repo_root/scripts/run_playwright_e2e.sh" "$mode" "$group"

    rg -q " $command$" "$capture.args"
    rg -q "^MANAGED_VALKEY_E2E_ACTION_DELAY_MS=$delay$" "$capture.env"
    rg -q "^MANAGED_VALKEY_E2E_SCROLL_PAUSE_MS=$scroll_pause$" "$capture.env"
    rg -q "^MANAGED_VALKEY_E2E_SECRET_VALUES_FILE=$secret_values_file$" "$capture.env"
    rg --fixed-strings --line-regexp --quiet testpassword "$secret_values_file"
    [[ "$(stat -c '%a' "$secret_values_file")" == 600 ]]
    ! rg --fixed-strings --quiet "$dynamic_secret" "$artifact_dir"
    ! rg --fixed-strings --quiet testpassword "$artifact_dir"
    rg --fixed-strings --quiet '<redacted>' "$artifact_dir/report/index.html"
}

run_mode test all test 0 0
run_mode test lifecycle test:lifecycle 0 0
run_mode test quotas test:quotas 0 0
run_mode test failures test:failures 0 0
run_mode headed all test:headed 1000 500
run_mode prod all test:prod 0 0

selected_run_id="runner-selected-${RANDOM}"
selected_artifact_dir="$repo_root/tmp/playwright/e2e/$selected_run_id"
artifact_dirs+=("$selected_artifact_dir")
MANAGED_VALKEY_E2E_FILE=00-single-lifecycle.spec.ts \
    MANAGED_VALKEY_E2E_SECRET_VALUES_FILE="$temporary/selected-secret-values" \
    MV_E2E_ARTIFACT_DIR="$selected_artifact_dir" \
    MV_E2E_CAPTURE="$temporary/selected" \
    MV_E2E_DYNAMIC_SECRET=dynamic-selected \
    MV_RUN_ID="$selected_run_id" \
    PATH="$mock_bin:$PATH" \
    "$repo_root/scripts/run_playwright_e2e.sh" test all
rg -q ' exec playwright test specs/00-single-lifecycle\.spec\.ts$' "$temporary/selected.args"

if MANAGED_VALKEY_E2E_FILE= PATH="$mock_bin:$PATH" \
    "$repo_root/scripts/run_playwright_e2e.sh" test all \
    >"$temporary/empty-file.out" 2>"$temporary/empty-file.err"; then
    echo "пустое имя файла Playwright было принято запускателем" >&2
    exit 1
fi
rg -q 'MANAGED_VALKEY_E2E_FILE не должен быть пустым' "$temporary/empty-file.err"

if MANAGED_VALKEY_E2E_FILE=unknown.spec.ts PATH="$mock_bin:$PATH" \
    "$repo_root/scripts/run_playwright_e2e.sh" test all \
    >"$temporary/unknown-file.out" 2>"$temporary/unknown-file.err"; then
    echo "неизвестный файл Playwright был принят запускателем" >&2
    exit 1
fi
rg -q 'файл теста не найден: unknown.spec.ts' "$temporary/unknown-file.err"

if PATH="$mock_bin:$PATH" "$repo_root/scripts/run_playwright_e2e.sh" unknown \
    >"$temporary/invalid.out" 2>"$temporary/invalid.err"; then
    echo "неизвестный режим Playwright был принят" >&2
    exit 1
fi
rg -q 'usage: .* <test|headed|prod>' "$temporary/invalid.err"

if PATH="$mock_bin:$PATH" "$repo_root/scripts/run_playwright_e2e.sh" test unknown \
    >"$temporary/invalid-group.out" 2>"$temporary/invalid-group.err"; then
    echo "неизвестная группа Playwright была принята" >&2
    exit 1
fi
rg -q 'usage: .* \[all|lifecycle|quotas|failures\]' "$temporary/invalid-group.err"

if PATH="$mock_bin:$PATH" "$repo_root/scripts/run_playwright_e2e.sh" headed lifecycle \
    >"$temporary/headed-group.out" 2>"$temporary/headed-group.err"; then
    echo "группа Playwright была принята в headed-режиме" >&2
    exit 1
fi
rg -q 'доступна только в headless-режиме test' "$temporary/headed-group.err"

empty_run_id="runner-empty-group-${RANDOM}"
empty_artifact_dir="$repo_root/tmp/playwright/e2e/$empty_run_id"
artifact_dirs+=("$empty_artifact_dir")
set +e
MANAGED_VALKEY_E2E_SECRET_VALUES_FILE="$temporary/empty-group-secret-values" \
    MV_E2E_ARTIFACT_DIR="$empty_artifact_dir" \
    MV_E2E_CAPTURE="$temporary/empty-group" \
    MV_E2E_DYNAMIC_SECRET=dynamic-empty-group \
    MV_E2E_PNPM_STATUS=1 \
    MV_RUN_ID="$empty_run_id" \
    PATH="$mock_bin:$PATH" \
    "$repo_root/scripts/run_playwright_e2e.sh" test lifecycle
empty_status=$?
set -e
[[ "$empty_status" == 1 ]]
rg -q ' test:lifecycle$' "$temporary/empty-group.args"
