#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
run_id=invalid-topology
state="$temporary/operator/$run_id"

if MV_TEST_STATE_ROOT="$temporary" \
    MANAGED_VALKEY_K3S_NODES=5 \
    MANAGED_VALKEY_ENVOY_REPLICAS=1 \
    "$repo_root/scripts/test_environment.sh" prepare operator "$run_id" \
    >"$temporary/stdout" 2>"$temporary/stderr"; then
    echo "недопустимая топология была принята" >&2
    exit 1
fi
[[ ! -e "$state" ]]
rg -q 'MANAGED_VALKEY_K3S_NODES должно быть от 1 до 4' "$temporary/stderr"

write_script() {
    local path=$1
    shift
    printf '%s\n' '#!/usr/bin/env bash' 'set -euo pipefail' "$@" >"$path"
    chmod +x "$path"
}

mock_bin=$temporary/bin
mkdir -p "$mock_bin"
write_script "$mock_bin/docker" \
    'case "${1:-}" in' \
    'compose) exit 0 ;;' \
    'ps)' \
    '    [[ "$*" != *com.h3llo-demo.managed-valkey.test-network* ]] || exit 0' \
    '    printf '\''owned-container\nforeign-container\n'\''' \
    '    ;;' \
    'inspect)' \
    '    if [[ "$2" == owned-container ]]; then' \
    '        [[ "$*" == *"{{.Name}}"* ]] && printf '\''/owned-container\n'\'' || printf '\''%s\n'\'' "$MV_EXPECTED_PROJECT"' \
    '    else' \
    '        [[ "$*" == *"{{.Name}}"* ]] && printf '\''/foreign-container\n'\'' || printf '\''foreign-project\n'\''' \
    '    fi' \
    '    ;;' \
    'network)' \
    '    case "$2" in' \
    '    ls) printf '\''owned-network\nforeign-network\n'\'' ;;' \
    '    inspect)' \
    '        [[ "$3" != "$MV_EXPECTED_NETWORK" ]] || exit 1' \
    '        if [[ "$3" == owned-network ]]; then' \
    '            [[ "$*" == *"{{.Name}}"* ]] && printf '\''owned-network\n'\'' || printf '\''%s\n'\'' "$MV_EXPECTED_PROJECT"' \
    '        else' \
    '            [[ "$*" == *"{{.Name}}"* ]] && printf '\''foreign-network\n'\'' || printf '\''foreign-project\n'\''' \
    '        fi' \
    '        ;;' \
    '    rm) exit 0 ;;' \
    '    esac' \
    '    ;;' \
    'volume)' \
    '    case "$2" in' \
    '    ls) printf '\''owned-volume\nforeign-volume\n'\'' ;;' \
    '    inspect)' \
    '        if [[ "$3" == owned-volume ]]; then' \
    '            [[ "$*" == *"{{.Name}}"* ]] && printf '\''owned-volume\n'\'' || printf '\''%s\n'\'' "$MV_EXPECTED_PROJECT"' \
    '        else' \
    '            [[ "$*" == *"{{.Name}}"* ]] && printf '\''foreign-volume\n'\'' || printf '\''foreign-project\n'\''' \
    '        fi' \
    '        ;;' \
    '    esac' \
    '    ;;' \
    'esac'
write_script "$mock_bin/ip" \
    '[[ "${1:-}" == route && "${2:-}" == show && "${3:-}" == exact ]] || exit 2' \
    'if [[ "$4" == 10.64.0.0/16 ]]; then' \
    '    printf '\''10.64.0.0/16 via 172.28.0.10\n'\''' \
    'elif [[ "$4" == 10.65.0.0/16 && ! -f "$MV_SECOND_ROUTE_REMOVED" ]]; then' \
    '    printf '\''10.65.0.0/16 via 172.28.0.11\n'\''' \
    'fi'
write_script "$mock_bin/sudo" \
    '[[ "$1" == ip && "$2" == route && "$3" == del ]]' \
    '[[ "$4" != 10.64.0.0/16 ]] || exit 12' \
    ': >"$MV_SECOND_ROUTE_REMOVED"'
write_script "$mock_bin/kubectl" \
    'case "$*" in' \
    '*"get nodes,pods,services,statefulsets,deployments,endpointslices,valkeyinstances.valkey.h3llo-demo.com"*) printf '\''kind: List\n'\'' ;;' \
    '*"get events"*) printf '\''kind: EventList\n'\'' ;;' \
    '*"logs -n valkey-system"*) printf '\''operator pod log\n'\'' ;;' \
    '*) exit 2 ;;' \
    'esac'

