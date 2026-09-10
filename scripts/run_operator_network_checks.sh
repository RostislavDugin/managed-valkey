#!/usr/bin/env bash
set -uo pipefail

repo_root=$(git rev-parse --show-toplevel)
root_run_id=${1:?задайте ID общего запуска}
environment_run_id=${2:?задайте ID окружения}
memory_mib=${3:?задайте оценку памяти}
operator_image=${4:?задайте образ оператора}
valkey_image=${5:?задайте образ Valkey}
run_dir=${6:?задайте каталог результатов}
image_archive=${7:?задайте архив образов k3s}
resources=${MV_TEST_RESOURCES_SCRIPT:-$repo_root/scripts/test_resources.sh}
owner="$root_run_id-network-checks"
started_at=$(date +%s)
reservation_active=0

release_reservation() {
    if ((reservation_active == 1)); then
        "$resources" release "$owner" || true
    fi
}

run_checks_in_environment() {
    local services=(k3s-server k3s-agent-1 k3s-agent-2 k3s-agent-3)

    "$repo_root/scripts/check_operator_test_environment.sh" 4 2
    "$repo_root/scripts/load_k3s_image.sh" "$MANAGED_VALKEY_COMPOSE_PROJECT" \
        --archive "$image_archive" "${services[@]}"
    export MANAGED_VALKEY_OPERATOR_IMAGE=$operator_image
    export MANAGED_VALKEY_VALKEY_IMAGE=$valkey_image
    "$repo_root/scripts/check_operator_network_faults.sh"
}

reserve_checks() {
    local owner_start

    owner_start=$("$resources" start-ticks "$$") || return 2
    "$resources" reserve "$owner" operator "$memory_mib" "$$" "$owner_start" \
        "${MV_TEST_RESOURCE_WAIT_SECONDS:-900}" || return $?
    reservation_active=1
}

run_checks() {
    export -f run_checks_in_environment
    env \
        MANAGED_VALKEY_K3S_NODES=4 \
        MANAGED_VALKEY_ENVOY_REPLICAS=2 \
        MV_TEST_RESOURCE_RESERVED=1 \
        MV_NETWORK_CHECK_REPO_ROOT="$repo_root" \
        MV_NETWORK_CHECK_OPERATOR_IMAGE="$operator_image" \
        MV_NETWORK_CHECK_VALKEY_IMAGE="$valkey_image" \
        MANAGED_VALKEY_K3S_IMAGE_ARCHIVE="$image_archive" \
        "$repo_root/scripts/test_environment.sh" exec operator "$environment_run_id" -- \
        bash -c 'repo_root=$MV_NETWORK_CHECK_REPO_ROOT; operator_image=$MV_NETWORK_CHECK_OPERATOR_IMAGE; valkey_image=$MV_NETWORK_CHECK_VALKEY_IMAGE; image_archive=$MANAGED_VALKEY_K3S_IMAGE_ARCHIVE; run_checks_in_environment'
}

set +e
reserve_checks
reserve_status=$?
set -e
((reserve_status == 0)) || exit "$reserve_status"
trap release_reservation EXIT
set +e
run_checks 2>&1 | tee "$run_dir/network-faults.log"
command_status=${PIPESTATUS[0]}
set -e
if ((command_status == 0)); then
    printf 'pass\n' >"$run_dir/network-faults.status"
else
    printf 'fail\n' >"$run_dir/network-faults.status"
fi
exec 9>>"$run_dir/durations.lock"
flock 9
printf 'network-faults\t%s\t%s\n' "$started_at" "$(($(date +%s) - started_at))" \
    >>"$run_dir/durations.tsv"
flock -u 9
exit "$command_status"
