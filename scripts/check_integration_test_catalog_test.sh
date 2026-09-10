#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT

write_mock() {
    local path=$1
    shift
    printf '%s\n' '#!/usr/bin/env bash' 'set -euo pipefail' "$@" >"$path"
    chmod +x "$path"
}

complete="$temporary/complete-go"
write_mock "$complete" \
    'printf '\''%s\n'\'' \' \
    'Test_CreateSingleValkey_WithRealApiAndOperator_BecomesReachableAndRunning \' \
    'Test_ResizeSingleValkey_WithRealApiAndOperator_AppliesRequestedResources \' \
    'Test_RotateSingleValkeyPassword_WithRealApiAndOperator_ReplacesApplicationCredential \' \
    'Test_DeleteSingleValkey_WithRealApiAndOperator_RemovesKubernetesResourcesAndApiState'
MV_INTEGRATION_GO_COMMAND="$complete" "$repo_root/scripts/check_integration_test_catalog.sh"

incomplete="$temporary/incomplete-go"
write_mock "$incomplete" \
    'printf '\''%s\n'\'' \' \
    'Test_CreateSingleValkey_WithRealApiAndOperator_BecomesReachableAndRunning \' \
    'Test_ResizeSingleValkey_WithRealApiAndOperator_AppliesRequestedResources \' \
    'Test_DeleteSingleValkey_WithRealApiAndOperator_RemovesKubernetesResourcesAndApiState'
if MV_INTEGRATION_GO_COMMAND="$incomplete" "$repo_root/scripts/check_integration_test_catalog.sh" \
    >"$temporary/stdout" 2>"$temporary/stderr"; then
    echo "каталог принял запуск без обязательного теста" >&2
    exit 1
fi
rg -q 'Test_RotateSingleValkeyPassword_WithRealApiAndOperator_ReplacesApplicationCredential' \
    "$temporary/stderr"
