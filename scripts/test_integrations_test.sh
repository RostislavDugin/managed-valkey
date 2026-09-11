#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT

write_script() {
    local path=$1
    shift
    printf '%s\n' '#!/usr/bin/env bash' 'set -euo pipefail' "$@" >"$path"
    chmod +x "$path"
}

mock_bin="$temporary/bin"
mkdir -p "$mock_bin"
write_script "$mock_bin/curl" 'exit 0'
write_script "$mock_bin/go" \
    'case "$*" in' \
    '*"tool goose"*) exit 0 ;;' \
    '*"run ./api/cmd/api"*) component=api ;;' \
    '*"run -tags integration ./operator/cmd/operator"*) component=operator ;;' \
    '*"test -tags integration"*)' \
    '    : >"$MV_FAKE_TEST_STARTED"' \
    '    env >"$MV_FAKE_TEST_ENV"' \
    '    if [[ "${MV_FAKE_TEST_FAIL:-0}" == 1 ]]; then' \
    '        printf '\''status=provisioning\n'\'' >"$MANAGED_VALKEY_INTEGRATION_DIAGNOSTICS_DIR/last-http-failed.txt"' \
    '        exit 7' \
    '    fi' \
    '    [[ "${MV_FAKE_TEST_BLOCK:-0}" == 1 ]] || exit 0' \
    '    trap '\''exit 143'\'' TERM' \
    '    while true; do sleep 0.1; done' \
    '    ;;' \
    '*) exit 2 ;;' \
    'esac' \
    ': >"$MV_FAKE_STARTED_DIR/$component"' \
    'env >"$MV_FAKE_STARTED_DIR/$component.env"' \
    'trap '\''exit 0'\'' TERM INT' \
    'while true; do sleep 0.1; done'

prepare_state() {
    local state=$1

    mkdir -p "$state/diagnostics"
    : >"$state/admin.kubeconfig"
    : >"$state/api.kubeconfig"
    : >"$state/operator.kubeconfig"
    : >"$state/ca.crt"
    printf 'VALKEY_OPERATOR_CIDRS=127.0.0.1/32\n' >"$state/operator.env"
}

process_is_live() {
    local pid=$1

    ps -o stat= -p "$pid" | awk '$1 !~ /^Z/ { live = 1 } END { exit !live }'
}

run_script() {
    local state=$1

    PATH="$mock_bin:$PATH" \
        ADMIN_KUBECONFIG="$state/admin.kubeconfig" \
        API_KUBECONFIG="$state/api.kubeconfig" \
        DIAGNOSTICS_DIR="$state/diagnostics" \
        MANAGED_VALKEY_CA_FILE="$state/ca.crt" \
        MANAGED_VALKEY_PUBLIC_ADDRESS=127.0.0.1:31379 \
        MV_FAKE_STARTED_DIR="$state/started" \
        MV_FAKE_TEST_ENV="$state/test.env" \
        MV_FAKE_TEST_STARTED="$state/test-started" \
        MV_INTEGRATION_STOP_GRACE_SECONDS=2 \
        MV_RUN_ID=integration-shell-test \
        MV_STATE_DIR="$state" \
        OPERATOR_ENV="$state/operator.env" \
        OPERATOR_KUBECONFIG="$state/operator.kubeconfig" \
        TEST_DATABASE_URL=postgres://test \
        VALKEY_BASE_DOMAIN=integration.valkey.localhost \
        "$repo_root/scripts/test_integrations.sh"
}

normal_state="$temporary/normal"
prepare_state "$normal_state"
mkdir -p "$normal_state/started"
run_script "$normal_state"
[[ -f "$normal_state/started/api" ]]
[[ -f "$normal_state/started/operator" ]]
rg -q '^VALKEY_PUBLIC_PORT=41379$' "$normal_state/started/api.env"
rg -q '^MANAGED_K8S_NODE_COUNT=3$' "$normal_state/started/api.env"
rg -q '^MANAGED_K8S_NODE_CAPACITY_VCPU=4$' "$normal_state/started/api.env"
rg -q '^MANAGED_K8S_NODE_CAPACITY_RAM_GB=8$' "$normal_state/started/api.env"
rg -q '^MANAGED_K8S_NODE_RESERVED_CPU_MILLI=2000$' "$normal_state/started/api.env"
rg -q '^MANAGED_K8S_NODE_RESERVED_RAM_MIB=4096$' "$normal_state/started/api.env"
jwt_secret=$(awk -F= '$1 == "JWT_SECRET" {print $2}' "$normal_state/started/api.env")
[[ -n "$jwt_secret" ]]
rg --fixed-strings --line-regexp --quiet "$jwt_secret" "$normal_state/integration-secret-values"
rg -q '^MANAGED_VALKEY_INTEGRATION_API_URL=http://127\.0\.0\.1:' "$normal_state/test.env"
rg -q '^MANAGED_VALKEY_INTEGRATION_ADMIN_KUBECONFIG=' "$normal_state/test.env"
rg -q '^MANAGED_VALKEY_INTEGRATION_SECRET_VALUES_FILE=' "$normal_state/test.env"
rg -q '^script[[:space:]][0-9]+$' "$normal_state/diagnostics/durations.tsv"
while IFS=$'\t' read -r pid name _; do
    [[ "$name" != api && "$name" != operator ]] || ! process_is_live "$pid"
done <"$normal_state/processes.tsv"

failed_state="$temporary/failed"
prepare_state "$failed_state"
mkdir -p "$failed_state/started"
set +e
MV_FAKE_TEST_FAIL=1 run_script "$failed_state"
failed_status=$?
set -e
[[ "$failed_status" == 7 ]]
[[ -f "$failed_state/diagnostics/api.log" ]]
[[ -f "$failed_state/diagnostics/operator.log" ]]
[[ -f "$failed_state/diagnostics/last-http-failed.txt" ]]
rg -q '^scenarios[[:space:]][0-9]+$' "$failed_state/diagnostics/durations.tsv"
if find "$failed_state/diagnostics" -type f \( -iname '*secret*' -o -iname '*request*' \) | rg -q .; then
    echo "диагностика содержит файл Secret или тело запроса" >&2
    exit 1
fi
"$repo_root/scripts/check_test_artifacts.sh" \
    --secret-values-file "$failed_state/integration-secret-values" \
    "$failed_state/diagnostics"

interrupted_state="$temporary/interrupted"
prepare_state "$interrupted_state"
mkdir -p "$interrupted_state/started"
export -f run_script
export mock_bin repo_root
set +e
MV_FAKE_TEST_BLOCK=1 setsid bash -c 'run_script "$1"' bash "$interrupted_state" &
interrupted_pid=$!
set -e
for _ in {1..100}; do
    [[ ! -f "$interrupted_state/test-started" ]] || break
    sleep 0.05
done
[[ -f "$interrupted_state/test-started" ]]
kill -TERM -- "-$interrupted_pid"
set +e
wait "$interrupted_pid"
interrupted_status=$?
set -e
[[ "$interrupted_status" == 143 ]]
while IFS=$'\t' read -r pid name _; do
    [[ "$name" != api && "$name" != operator ]] || ! process_is_live "$pid"
done <"$interrupted_state/processes.tsv"
