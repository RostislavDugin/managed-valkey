#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
catalog=${MV_OPERATOR_TEST_CATALOG:-$repo_root/scripts/operator_test_scenarios.tsv}
validator=${MV_OPERATOR_TEST_VALIDATOR:-$repo_root/scripts/validate_operator_test_scenarios.sh}
resources=${MV_TEST_RESOURCES_SCRIPT:-$repo_root/scripts/test_resources.sh}
cleanup_script=${MV_TEST_CLEANUP_SCRIPT:-$repo_root/scripts/cleanup_test_environments.sh}
prepare_script=${MV_OPERATOR_TEST_PREPARE_SCRIPT:-$repo_root/scripts/prepare_operator_tests.sh}
scenario_script=${MV_OPERATOR_TEST_SCENARIO_SCRIPT:-$repo_root/scripts/run_operator_test_scenario.sh}
network_script=${MV_OPERATOR_TEST_NETWORK_SCRIPT:-$repo_root/scripts/run_operator_network_checks.sh}
reporter=${MV_OPERATOR_TEST_REPORTER:-$repo_root/scripts/report_operator_test_matrix.sh}
run_id=${MANAGED_VALKEY_OPERATOR_RUN_ID:-operator-$(date -u +%Y%m%d%H%M%S)-$$-$RANDOM}
run_dir=${MV_OPERATOR_TEST_RUN_DIR:-$repo_root/tmp/operator-test-runs/$run_id}
results_dir="$run_dir/scenarios"
binary="$run_dir/operator-integration.test"
operator_image="managed-valkey/operator:$run_id"
valkey_image=valkey/valkey:8.1.9
pause_image=rancher/mirrored-pause:3.10.2
envoy_gateway_image=docker.io/envoyproxy/gateway:v1.8.4
envoy_image='docker.io/envoyproxy/envoy:distroless-v1.38.4@sha256:b28fbee81528c5b6e8857412e5e0f48ea5baa0199cf73ab611aa7f88a808eba7'
image_archive="$run_dir/k3s-images.tar"
max_parallel=${MV_OPERATOR_TEST_MAX_PARALLEL:-12}
network_memory_mib=${MV_OPERATOR_NETWORK_TEST_MEMORY_MIB:-6144}
preparation_memory_mib=${MV_OPERATOR_PREPARATION_MEMORY_MIB:-4096}
declare -A scenario_by_pid=()
declare -A status_by_scenario=()
declare -A preparation_by_pid=()
signal_status=0
preparation_reservation_owner="$run_id-preparation"
preparation_reservation_active=0

fail() {
    echo "operator-test: $*" >&2
    exit 2
}

[[ "$max_parallel" =~ ^[1-9][0-9]*$ ]] ||
    fail "MV_OPERATOR_TEST_MAX_PARALLEL должен быть положительным числом"
[[ "$network_memory_mib" =~ ^[1-9][0-9]*$ ]] ||
    fail "MV_OPERATOR_NETWORK_TEST_MEMORY_MIB должен быть положительным числом"
[[ "$preparation_memory_mib" =~ ^[1-9][0-9]*$ ]] ||
    fail "MV_OPERATOR_PREPARATION_MEMORY_MIB должен быть положительным числом"
[[ "${MV_OPERATOR_CANCEL_GRACE_SECONDS:-60}" =~ ^[0-9]+$ ]] ||
    fail "MV_OPERATOR_CANCEL_GRACE_SECONDS должно быть целым неотрицательным числом"
[[ -x "$validator" ]] || fail "проверка каталога не найдена: $validator"

mkdir -p "$results_dir"
chmod 0700 "$run_dir" "$results_dir"
printf '%s\n' "$$" >"$run_dir/owner.pid"
"$resources" start-ticks "$$" >"$run_dir/owner.start-ticks"
tr -d '\n' </proc/sys/kernel/random/boot_id >"$run_dir/owner.boot-id"

