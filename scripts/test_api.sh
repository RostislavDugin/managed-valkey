#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
mode=${1:-fast}

case "$mode" in
fast | full) ;;
*)
    echo "usage: $0 [fast|full]" >&2
    exit 2
    ;;
esac

cd "$repo_root"
if [[ "$mode" == full ]]; then
    docker_gateway=$(docker network inspect \
        "${MANAGED_VALKEY_DOCKER_NETWORK:?MANAGED_VALKEY_DOCKER_NETWORK не задан}" \
        --format '{{(index .IPAM.Config 0).Gateway}}')
    if [[ "$docker_gateway" != "${K3S_DOCKER_GATEWAY:?K3S_DOCKER_GATEWAY не задан}" ]]; then
        echo "шлюз окружения $K3S_DOCKER_GATEWAY не совпадает со шлюзом сети $docker_gateway" >&2
        exit 1
    fi
fi
"$repo_root/scripts/check_operator_generated.sh"
go tool goose -allow-missing -dir migrations postgres \
    "${TEST_DATABASE_URL:?TEST_DATABASE_URL не задан}" up
if [[ "$mode" == full ]]; then
    TEST_DATABASE_URL=$TEST_DATABASE_URL \
        KUBECONFIG="${API_KUBECONFIG:?API_KUBECONFIG не задан}" \
        ADMIN_KUBECONFIG="${ADMIN_KUBECONFIG:?ADMIN_KUBECONFIG не задан}" \
        go test -tags=k3s -count=1 ./api/... ./internal/...
else
    unset KUBECONFIG ADMIN_KUBECONFIG
    TEST_DATABASE_URL=$TEST_DATABASE_URL go test -count=1 ./api/... ./internal/...
fi
