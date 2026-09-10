#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
nodes=${MANAGED_VALKEY_K3S_NODES:?MANAGED_VALKEY_K3S_NODES не задан}
operator_image=${MV_OPERATOR_TEST_IMAGE:?MV_OPERATOR_TEST_IMAGE не задан}
valkey_image=${MV_OPERATOR_VALKEY_IMAGE:?MV_OPERATOR_VALKEY_IMAGE не задан}
binary=${MV_OPERATOR_TEST_BINARY:?MV_OPERATOR_TEST_BINARY не задан}
pattern=${MV_OPERATOR_TEST_PATTERN:?MV_OPERATOR_TEST_PATTERN не задан}
timeout=${MV_OPERATOR_TEST_TIMEOUT:?MV_OPERATOR_TEST_TIMEOUT не задан}
image_archive=${MANAGED_VALKEY_K3S_IMAGE_ARCHIVE:?MANAGED_VALKEY_K3S_IMAGE_ARCHIVE не задан}
services=(k3s-server)

if ((nodes >= 2)); then
    services+=(k3s-agent-1)
fi
if ((nodes >= 3)); then
    services+=(k3s-agent-2)
fi
if ((nodes == 4)); then
    services+=(k3s-agent-3)
fi

KUBECONFIG="${ADMIN_KUBECONFIG:?ADMIN_KUBECONFIG не задан}" kubectl get --raw=/readyz >/dev/null
KUBECONFIG="$ADMIN_KUBECONFIG" kubectl -n valkey-system get \
    gateway/valkey clienttrafficpolicy/valkey-connections secret/valkey-wildcard-tls >/dev/null
"$repo_root/scripts/check_operator_test_environment.sh" "$nodes" \
    "${MANAGED_VALKEY_ENVOY_REPLICAS:?MANAGED_VALKEY_ENVOY_REPLICAS не задан}"
"$repo_root/scripts/load_k3s_image.sh" "$MANAGED_VALKEY_COMPOSE_PROJECT" \
    --archive "$image_archive" "${services[@]}"

export MANAGED_VALKEY_OPERATOR_IMAGE=$operator_image
export MANAGED_VALKEY_VALKEY_IMAGE=$valkey_image
export MANAGED_VALKEY_REPO_ROOT=$repo_root
export MANAGED_VALKEY_ADMIN_KUBECONFIG=$ADMIN_KUBECONFIG
export MANAGED_VALKEY_OPERATOR_KUBECONFIG=${OPERATOR_KUBECONFIG:?OPERATOR_KUBECONFIG не задан}

"$binary" -test.v -test.count=1 "-test.timeout=$timeout" "-test.run=$pattern"
