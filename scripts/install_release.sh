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
    "$source_dir/activate_release.sh"
    "$source_dir/install_release.sh"
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

release_dir=$deploy_root/releases/$release_sha
mkdir -p "$deploy_root/bin" "$deploy_root/releases" "$deploy_root/shared" "$release_dir/images"

install -m 0600 "$source_dir/.env" "$deploy_root/shared/.env"
install -m 0644 "$source_dir/docker-compose.prod.yml" "$release_dir/docker-compose.prod.yml"
install -m 0644 "$source_dir/release.env" "$release_dir/release.env"
install -m 0644 "$source_dir/images/api.tar.gz" "$release_dir/images/api.tar.gz"
install -m 0644 "$source_dir/images/migrate.tar.gz" "$release_dir/images/migrate.tar.gz"
install -m 0644 "$source_dir/images/caddy.tar.gz" "$release_dir/images/caddy.tar.gz"
install -m 0755 "$source_dir/activate_release.sh" "$deploy_root/bin/activate_release.sh"
install -m 0755 "$source_dir/install_release.sh" "$deploy_root/bin/install_release.sh"

"$deploy_root/bin/activate_release.sh" "$release_sha" "$deploy_root"
