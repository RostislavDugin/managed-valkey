#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
docker_bin=$(command -v docker)
work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT

fake_bin=$work_dir/bin
deploy_root=$work_dir/managed-valkey
docker_log=$work_dir/docker.log
mkdir -p "$fake_bin"

cat >"$fake_bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$TEST_DOCKER_LOG"
if [[ ${1:-} == load ]]; then
    cat >/dev/null
fi
EOF

cat >"$fake_bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ $TEST_CURL_RESULT == success ]]
EOF

cat >"$fake_bin/sleep" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

chmod 0755 "$fake_bin/docker" "$fake_bin/curl" "$fake_bin/sleep"

make_source() {
    local release_sha=$1
    local source_dir=$2

    mkdir -p "$source_dir/images"
    cp "$repo_root/docker-compose.prod.yml" "$source_dir/docker-compose.prod.yml"
    cp "$repo_root/scripts/activate_release.sh" "$source_dir/activate_release.sh"
    cp "$repo_root/scripts/install_release.sh" "$source_dir/install_release.sh"
    printf '%s\n' 'POSTGRES_PASSWORD=test-password' 'JWT_SECRET=test-jwt-secret' >"$source_dir/.env"
    cat >"$source_dir/release.env" <<EOF
RELEASE_SHA=$release_sha
API_IMAGE=managed-valkey/api:$release_sha
MIGRATE_IMAGE=managed-valkey/migrate:$release_sha
CADDY_IMAGE=managed-valkey/caddy:$release_sha
EOF
    for image in api migrate caddy; do
        printf '%s\n' "$image-$release_sha" | gzip >"$source_dir/images/$image.tar.gz"
    done
}

sha_a=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
sha_b=bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
source_a=$work_dir/source-a
source_b=$work_dir/source-b
make_source "$sha_a" "$source_a"
make_source "$sha_b" "$source_b"

export PATH=$fake_bin:$PATH
export TEST_DOCKER_LOG=$docker_log
export TEST_CURL_RESULT=success
export READY_ATTEMPTS=1
export READY_DELAY_SECONDS=0

"$repo_root/scripts/install_release.sh" "$sha_a" "$source_a" "$deploy_root"
[[ $(readlink "$deploy_root/current") == "$deploy_root/releases/$sha_a" ]]
[[ $(stat -c '%a' "$deploy_root/shared/.env") == 600 ]]

"$docker_bin" compose \
    --project-name managed-valkey \
    --env-file "$deploy_root/shared/.env" \
    --env-file "$deploy_root/releases/$sha_a/release.env" \
    --file "$deploy_root/releases/$sha_a/docker-compose.prod.yml" \
    config --quiet

export TEST_CURL_RESULT=failure
if "$repo_root/scripts/install_release.sh" "$sha_b" "$source_b" "$deploy_root"; then
    echo "установка с ошибкой проверки готовности завершилась успешно" >&2
    exit 1
fi
[[ $(readlink "$deploy_root/current") == "$deploy_root/releases/$sha_a" ]]

export TEST_CURL_RESULT=success
"$repo_root/scripts/activate_release.sh" "$sha_b" "$deploy_root"
[[ $(readlink "$deploy_root/current") == "$deploy_root/releases/$sha_b" ]]
"$repo_root/scripts/activate_release.sh" "$sha_a" "$deploy_root"
[[ $(readlink "$deploy_root/current") == "$deploy_root/releases/$sha_a" ]]

grep -q -- '--project-name managed-valkey' "$docker_log"
if rg -n 'goose[[:space:]]+down' "$repo_root/scripts/activate_release.sh" "$repo_root/scripts/install_release.sh"; then
    exit 1
fi

echo "сценарии установки и повторного запуска версии прошли"
