#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
temporary=$(mktemp -d)
trap 'rm -rf -- "$temporary"' EXIT

mock_bin="$temporary/bin"
mkdir -p "$mock_bin"
printf '%s\n' \
    '#!/usr/bin/env bash' \
    'set -euo pipefail' \
    'printf '\''%s\n'\'' "$*" >"$MV_E2E_CATALOG_ARGS"' \
    'exit "${MV_E2E_CATALOG_STATUS:-0}"' \
    >"$mock_bin/pnpm"
chmod +x "$mock_bin/pnpm"

arguments="$temporary/arguments"
MV_E2E_CATALOG_ARGS="$arguments" PATH="$mock_bin:$PATH" \
    "$repo_root/scripts/check_e2e_catalog.sh"
rg -q '/tests/e2e check:catalog all$' "$arguments"

MV_E2E_CATALOG_ARGS="$arguments" PATH="$mock_bin:$PATH" \
    "$repo_root/scripts/check_e2e_catalog.sh" failures
rg -q '/tests/e2e check:catalog failures$' "$arguments"

set +e
MV_E2E_CATALOG_ARGS="$arguments" MV_E2E_CATALOG_STATUS=23 PATH="$mock_bin:$PATH" \
    "$repo_root/scripts/check_e2e_catalog.sh"
status=$?
set -e
[[ "$status" == 23 ]]
