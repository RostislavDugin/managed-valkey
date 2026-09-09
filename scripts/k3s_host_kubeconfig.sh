#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
output="${1:-${ADMIN_KUBECONFIG:-$repo_root/tmp/k3s/dev/admin.kubeconfig}}"
project="${MANAGED_VALKEY_COMPOSE_PROJECT:-managed-valkey-dev}"
server="${KUBERNETES_SERVER:-https://127.0.0.1:6443}"

mkdir -p "$(dirname "$output")"

umask 077
docker compose -f "$repo_root/docker-compose.dev.yml" -p "$project" exec -T k3s-server \
    cat /etc/rancher/k3s/k3s.yaml >"$output"
KUBECONFIG="$output" kubectl config set-cluster default --server="$server" >/dev/null
chmod 0600 "$output"

echo "$output"
