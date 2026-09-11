#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 2 || $# -gt 3 ]]; then
    echo "использование: $0 <sha> <каталог-входных-файлов> [корень-развёртывания]" >&2
    exit 2
fi

release_sha=$1
source_dir=$2
deploy_root=${3:-/opt/managed-valkey}

if [[ ! $release_sha =~ ^[0-9a-f]{40}$ ]]; then
    echo "SHA должен состоять из 40 строчных шестнадцатеричных символов" >&2
    exit 2
fi

required_files=(
    "$source_dir/.env"
    "$source_dir/docker-compose.prod.yml"
    "$source_dir/release.env"
    "$source_dir/install_release.sh"
    "$source_dir/secrets/kubeconfig/api.kubeconfig"
    "$source_dir/images/api.tar.gz"
    "$source_dir/images/migrate.tar.gz"
    "$source_dir/images/caddy.tar.gz"
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

if [[ $(cat "$source_dir/release.env") != "$expected_release_env" ]]; then
    echo "release.env не соответствует выбранному SHA" >&2
    exit 1
fi

if ! awk -F= '
    $1 == "POSTGRES_PASSWORD" && length(substr($0, index($0, "=") + 1)) > 0 { postgres++ ; next }
    $1 == "JWT_SECRET" && length(substr($0, index($0, "=") + 1)) > 0 { jwt++ ; next }
    { invalid++ }
    END { exit !(postgres == 1 && jwt == 1 && invalid == 0) }
' "$source_dir/.env"; then
    echo ".env должен содержать только непустые POSTGRES_PASSWORD и JWT_SECRET" >&2
    exit 1
fi

if [[ $deploy_root != /* || $deploy_root == / || ${deploy_root##*/} != managed-valkey ]]; then
    echo "корень развёртывания должен быть абсолютным каталогом managed-valkey" >&2
    exit 2
fi

for archive in "$source_dir"/images/{api,migrate,caddy}.tar.gz; do
    gzip -dc "$archive" | docker load >/dev/null
done

mkdir -p "$deploy_root"
install -m 0600 "$source_dir/.env" "$deploy_root/.env"
install -d -m 0700 "$deploy_root/secrets" "$deploy_root/secrets/kubeconfig"
install -m 0600 \
    "$source_dir/secrets/kubeconfig/api.kubeconfig" \
    "$deploy_root/secrets/kubeconfig/api.kubeconfig"
if ((EUID == 0)); then
    chown 65532:65532 "$deploy_root/secrets/kubeconfig/api.kubeconfig"
fi
install -m 0644 "$source_dir/docker-compose.prod.yml" "$deploy_root/docker-compose.prod.yml"
install -m 0644 "$source_dir/release.env" "$deploy_root/release.env"
install -m 0755 "$source_dir/install_release.sh" "$deploy_root/install_release.sh"

compose=(
    docker compose
    --project-name managed-valkey
    --env-file "$deploy_root/.env"
    --env-file "$deploy_root/release.env"
    --file "$deploy_root/docker-compose.prod.yml"
)

"${compose[@]}" config --quiet
"${compose[@]}" up --detach --remove-orphans --wait --wait-timeout 180

kubeconfig_mount_read_write=$(docker inspect managed-valkey-api-1 \
    --format '{{range .Mounts}}{{if eq .Destination "/run/secrets/kubeconfig/api.kubeconfig"}}{{.RW}}{{end}}{{end}}')
if [[ $kubeconfig_mount_read_write != false ]]; then
    echo "kubeconfig API не смонтирован только для чтения" >&2
    exit 1
fi

ready_url=${READY_URL:-https://app.h3llo-demo.com/readyz}
ready_attempts=${READY_ATTEMPTS:-30}
ready_delay_seconds=${READY_DELAY_SECONDS:-2}
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

deployed_sha_file=$deploy_root/.deployed-sha
next_deployed_sha=$deploy_root/.deployed-sha-$release_sha-$$
printf '%s\n' "$release_sha" >"$next_deployed_sha"

rm -f -- "$deploy_root/current"
rm -rf -- "$deploy_root/bin" "$deploy_root/releases" "$deploy_root/shared"
mv -f "$next_deployed_sha" "$deployed_sha_file"

echo "установлена ревизия $release_sha"
