#!/usr/bin/env bash
set -euo pipefail

(($# >= 3)) || {
    echo "usage: $0 <compose-project> <image> <node> [node...]" >&2
    exit 2
}

project=$1
image=$2
shift 2

docker image inspect "$image" >/dev/null
archive=$(mktemp)
trap 'rm -f "$archive"' EXIT
docker save --output "$archive" "$image"

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
    docker exec -i "$container" ctr --namespace k8s.io images import - <"$archive" >/dev/null
done
