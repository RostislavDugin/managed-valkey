#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
go_command=${MV_INTEGRATION_GO_COMMAND:-go}
required_tests=(
    Test_CreateSingleValkey_WithRealApiAndOperator_BecomesReachableAndRunning
    Test_ResizeSingleValkey_WithRealApiAndOperator_AppliesRequestedResources
    Test_RotateSingleValkeyPassword_WithRealApiAndOperator_ReplacesApplicationCredential
    Test_DeleteSingleValkey_WithRealApiAndOperator_RemovesKubernetesResourcesAndApiState
)

catalog=$(cd "$repo_root" && "$go_command" test -tags integration -run '^$' -list '^Test_' ./tests/integrations)
status=0
for test_name in "${required_tests[@]}"; do
    if ! rg --fixed-strings --line-regexp --quiet "$test_name" <<<"$catalog"; then
        echo "integration: обязательный тест не найден: $test_name" >&2
        status=1
    fi
done
exit "$status"
