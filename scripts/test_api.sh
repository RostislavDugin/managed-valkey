#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)

cd "$repo_root"
"$repo_root/scripts/check_operator_generated.sh"
go tool goose -allow-missing -dir migrations postgres \
    "${TEST_DATABASE_URL:?TEST_DATABASE_URL не задан}" up
TEST_DATABASE_URL=$TEST_DATABASE_URL \
    KUBECONFIG="${API_KUBECONFIG:?API_KUBECONFIG не задан}" \
    go test -count=1 ./api/... ./internal/...
