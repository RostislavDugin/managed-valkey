#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
temporary=$(mktemp -d)
trap 'rm -rf -- "$temporary"' EXIT

write_script() {
    local path=$1
    shift
    printf '%s\n' '#!/usr/bin/env bash' 'set -euo pipefail' "$@" >"$path"
    chmod +x "$path"
}

mock_bin="$temporary/bin"
mkdir -p "$mock_bin"
write_script "$mock_bin/curl" \
    'url=${*: -1}' \
    'case "$url" in' \
    '*readyz)' \
    '    [[ -f "$MV_E2E_FAKE_STARTED/api" && -f "$MV_E2E_FAKE_STARTED/operator" ]]' \
    '    ;;' \
    '*/auth) [[ -f "$MV_E2E_FAKE_STARTED/web" ]] ;;' \
    '*) exit 2 ;;' \
    'esac'
write_script "$mock_bin/go" \
    'case "$*" in' \
    '*"tool goose"*) : >"$MV_E2E_FAKE_MIGRATION_STARTED"; exit 0 ;;' \
    '*"run ./api/cmd/api"*) component=api ;;' \
    '*"run -tags integration ./operator/cmd/operator"*) component=operator ;;' \
    '*) exit 2 ;;' \
    'esac' \
    ': >"$MV_E2E_FAKE_STARTED/$component"' \
    'env >"$MV_E2E_FAKE_STARTED/$component.env"' \
    'if [[ "${MV_E2E_FAKE_STUBBORN:-}" == "$component" ]]; then' \
    '    trap '\'''\'' TERM INT' \
    'else' \
    '    trap '\''exit 0'\'' TERM INT' \
    'fi' \
    'while true; do sleep 0.1; done'
write_script "$mock_bin/pnpm" \
    'if [[ "$*" == *"/web dev "* ]]; then' \
    '    : >"$MV_E2E_FAKE_STARTED/web"' \
    '    env >"$MV_E2E_FAKE_STARTED/web.env"' \
    '    trap '\''exit 0'\'' TERM INT' \
    '    while true; do sleep 0.1; done' \
    'fi' \
    'env >"$MV_E2E_FAKE_STARTED/playwright.env"' \
    'printf '\''%s\n'\'' "$*" >"$MV_E2E_FAKE_STARTED/playwright.args"'

prepare_state() {
    local state=$1

    mkdir -p "$state/diagnostics" "$state/started"
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
    local capacity=(
		MANAGED_K8S_CLUSTER_VCPU=21
		MANAGED_K8S_CLUSTER_RAM_GB=69
    )
    local selection=()

    if [[ ${MV_E2E_USE_DEFAULT_CAPACITY:-} == 1 ]]; then
		capacity=()
    fi
    if [[ -v MANAGED_VALKEY_E2E_FILE ]]; then
        selection=(MANAGED_VALKEY_E2E_FILE="$MANAGED_VALKEY_E2E_FILE")
    fi

    env \
        PATH="$mock_bin:$PATH" \
        ADMIN_KUBECONFIG="$state/admin.kubeconfig" \
        API_KUBECONFIG="$state/api.kubeconfig" \
        DIAGNOSTICS_DIR="$state/diagnostics" \
        MANAGED_VALKEY_CA_FILE="$state/ca.crt" \
        MANAGED_VALKEY_COMPOSE_PROJECT=managed-valkey-e2e-shell-test \
        MANAGED_VALKEY_ENVOY_REPLICAS=2 \
        MANAGED_VALKEY_PUBLIC_ADDRESS=127.0.0.1:31379 \
        MANAGED_VALKEY_VALKEY_IMAGE=valkey/valkey:8.1.9 \
        VALKEY_INTEGRATION_REQUEST_CPU=100m \
        VALKEY_INTEGRATION_REQUEST_MEMORY=128Mi \
        MV_E2E_FAKE_STUBBORN="${MV_E2E_FAKE_STUBBORN:-}" \
        MV_E2E_FAKE_STARTED="$state/started" \
        MV_E2E_FAKE_MIGRATION_STARTED="$state/migration-started" \
        MV_E2E_READY_TIMEOUT_SECONDS=2 \
        MV_E2E_STOP_GRACE_SECONDS="${MV_E2E_STOP_GRACE_SECONDS:-2}" \
        MV_RUN_ID=e2e-shell-test \
        MV_STATE_DIR="$state" \
        OPERATOR_ENV="$state/operator.env" \
        OPERATOR_KUBECONFIG="$state/operator.kubeconfig" \
        TEST_DATABASE_URL=postgres://test \
        VALKEY_BASE_DOMAIN=e2e.valkey.localhost \
        "${capacity[@]}" \
        "${selection[@]}" \
        "$repo_root/scripts/test_e2e.sh"
}

normal_state="$temporary/normal"
prepare_state "$normal_state"
run_script "$normal_state"
for component in api operator web; do
    [[ -f "$normal_state/started/$component" ]]
done
rg -q '^VALKEY_IMAGE=valkey/valkey:8\.1\.9$' "$normal_state/started/operator.env"
rg -q '^VALKEY_ENVOY_PROCESSES=2$' "$normal_state/started/operator.env"
rg -q '^VALKEY_INTEGRATION_REQUEST_CPU=100m$' "$normal_state/started/operator.env"
rg -q '^VALKEY_INTEGRATION_REQUEST_MEMORY=128Mi$' "$normal_state/started/operator.env"
rg -q '^VALKEY_PUBLIC_PORT=41379$' "$normal_state/started/operator.env"
rg -q '^MANAGED_K8S_CLUSTER_VCPU=21$' "$normal_state/started/api.env"
rg -q '^MANAGED_K8S_CLUSTER_RAM_GB=69$' "$normal_state/started/api.env"
rg -q '^MANAGED_VALKEY_E2E_ACTION_DELAY_MS=0$' "$normal_state/started/playwright.env"
rg -q '^MANAGED_VALKEY_E2E_SCROLL_PAUSE_MS=0$' "$normal_state/started/playwright.env"
rg -q ' test$' "$normal_state/started/playwright.args"
rg -q '^MANAGED_VALKEY_E2E_ADMIN_KUBECONFIG=' "$normal_state/started/playwright.env"
rg -q '^MANAGED_VALKEY_E2E_COMPOSE_PROJECT=managed-valkey-e2e-shell-test$' \
    "$normal_state/started/playwright.env"
secret_values_file="$normal_state/e2e-secret-values"
rg --fixed-strings --line-regexp --quiet testpassword "$secret_values_file"
jwt_secret=$(awk -F= '$1 == "JWT_SECRET" { print $2 }' "$normal_state/started/api.env")
rg --fixed-strings --line-regexp --quiet "$jwt_secret" "$secret_values_file"
[[ "$(stat -c '%a' "$secret_values_file")" == 600 ]]
for stage in migrations processes-ready playwright script; do
    rg -q "^${stage}[[:space:]][0-9]+$" "$normal_state/diagnostics/durations.tsv"
done
while IFS=$'\t' read -r pid name _; do
    [[ "$name" != api && "$name" != operator && "$name" != web ]] || ! process_is_live "$pid"
done <"$normal_state/processes.tsv"

selected_state="$temporary/selected"
prepare_state "$selected_state"
MANAGED_VALKEY_E2E_FILE=00-single-lifecycle.spec.ts run_script "$selected_state"
rg -q ' exec playwright test specs/00-single-lifecycle\.spec\.ts$' \
    "$selected_state/started/playwright.args"

empty_state="$temporary/empty-selection"
prepare_state "$empty_state"
if MANAGED_VALKEY_E2E_FILE= run_script "$empty_state" >/dev/null 2>&1; then
    echo "пустое имя файла Playwright было принято" >&2
    exit 1
fi
[[ ! -e "$empty_state/migration-started" ]]
[[ ! -e "$empty_state/e2e-secret-values" ]]
[[ ! -e "$empty_state/started/api" ]]

unknown_state="$temporary/unknown-selection"
prepare_state "$unknown_state"
if MANAGED_VALKEY_E2E_FILE=unknown.spec.ts run_script "$unknown_state" >/dev/null 2>&1; then
    echo "неизвестный файл Playwright был принят" >&2
    exit 1
fi
[[ ! -e "$unknown_state/migration-started" ]]
[[ ! -e "$unknown_state/e2e-secret-values" ]]
[[ ! -e "$unknown_state/started/api" ]]

defaults_state="$temporary/defaults"
prepare_state "$defaults_state"
MV_E2E_USE_DEFAULT_CAPACITY=1 run_script "$defaults_state"
rg -q '^MANAGED_K8S_CLUSTER_VCPU=12$' "$defaults_state/started/api.env"
rg -q '^MANAGED_K8S_CLUSTER_RAM_GB=48$' "$defaults_state/started/api.env"

group_state="$temporary/group"
prepare_state "$group_state"
MANAGED_VALKEY_E2E_GROUP=failures run_script "$group_state"
rg -q ' test:failures$' "$group_state/started/playwright.args"

stubborn_state="$temporary/stubborn"
prepare_state "$stubborn_state"
set +e
MV_E2E_FAKE_STUBBORN=operator MV_E2E_STOP_GRACE_SECONDS=0 run_script "$stubborn_state"
stubborn_status=$?
set -e
[[ "$stubborn_status" != 0 ]]
while IFS=$'\t' read -r pid name _; do
    [[ "$name" != api && "$name" != operator && "$name" != web ]] || ! process_is_live "$pid"
done <"$stubborn_state/processes.tsv"

just_dry_run=$(just --dry-run test-e2e 2>&1)
rg -q 'tests/e2e/Justfile test$' <<<"$just_dry_run"
just_dry_run=$(just --dry-run test-e2e-headed 2>&1)
rg -q 'tests/e2e/Justfile test-headed$' <<<"$just_dry_run"
just_dry_run=$(just --dry-run test-e2e-group lifecycle 2>&1)
rg -q 'tests/e2e/Justfile test-group "lifecycle"$' <<<"$just_dry_run"
