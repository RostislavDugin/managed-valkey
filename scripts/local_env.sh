#!/usr/bin/env bash

load_local_env() {
    local project_root=${1:?задайте корень проекта} env_file allexport_enabled=0

    env_file="$project_root/.env"
    [[ -f "$env_file" ]] || env_file="$project_root/.env.example"
    [[ -f "$env_file" ]] || {
        echo "локальные настройки не найдены: $project_root/.env или $project_root/.env.example" >&2
        return 1
    }

    [[ $- == *a* ]] && allexport_enabled=1
    set -a
    source "$env_file"
    ((allexport_enabled == 1)) || set +a
}
