#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
mode=${1:-test}
run_id=${MV_RUN_ID:-manual-$(date -u +%Y%m%d%H%M%S)-$$-$RANDOM}

case "$mode" in
test) command=test ;;
headed) command=test:headed ;;
prod) command=test:prod ;;
*)
    echo "usage: $0 <test|headed|prod>" >&2
    exit 2
    ;;
esac

set +e
MANAGED_VALKEY_E2E_ACCOUNT_PASSWORD=testpassword \
    MV_RUN_ID="$run_id" \
    pnpm --dir "$repo_root/tests/e2e" "$command"
test_status=$?
set -e

artifact_dir="$repo_root/tmp/playwright/e2e/$run_id"
MANAGED_VALKEY_E2E_ACCOUNT_PASSWORD=testpassword \
    "$repo_root/scripts/sanitize_e2e_artifacts.sh" "$artifact_dir"

check_paths=("$artifact_dir")
if [[ -n "${DIAGNOSTICS_DIR:-}" ]]; then
    check_paths+=("$DIAGNOSTICS_DIR")
fi
"$repo_root/scripts/check_e2e_artifacts.sh" "${check_paths[@]}"

exit "$test_status"
