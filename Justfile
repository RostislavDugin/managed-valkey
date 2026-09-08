run:
    #!/usr/bin/env bash
    set -euo pipefail

    docker compose -f docker-compose.dev.yml up -d --wait

    set -m
    just --justfile api/Justfile run &
    api_pid=$!
    pnpm --dir web run dev &
    web_pid=$!

    cleanup() {
        trap - EXIT INT TERM
        kill -TERM -- "-$api_pid" "-$web_pid" 2>/dev/null || true
        wait "$api_pid" "$web_pid" 2>/dev/null || true
    }

    trap cleanup EXIT INT TERM
    set +e
    wait -n "$api_pid" "$web_pid"
    status=$?
    set -e
    exit "$status"