stop_process_groups() {
    local pid

    for pid in "${!preparation_by_pid[@]}"; do
        kill -TERM -- "-$pid" 2>/dev/null || true
    done
    for pid in "${!scenario_by_pid[@]}"; do
        kill -TERM -- "-$pid" 2>/dev/null || true
    done
}

handle_signal() {
    signal_status=$1
    stop_process_groups
    wait_for_all_process_groups
}

process_group_active() {
    local group=$1

    ps -eo pgid=,stat= | awk -v group="$group" '
        $1 == group && $2 !~ /^Z/ { active = 1 }
        END { exit !active }
    '
}

wait_for_process_group() {
    local group=$1 grace_seconds=${MV_OPERATOR_CANCEL_GRACE_SECONDS:-60} deadline killed=0

    deadline=$((SECONDS + grace_seconds))
    while process_group_active "$group"; do
        if ((SECONDS >= deadline && killed == 0)); then
            kill -KILL -- "-$group" 2>/dev/null || true
            killed=1
        fi
        sleep 0.1
    done
    wait "$group" 2>/dev/null || true
    ((killed == 0))
}

wait_for_preparation() {
    local pid=$1 command_status=0

    wait "$pid" || command_status=$?
    if ! wait_for_process_group "$pid" && ((command_status == 0)); then
        command_status=1
    fi
    unset 'preparation_by_pid[$pid]'
    return "$command_status"
}

wait_for_all_process_groups() {
    local pid

    for pid in "${!preparation_by_pid[@]}"; do
        wait_for_process_group "$pid" || true
        unset 'preparation_by_pid[$pid]'
    done
    for pid in "${!scenario_by_pid[@]}"; do
        wait_for_process_group "$pid" || true
        unset 'scenario_by_pid[$pid]'
    done
}

reserve_preparation_resources() {
    local owner_start

    owner_start=$("$resources" start-ticks "$$")
    "$resources" reserve "$preparation_reservation_owner" operator \
        "$preparation_memory_mib" "$$" "$owner_start" \
        "${MV_TEST_RESOURCE_WAIT_SECONDS:-900}"
    preparation_reservation_active=1
}

release_preparation_resources() {
    if ((preparation_reservation_active == 1)); then
        "$resources" release "$preparation_reservation_owner" || return $?
        preparation_reservation_active=0
    fi
}

wait_for_scenario() {
    local finished_pid="" command_status scenario

    set +e
    wait -n -p finished_pid "${!scenario_by_pid[@]}"
    command_status=$?
    set -e
    [[ -n "$finished_pid" ]] || return 0
    scenario=${scenario_by_pid[$finished_pid]}
    if ! wait_for_process_group "$finished_pid" && ((command_status == 0)); then
        command_status=1
    fi
    status_by_scenario[$scenario]=$command_status
    unset 'scenario_by_pid[$finished_pid]'
}

launch_scenario() {
    local scenario=$1 pattern=$2 nodes=$3 envoy_replicas=$4 memory_mib=$5 timeout=$6
    local scenario_run_id checksum

    checksum=$(printf '%s' "$run_id-$scenario" | cksum | awk '{ print $1 }')
    scenario_run_id="op-${scenario:0:20}-$checksum"
    setsid "$scenario_script" \
        "$run_id" "$scenario_run_id" "$scenario" "$pattern" "$nodes" "$envoy_replicas" \
        "$memory_mib" "$timeout" "$operator_image" "$valkey_image" "$binary" "$results_dir" \
        "$image_archive" &
    scenario_by_pid[$!]=$scenario
}

launch_network_checks() {
    local checksum environment_run_id

    checksum=$(printf '%s' "$run_id-network-checks" | cksum | awk '{ print $1 }')
    environment_run_id="op-network-$checksum"
    setsid "$network_script" \
        "$run_id" "$environment_run_id" "$network_memory_mib" \
        "$operator_image" "$valkey_image" "$run_dir" "$image_archive" &
    scenario_by_pid[$!]=network-checks
}