cleanup_state=$temporary/cleanup-state
cleanup_project=managed-valkey-api-cleanup-routes
cleanup_network=${cleanup_project}_default
mkdir -p "$cleanup_state"
printf '%s\n' \
    'MV_SUITE=api' \
    'MV_RUN_ID=cleanup-routes' \
    "MV_STATE_DIR=$(printf '%q' "$cleanup_state")" \
    "MANAGED_VALKEY_COMPOSE_PROJECT=$cleanup_project" \
    "MANAGED_VALKEY_DOCKER_NETWORK=$cleanup_network" \
    "ROUTES_FILE=$(printf '%q' "$cleanup_state/routes.tsv")" \
    >"$cleanup_state/environment.env"
printf '%s\n' \
    $'10.64.0.0/16\t172.28.0.10' \
    $'10.65.0.0/16\t172.28.0.11' \
    >"$cleanup_state/routes.tsv"
printf 'ready\n' >"$cleanup_state/status"
sleep 30 &
remaining_pid=$!
remaining_start=$("$repo_root/scripts/test_resources.sh" start-ticks "$remaining_pid")
printf '%s\tsleep\t%s\t%s\n' \
    "$remaining_pid" "$remaining_start" "$(tr -d '\n' </proc/sys/kernel/random/boot_id)" \
    >"$cleanup_state/processes.tsv"

export MV_EXPECTED_PROJECT=$cleanup_project
export MV_EXPECTED_NETWORK=$cleanup_network
export MV_SECOND_ROUTE_REMOVED=$temporary/second-route-removed
if PATH="$mock_bin:$PATH" "$repo_root/scripts/test_environment.sh" cleanup "$cleanup_state" \
    >"$temporary/cleanup.out" 2>"$temporary/cleanup.err"; then
    echo "ошибка удаления маршрута была потеряна" >&2
    kill "$remaining_pid"
    wait "$remaining_pid" 2>/dev/null || true
    exit 1
fi
kill "$remaining_pid"
wait "$remaining_pid" 2>/dev/null || true
[[ "$(<"$cleanup_state/status")" == cleanup-failed ]]
rg -q 'остаток container: id=owned-container name=/owned-container' "$temporary/cleanup.err"
rg -q 'остаток network: id=owned-network name=owned-network' "$temporary/cleanup.err"
rg -q 'остаток volume: id=owned-volume name=owned-volume' "$temporary/cleanup.err"
rg -q 'остаток route: destination=10.64.0.0/16 gateway=172.28.0.10' "$temporary/cleanup.err"
rg -q "остаток process: pid=$remaining_pid command=sleep" "$temporary/cleanup.err"
! rg -q 'foreign-' "$temporary/cleanup.err"
! rg -q 'destination=10.65.0.0/16' "$temporary/cleanup.err"

export MV_TEST_STATE_ROOT=$temporary/wait-state
source <(sed '/^command_name=/,$d' "$repo_root/scripts/test_environment.sh")

(
    unset MV_TEST_K3S_ENABLED
    configure_environment_mode api
    [[ "$MV_TEST_K3S_ENABLED" == 1 ]]
)
(
    MV_TEST_K3S_ENABLED=0
    configure_environment_mode api
    [[ "$MV_TEST_K3S_ENABLED" == 0 ]]
)
if (MV_TEST_K3S_ENABLED=0; configure_environment_mode operator) 2>"$temporary/mode.err"; then
    echo "режим без k3s был разрешён оператору" >&2
    exit 1
fi
rg -q 'окружение без k3s поддерживает только профиль api' "$temporary/mode.err"

