#!/usr/bin/env bash
set -uo pipefail

repo_root=$(git rev-parse --show-toplevel)
root_run_id=${1:?задайте ID общего запуска}
environment_run_id=${2:?задайте ID окружения}
scenario=${3:?задайте сценарий}
pattern=${4:?задайте выражение теста}
nodes=${5:?задайте число нод}
envoy_replicas=${6:?задайте число реплик Envoy}
memory_mib=${7:?задайте оценку памяти}
timeout=${8:?задайте timeout}
operator_image=${9:?задайте образ оператора}
valkey_image=${10:?задайте образ Valkey}
binary=${11:?задайте тестовый бинарник}
results_root=${12:?задайте каталог результатов}
image_archive=${13:?задайте архив образов k3s}
attempt_dir="$results_root/$scenario/attempts/1"
resources=${MV_TEST_RESOURCES_SCRIPT:-$repo_root/scripts/test_resources.sh}
owner="$root_run_id-$scenario"
started_at=$(date +%s)
reservation_active=0

mkdir -p "$attempt_dir"
chmod 0700 "$attempt_dir"

release_reservation() {
    if ((reservation_active == 1)); then
        "$resources" release "$owner" || true
    fi
}

reserve_scenario() {
    local owner_start

    owner_start=$("$resources" start-ticks "$$") || return 2
    "$resources" reserve "$owner" operator "$memory_mib" "$$" "$owner_start" \
        "${MV_TEST_RESOURCE_WAIT_SECONDS:-900}" || return $?
    reservation_active=1
}

run_scenario() {
    env \
        MANAGED_VALKEY_K3S_NODES="$nodes" \
        MANAGED_VALKEY_ENVOY_REPLICAS="$envoy_replicas" \
        MV_TEST_RESOURCE_RESERVED=1 \
        MV_OPERATOR_TEST_PATTERN="$pattern" \
        MV_OPERATOR_TEST_TIMEOUT="$timeout" \
        MV_OPERATOR_TEST_IMAGE="$operator_image" \
        MV_OPERATOR_VALKEY_IMAGE="$valkey_image" \
        MANAGED_VALKEY_K3S_IMAGE_ARCHIVE="$image_archive" \
        MV_OPERATOR_TEST_BINARY="$binary" \
        "$repo_root/scripts/test_environment.sh" exec operator "$environment_run_id" -- \
        "$repo_root/scripts/run_operator_test_scenario_in_environment.sh"
}

set +e
reserve_scenario
reserve_status=$?
set -e
((reserve_status == 0)) || exit "$reserve_status"
trap release_reservation EXIT
set +e
run_scenario 2>&1 | tee "$attempt_dir/log"
command_status=${PIPESTATUS[0]}
set -e
if ((command_status == 0)); then
    printf '%s\n' pass >"$attempt_dir/status"
else
    printf '%s\n' fail >"$attempt_dir/status"
fi
printf '%s\n' "$(($(date +%s) - started_at))" >"$attempt_dir/duration"
exit "$command_status"
