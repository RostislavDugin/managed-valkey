#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 ]]; then
    echo "использование: $0 <sha> [корень-развёртывания]" >&2
    exit 2
fi

release_sha=$1
deploy_root=${2:-/opt/managed-valkey}

if [[ ! $release_sha =~ ^[0-9a-f]{40}$ ]]; then
    echo "SHA должен состоять из 40 строчных шестнадцатеричных символов" >&2
    exit 2
fi

release_dir=$deploy_root/releases/$release_sha
shared_env=$deploy_root/shared/.env
compose_file=$release_dir/docker-compose.prod.yml
release_env=$release_dir/release.env
ready_url=${READY_URL:-https://app.h3llo-demo.com/readyz}
ready_attempts=${READY_ATTEMPTS:-30}
ready_delay_seconds=${READY_DELAY_SECONDS:-2}

required_files=(
    "$shared_env"
    "$compose_file"
    "$release_env"
    "$release_dir/images/api.tar.gz"
    "$release_dir/images/migrate.tar.gz"
    "$release_dir/images/caddy.tar.gz"
)

for required_file in "${required_files[@]}"; do
    if [[ ! -f $required_file ]]; then
        echo "не найден обязательный файл: $required_file" >&2
        exit 1
    fi
done

expected_release_env=$(cat <<EOF
RELEASE_SHA=$release_sha
API_IMAGE=managed-valkey/api:$release_sha
MIGRATE_IMAGE=managed-valkey/migrate:$release_sha
CADDY_IMAGE=managed-valkey/caddy:$release_sha
EOF
)

if [[ $(cat "$release_env") != "$expected_release_env" ]]; then
    echo "release.env не соответствует выбранному SHA" >&2
    exit 1
fi

for archive in "$release_dir"/images/{api,migrate,caddy}.tar.gz; do
    gzip -dc "$archive" | docker load >/dev/null
done

compose=(
    docker compose
    --project-name managed-valkey
    --env-file "$shared_env"
    --env-file "$release_env"
    --file "$compose_file"
)

"${compose[@]}" config --quiet
"${compose[@]}" up --detach --remove-orphans --wait --wait-timeout 180

ready=false
for ((attempt = 1; attempt <= ready_attempts; attempt++)); do
    if curl --fail --silent --show-error --max-time 10 "$ready_url" >/dev/null 2>&1; then
        ready=true
        break
    fi

    if ((attempt < ready_attempts)); then
        sleep "$ready_delay_seconds"
    fi
done

if [[ $ready != true ]]; then
    echo "API не прошёл внешнюю проверку готовности: $ready_url" >&2
    exit 1
fi

next_link=$deploy_root/.current-$release_sha-$$
ln -s "$release_dir" "$next_link"
mv -Tf "$next_link" "$deploy_root/current"

echo "активирована версия $release_sha"