(
    unset MANAGED_VALKEY_K3S_NODES MANAGED_VALKEY_ENVOY_REPLICAS
    configure_cluster_profile api
    [[ "$BOOTSTRAP_PROFILE" == api ]]
    [[ "$MANAGED_VALKEY_K3S_NODES" == 1 ]]
    [[ "$MANAGED_VALKEY_ENVOY_REPLICAS" == 0 ]]
)
(
    unset MANAGED_VALKEY_K3S_NODES MANAGED_VALKEY_ENVOY_REPLICAS
    configure_cluster_profile operator
    [[ "$BOOTSTRAP_PROFILE" == full ]]
    [[ "$MANAGED_VALKEY_K3S_NODES" == 3 ]]
    [[ "$MANAGED_VALKEY_ENVOY_REPLICAS" == 2 ]]
)
(
    unset MANAGED_VALKEY_K3S_NODES MANAGED_VALKEY_ENVOY_REPLICAS
    configure_cluster_profile integration
    [[ "$BOOTSTRAP_PROFILE" == full ]]
    [[ "$MANAGED_VALKEY_K3S_NODES" == 1 ]]
    [[ "$MANAGED_VALKEY_ENVOY_REPLICAS" == 1 ]]
)
(
    unset MANAGED_VALKEY_K3S_NODES MANAGED_VALKEY_ENVOY_REPLICAS
    configure_cluster_profile e2e
    [[ "$BOOTSTRAP_PROFILE" == full ]]
    [[ "$MANAGED_VALKEY_K3S_NODES" == 3 ]]
    [[ "$MANAGED_VALKEY_ENVOY_REPLICAS" == 2 ]]
)

[[ "$(MV_TEST_K3S_ENABLED=0 default_memory_mib_for_suite api)" == 512 ]]
[[ "$(default_memory_mib_for_suite api)" == 3072 ]]
[[ "$(default_memory_mib_for_suite operator)" == 3072 ]]
[[ "$(default_memory_mib_for_suite integration)" == 8192 ]]
[[ "$(default_memory_mib_for_suite e2e)" == 3072 ]]

diagnostic_state=$temporary/integration-diagnostics
mkdir -p "$diagnostic_state/diagnostics"
: >"$diagnostic_state/admin.kubeconfig"
printf 'external operator log\n' >"$diagnostic_state/diagnostics/operator.log"
printf '%s\n' \
    'MV_SUITE=integration' \
    'MV_RUN_ID=integration-diagnostics' \
    "DIAGNOSTICS_DIR=$(printf '%q' "$diagnostic_state/diagnostics")" \
    "ADMIN_KUBECONFIG=$(printf '%q' "$diagnostic_state/admin.kubeconfig")" \
    'MANAGED_VALKEY_COMPOSE_PROJECT=integration-diagnostics' \
    >"$diagnostic_state/environment.env"
PATH="$mock_bin:$PATH" collect_diagnostics "$diagnostic_state"
rg -q '^external operator log$' "$diagnostic_state/diagnostics/operator.log"
rg -q '^operator pod log$' "$diagnostic_state/diagnostics/operator-kubernetes.log"

reserve_environment_resources() { :; }
release_environment_resources() { :; }
prepare() {
    local suite=$1 run_id=$2 test_state="$state_root/$suite/$run_id"

    mkdir -p "$test_state/diagnostics"
    printf 'MV_SUITE=%q\nMV_RUN_ID=%q\nMV_STATE_DIR=%q\nDIAGNOSTICS_DIR=%q\n' \
        "$suite" "$run_id" "$test_state" "$test_state/diagnostics" >"$test_state/environment.env"
}
collect_diagnostics() { :; }
cleanup() { :; }
slow_success=$temporary/slow-success
write_script "$slow_success" 'sleep 1'
if ! MV_INTEGRATION_TARGET_SECONDS=0 run_in_environment integration target-overrun -- "$slow_success" \
    >"$temporary/duration.out" 2>"$temporary/duration.err"; then
    echo "успешный integration-запуск стал ошибкой из-за длительности" >&2
    exit 1
fi
rg -q '^full[[:space:]][1-9][0-9]*$' \
    "$state_root/integration/target-overrun/diagnostics/durations.tsv"
rg -q 'target_seconds=0 target_exceeded=true' "$temporary/duration.err"
stubborn=$temporary/stubborn
write_script "$stubborn" \
    'sleep 0.2' \
    'trap '\'''\'' TERM' \
    'sleep 30 &'
export MV_TEST_CANCEL_GRACE_SECONDS=0
if run_in_environment operator forced-kill -- "$stubborn"; then
    echo "принудительный KILL дочерней группы не сделал запуск неуспешным" >&2
    exit 1
fi
stubborn_pid=$(cut -f1 "$state_root/operator/forced-kill/processes.tsv")
if ps -eo pgid=,stat= | awk -v group="$stubborn_pid" '$1 == group && $2 !~ /^Z/ { found = 1 } END { exit !found }'; then
    echo "после принудительного KILL осталась дочерняя группа $stubborn_pid" >&2
    exit 1
fi
