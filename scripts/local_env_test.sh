#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT

source "$repo_root/scripts/local_env.sh"

printf 'LOCAL_ENV_TEST_VALUE=example\n' >"$temporary/.env.example"
(
    unset LOCAL_ENV_TEST_VALUE
    load_local_env "$temporary"
    [[ "$LOCAL_ENV_TEST_VALUE" == example ]]
    export -p | rg -q 'LOCAL_ENV_TEST_VALUE="example"'
    [[ $- != *a* ]]
)

printf 'LOCAL_ENV_TEST_VALUE=local\n' >"$temporary/.env"
(
    unset LOCAL_ENV_TEST_VALUE
    load_local_env "$temporary"
    [[ "$LOCAL_ENV_TEST_VALUE" == local ]]
)

missing="$temporary/missing"
mkdir "$missing"
if (load_local_env "$missing") >"$temporary/missing.out" 2>"$temporary/missing.err"; then
    echo "отсутствующие локальные настройки были приняты" >&2
    exit 1
fi
rg -q 'локальные настройки не найдены' "$temporary/missing.err"

if rg -n 'dotenv-path' "$repo_root/api/Justfile" "$repo_root/operator/Justfile"; then
    echo "Justfile загружает .env для команд тестов" >&2
    exit 1
fi
if rg -n '\$repo_root/\.env' "$repo_root/scripts/test_environment.sh"; then
    echo "тестовое окружение читает локальный .env" >&2
    exit 1
fi
