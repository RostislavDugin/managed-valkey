#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
mode=${1:-test}
group=${2:-${MANAGED_VALKEY_E2E_GROUP:-all}}
run_id=${MV_RUN_ID:-manual-$(date -u +%Y%m%d%H%M%S)-$$-$RANDOM}
temporary_secret_dir=""

case "$mode" in
test)
    command=test
    action_delay_ms=0
    scroll_pause_ms=0
    ;;
headed)
    command=test:headed
    action_delay_ms=1000
    scroll_pause_ms=500
    ;;
prod)
    command=test:prod
    action_delay_ms=0
    scroll_pause_ms=0
    ;;
*)
    echo "usage: $0 <test|headed|prod>" >&2
    exit 2
    ;;
esac

case "$group" in
all) ;;
lifecycle | quotas | failures)
    if [[ "$mode" != test ]]; then
        echo "группа $group доступна только в headless-режиме test" >&2
        exit 2
    fi
    command="test:$group"
    ;;
*)
    echo "usage: $0 <test|headed|prod> [all|lifecycle|quotas|failures]" >&2
    exit 2
    ;;
esac

if [[ -n "${MANAGED_VALKEY_E2E_SECRET_VALUES_FILE:-}" ]]; then
    secret_values_file=$MANAGED_VALKEY_E2E_SECRET_VALUES_FILE
    mkdir -p "$(dirname "$secret_values_file")"
else
    temporary_secret_dir=$(mktemp -d)
    secret_values_file="$temporary_secret_dir/e2e-secret-values"
fi
cleanup_temporary_secret_dir() {
    [[ -z "$temporary_secret_dir" ]] || rm -rf -- "$temporary_secret_dir"
}
trap cleanup_temporary_secret_dir EXIT

account_password=${MANAGED_VALKEY_E2E_ACCOUNT_PASSWORD:-testpassword}
umask 077
touch "$secret_values_file"
chmod 0600 "$secret_values_file"
if ! grep --fixed-strings --line-regexp --quiet -- "$account_password" "$secret_values_file"; then
    printf '%s\n' "$account_password" >>"$secret_values_file"
fi

set +e
MANAGED_VALKEY_E2E_ACCOUNT_PASSWORD="$account_password" \
    MANAGED_VALKEY_E2E_ACTION_DELAY_MS="$action_delay_ms" \
    MANAGED_VALKEY_E2E_SCROLL_PAUSE_MS="$scroll_pause_ms" \
    MANAGED_VALKEY_E2E_SECRET_VALUES_FILE="$secret_values_file" \
    MV_RUN_ID="$run_id" \
    pnpm --dir "$repo_root/tests/e2e" "$command"
test_status=$?
set -e

artifact_dir="$repo_root/tmp/playwright/e2e/$run_id"
"$repo_root/scripts/sanitize_e2e_artifacts.sh" \
    --secret-values-file "$secret_values_file" "$artifact_dir"

check_options=(--secret-values-file "$secret_values_file")
check_paths=("$artifact_dir")
if [[ -n "${DIAGNOSTICS_DIR:-}" ]]; then
    check_paths+=("$DIAGNOSTICS_DIR")
fi
"$repo_root/scripts/check_e2e_artifacts.sh" "${check_options[@]}" "${check_paths[@]}"

exit "$test_status"
