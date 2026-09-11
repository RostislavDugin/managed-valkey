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
        if [[ "$(docker inspect "$container" --format '{{.State.Running}}')" != true ]]; then
            echo "load-k3s-image: перезапуск остановленного $project/$node" >&2
            docker start "$container" >/dev/null
        fi
        if docker exec "$container" ctr --namespace k8s.io images list >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    echo "load-k3s-image: containerd не готов в $project/$node" >&2
    return 1
}

import_archive() {
    local container=$1 node=$2 archive=$3 attempt

    for attempt in 1 2 3; do
        wait_for_containerd "$container" "$node"
        if docker exec -i "$container" ctr --namespace k8s.io images import - <"$archive" >/dev/null; then
            return 0
        fi
        if ((attempt < 3)); then
            echo "load-k3s-image: повтор импорта в $project/$node после ошибки ($attempt/3)" >&2
            sleep 2
        fi
    done
    echo "load-k3s-image: не удалось импортировать образ в $project/$node" >&2
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
    mapfile -t containers < <(docker ps -aq \
        --filter "label=com.docker.compose.project=$project" \
        --filter "label=com.docker.compose.service=$node")
    if ((${#containers[@]} != 1)); then
        echo "load-k3s-image: для $project/$node найдено контейнеров: ${#containers[@]}" >&2
        exit 1
    fi
    container=${containers[0]}
    actual_project=$(docker inspect "$container" --format '{{index .Config.Labels "com.docker.compose.project"}}')
    actual_service=$(docker inspect "$container" --format '{{index .Config.Labels "com.docker.compose.service"}}')
    if [[ "$actual_project" != "$project" || "$actual_service" != "$node" ]]; then
        echo "load-k3s-image: контейнер $container не принадлежит $project/$node" >&2
        exit 1
    fi
    import_archive "$container" "$node" "$archive"
done