run_scenarios() {
    local row scenario pattern nodes envoy_replicas memory_mib estimated_seconds timeout
    local -a rows=()

    launch_network_checks
    mapfile -t rows < <(tail -n +2 "$catalog" | sort -t $'\t' -k6,6nr)
    for row in "${rows[@]}"; do
        ((signal_status == 0)) || break
        while ((${#scenario_by_pid[@]} >= max_parallel)); do
            wait_for_scenario
        done
        IFS=$'\t' read -r scenario pattern nodes envoy_replicas memory_mib estimated_seconds timeout <<<"$row"
        launch_scenario "$scenario" "$pattern" "$nodes" "$envoy_replicas" "$memory_mib" "$timeout"
    done
    while ((${#scenario_by_pid[@]} > 0)); do
        wait_for_scenario
    done
}

report_results() {
    local command_status=$? cleanup_status=0 report_status=0 release_status=0
    local scenario scenario_status

    trap - EXIT
    trap - INT TERM
    set +e
    stop_process_groups
    wait_for_all_process_groups
    release_preparation_resources
    release_status=$?
    "$cleanup_script" operator
    cleanup_status=$?
    "$reporter" "$run_dir" full 2>&1 |
        tee "$run_dir/matrix-report.log"
    report_status=${PIPESTATUS[0]}
    set -e
    for scenario in "${!status_by_scenario[@]}"; do
        scenario_status=${status_by_scenario[$scenario]}
        if ((scenario_status != 0 && command_status == 0)); then
            command_status=$scenario_status
        fi
    done
    if ((signal_status != 0)); then
        exit "$signal_status"
    fi
    if ((command_status != 0)); then
        exit "$command_status"
    fi
    if ((cleanup_status != 0)); then
        exit "$cleanup_status"
    fi
    if ((release_status != 0)); then
        exit "$release_status"
    fi
    exit "$report_status"
}

cd "$repo_root"
: >"$run_dir/durations.tsv"
trap 'handle_signal 130' INT
trap 'handle_signal 143' TERM
trap report_results EXIT
"$cleanup_script" operator
"$validator" "$catalog"
"$resources" status | tee "$run_dir/resources-before.tsv"
reserve_preparation_resources

for preparation in fast artifacts valkey; do
    ((signal_status == 0)) || exit "$signal_status"
    setsid "$prepare_script" "$preparation" \
        "$run_dir" "$binary" "$operator_image" "$valkey_image" "$pause_image" "$image_archive" \
        "$envoy_gateway_image" "$envoy_image" &
    preparation_by_pid[$!]=$preparation
done

preparation_status=0
for pid in "${!preparation_by_pid[@]}"; do
    [[ "${preparation_by_pid[$pid]}" != fast ]] || continue
    wait_for_preparation "$pid" || preparation_status=$?
done
((preparation_status == 0)) || exit "$preparation_status"
((signal_status == 0)) || exit "$signal_status"

setsid "$prepare_script" bundle \
    "$run_dir" "$binary" "$operator_image" "$valkey_image" "$pause_image" "$image_archive" \
    "$envoy_gateway_image" "$envoy_image" &
preparation_by_pid[$!]=bundle
for pid in "${!preparation_by_pid[@]}"; do
    [[ "${preparation_by_pid[$pid]}" == bundle ]] || continue
    wait_for_preparation "$pid" || preparation_status=$?
done
((preparation_status == 0)) || exit "$preparation_status"

for pid in "${!preparation_by_pid[@]}"; do
    wait_for_preparation "$pid" || preparation_status=$?
done
((preparation_status == 0)) || exit "$preparation_status"
release_preparation_resources
((signal_status == 0)) || exit "$signal_status"

run_scenarios
"$resources" status | tee "$run_dir/resources-after.tsv"
