#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
mock_bin=$temporary/bin
mkdir -p "$mock_bin"

printf '%s\n' \
    '#!/usr/bin/env bash' \
    'set -euo pipefail' \
    'case "$1" in' \
    'ps) printf "container-id\n" ;;' \
    'inspect)' \
    '    case "$4" in' \
    '    "{{.State.Running}}") [[ -f "$MV_CONTAINER_RUNNING_FILE" ]] && printf "true\n" || printf "false\n" ;;' \
    '    *compose.project*) printf "%s\n" "$MV_EXPECTED_PROJECT" ;;' \
    '    *compose.service*) printf "%s\n" "$MV_EXPECTED_NODE" ;;' \
    '    esac' \
    '    ;;' \
    'start)' \
    '    starts=0' \
    '    [[ ! -f "$MV_CONTAINER_STARTS_FILE" ]] || read -r starts <"$MV_CONTAINER_STARTS_FILE"' \
    '    printf "%s\n" "$((starts + 1))" >"$MV_CONTAINER_STARTS_FILE"' \
    '    : >"$MV_CONTAINER_RUNNING_FILE"' \
    '    ;;' \
    'exec)' \
    '    [[ -f "$MV_CONTAINER_RUNNING_FILE" ]] || exit 1' \
    '    if [[ "$2" != -i ]]; then exit 0; fi' \
    '    attempt=0' \
    '    [[ ! -f "$MV_IMPORT_ATTEMPTS_FILE" ]] || read -r attempt <"$MV_IMPORT_ATTEMPTS_FILE"' \
    '    attempt=$((attempt + 1))' \
    '    printf "%s\n" "$attempt" >"$MV_IMPORT_ATTEMPTS_FILE"' \
    '    ((attempt > 1)) || rm -f "$MV_CONTAINER_RUNNING_FILE"' \
    '    ((attempt > 1))' \
    '    ;;' \
    '*) exit 2 ;;' \
    'esac' \
    >"$mock_bin/docker"
chmod +x "$mock_bin/docker"
printf '%s\n' '#!/usr/bin/env bash' 'exit 0' >"$mock_bin/sleep"
chmod +x "$mock_bin/sleep"

archive=$temporary/image.tar
: >"$archive"
attempts=$temporary/import-attempts
starts=$temporary/container-starts
running=$temporary/container-running
stderr=$temporary/stderr
MV_EXPECTED_PROJECT=managed-valkey-test \
    MV_EXPECTED_NODE=k3s-server \
    MV_IMPORT_ATTEMPTS_FILE=$attempts \
    MV_CONTAINER_STARTS_FILE=$starts \
    MV_CONTAINER_RUNNING_FILE=$running \
    PATH="$mock_bin:$PATH" \
    "$repo_root/scripts/load_k3s_image.sh" \
    managed-valkey-test --archive "$archive" k3s-server \
    2>"$stderr"

[[ "$(<"$attempts")" == 2 ]]
[[ "$(<"$starts")" == 2 ]]
rg -q 'перезапуск остановленного managed-valkey-test/k3s-server' "$stderr"
rg -q 'повтор импорта в managed-valkey-test/k3s-server после ошибки' "$stderr"
