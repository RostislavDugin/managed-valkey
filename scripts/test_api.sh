#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)

cd "$repo_root"
docker_gateway=$(docker network inspect \
    "${MANAGED_VALKEY_DOCKER_NETWORK:?MANAGED_VALKEY_DOCKER_NETWORK не задан}" \
    --format '{{(index .IPAM.Config 0).Gateway}}')
if [[ "$docker_gateway" != "${K3S_DOCKER_GATEWAY:?K3S_DOCKER_GATEWAY не задан}" ]]; then
    echo "шлюз окружения $K3S_DOCKER_GATEWAY не совпадает со шлюзом сети $docker_gateway" >&2
    exit 1
fi
"$repo_root/scripts/check_operator_generated.sh"
go tool goose -allow-missing -dir migrations postgres \
    "${TEST_DATABASE_URL:?TEST_DATABASE_URL не задан}" up
TEST_DATABASE_URL=$TEST_DATABASE_URL \
    KUBECONFIG="${API_KUBECONFIG:?API_KUBECONFIG не задан}" \
    go test -count=1 ./api/... ./internal/...
