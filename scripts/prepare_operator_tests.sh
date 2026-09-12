#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
mode=${1:?задайте режим подготовки}
run_dir=${2:?задайте каталог запуска}
binary=${3:?задайте путь тестового бинарника}
operator_image=${4:?задайте образ оператора}
valkey_image=${5:?задайте образ Valkey}
pause_image=${6:-rancher/mirrored-pause:3.10.2}
image_archive=${7:-$run_dir/k3s-images.tar}
envoy_gateway_image=${8:-docker.io/envoyproxy/gateway:v1.8.4}
envoy_image=${9:-docker.io/envoyproxy/envoy:distroless-v1.38.4@sha256:b28fbee81528c5b6e8857412e5e0f48ea5baa0199cf73ab611aa7f88a808eba7}
envtest_version=1.35.0

run_logged() {
    local name=$1 started_at command_status
    shift
    started_at=$(date +%s)
    set +e
    "$@" 2>&1 | tee "$run_dir/$name.log"
    command_status=${PIPESTATUS[0]}
    set -e
    if ((command_status == 0)); then
        printf 'pass\n' >"$run_dir/$name.status"
    else
        printf 'fail\n' >"$run_dir/$name.status"
    fi
    exec 9>>"$run_dir/durations.lock"
    flock 9
    printf '%s\t%s\t%s\n' "$name" "$started_at" "$(($(date +%s) - started_at))" \
        >>"$run_dir/durations.tsv"
    flock -u 9
    return "$command_status"
}

run_script_tests() {
    "$repo_root/scripts/test_resources_test.sh"
    "$repo_root/scripts/cleanup_test_environments_test.sh"
    "$repo_root/scripts/test_environment_test.sh"
    "$repo_root/scripts/run_operator_test_scenario_test.sh"
    "$repo_root/scripts/test_operator_orchestration_test.sh"
    "$repo_root/scripts/test_operator_test_catalog.sh"
}

cd "$repo_root"
case "$mode" in
fast)
    "$repo_root/scripts/check_operator_generated.sh"
    run_logged orchestration run_script_tests
    run_logged unit go test -v -count=1 ./operator/... ./internal/...
    run_logged envtest env \
        KUBEBUILDER_ASSETS="$(go tool setup-envtest use "$envtest_version" \
            --bin-dir "$repo_root/bin/envtest" -p path)" \
        go test -v -tags=envtest -count=1 -run '^Test_Envtest' ./operator/... ./internal/...
    ;;
artifacts)
    run_logged integration-binary go test -c -tags=integration -o "$binary" ./operator/integration
    run_logged operator-image docker build --file operator/Dockerfile \
        --build-arg GO_BUILD_TAGS=integration --tag "$operator_image" .
    ;;
valkey)
    run_logged valkey-image docker pull "$valkey_image"
    run_logged pause-image docker pull "$pause_image"
    run_logged envoy-gateway-image docker pull "$envoy_gateway_image"
    run_logged envoy-image docker pull "$envoy_image"
    ;;
bundle)
    run_logged k3s-image-bundle docker save --output "$image_archive" \
        "$operator_image" "$valkey_image" "$pause_image" "$envoy_gateway_image" "$envoy_image"
    ;;
*)
    echo "prepare-operator-tests: неизвестный режим $mode" >&2
    exit 2
    ;;
esac
