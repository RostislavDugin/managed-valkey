#!/usr/bin/env bash
set -euo pipefail

(($# >= 3)) || {
    echo "usage: $0 <compose-project> <image>|--archive <path> <node> [node...]" >&2
    exit 2
}

project=$1
source_kind=image
source_value=$2
shift 2
if [[ "$source_value" == --archive ]]; then
    (($# >= 2)) || {
        echo "load-k3s-image: после --archive нужны путь и нода" >&2
        exit 2
    }
    source_kind=archive
    source_value=$1
    shift
fi

wait_for_containerd() {
    local container=$1 node=$2

    for _ in {1..60}; do
        if docker exec "$container" ctr --namespace k8s.io images list >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    echo "load-k3s-image: containerd не готов в $project/$node" >&2
    return 1
}

archive=$source_value
if [[ "$source_kind" == image ]]; then
    docker image inspect "$source_value" >/dev/null
    archive=$(mktemp)
    trap 'rm -f "$archive"' EXIT
    docker save --output "$archive" "$source_value"
else
    [[ -r "$archive" ]] || {
        echo "load-k3s-image: архив не читается: $archive" >&2
        exit 1
    }
fi

for node in "$@"; do
    mapfile -t containers < <(docker ps -q \
        --filter "label=com.docker.compose.project=$project" \
        --filter "label=com.docker.compose.service=$node")
    if ((${#containers[@]} != 1)); then
        echo "load-k3s-image: для $project/$node найдено контейнеров: ${#containers[@]}" >&2
        exit 1
    fi
    container=${containers[0]}
    running=$(docker inspect "$container" --format '{{.State.Running}}')
    actual_project=$(docker inspect "$container" --format '{{index .Config.Labels "com.docker.compose.project"}}')
    actual_service=$(docker inspect "$container" --format '{{index .Config.Labels "com.docker.compose.service"}}')
    if [[ "$running" != true || "$actual_project" != "$project" || "$actual_service" != "$node" ]]; then
        echo "load-k3s-image: контейнер $container не принадлежит работающему $project/$node" >&2
        exit 1
    fi
    wait_for_containerd "$container" "$node"
    docker exec -i "$container" ctr --namespace k8s.io images import - <"$archive" >/dev/null
done
