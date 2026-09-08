#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
output="${1:-$repo_root/tmp/k3s/admin.kubeconfig}"

mkdir -p "$(dirname "$output")"

umask 077
docker compose -f "$repo_root/docker-compose.dev.yml" exec -T k3s-server \
    cat /etc/rancher/k3s/k3s.yaml > "$output"

echo "$output"
