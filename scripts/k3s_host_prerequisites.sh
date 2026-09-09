#!/usr/bin/env bash
set -euo pipefail

minimum_inotify_instances=1024
minimum_inotify_watches=1048576

raise_sysctl() {
    local name=$1 minimum=$2 current

    current=$(sysctl -n "$name")
    if ((current < minimum)); then
        echo "k3s-host: $name=$current, поднимаю до $minimum" >&2
        sudo sysctl -w "$name=$minimum" >/dev/null
    fi

    current=$(sysctl -n "$name")
    if ((current < minimum)); then
        echo "k3s-host: $name должен быть не меньше $minimum" >&2
        return 1
    fi
}

command -v sysctl >/dev/null 2>&1 || {
    echo "k3s-host: команда sysctl не установлена" >&2
    exit 1
}
command -v sudo >/dev/null 2>&1 || {
    echo "k3s-host: команда sudo не установлена" >&2
    exit 1
}

raise_sysctl fs.inotify.max_user_instances "$minimum_inotify_instances"
raise_sysctl fs.inotify.max_user_watches "$minimum_inotify_watches"
