#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
envtest_version=1.35.0

cd "$repo_root"
"$repo_root/scripts/check_operator_generated.sh"
KUBEBUILDER_ASSETS="$(go tool setup-envtest use "$envtest_version" \
    --bin-dir "$repo_root/bin/envtest" -p path)" \
    go test -tags=envtest -count=1 ./operator/... ./internal/...

KUBECONFIG="${ADMIN_KUBECONFIG:?ADMIN_KUBECONFIG не задан}" \
    kubectl get --raw=/readyz >/dev/null
KUBECONFIG="$ADMIN_KUBECONFIG" kubectl -n valkey-system get \
    gateway/valkey clienttrafficpolicy/valkey-connections secret/valkey-wildcard-tls >/dev/null

MANAGED_VALKEY_ADMIN_KUBECONFIG=$ADMIN_KUBECONFIG \
    MANAGED_VALKEY_OPERATOR_KUBECONFIG="${OPERATOR_KUBECONFIG:?OPERATOR_KUBECONFIG не задан}" \
    go test -tags=integration -count=1 -timeout=20m ./operator/integration
