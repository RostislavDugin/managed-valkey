#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
group=${1:-all}
exec pnpm --dir "$repo_root/tests/e2e" check:catalog "$group"
