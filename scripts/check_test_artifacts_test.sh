#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT
scanner="$repo_root/scripts/check_test_artifacts.sh"
secret_values="$temporary/secret-values"
password=0123456789abcdef0123456789abcdef
token=eyJhbGciOiJIUzI1NiJ9.integration-test.signature
printf '%s\n' "$password" "$token" >"$secret_values"

safe="$temporary/safe"
mkdir -p "$safe"
printf 'API завершился: status=error\n' >"$safe/api.log"
printf 'operator reconcile failed\n' >"$safe/operator.log"
printf 'kind: ValkeyInstance\nstatus:\n  phase: provisioning\n' >"$safe/resources.yaml"
printf 'kind: Event\nreason: FailedScheduling\n' >"$safe/events.yaml"
printf 'full\t301\n' >"$safe/durations.tsv"
printf 'status=provisioning\n' >"$safe/last-http.txt"
"$scanner" --secret-values-file "$secret_values" "$safe"

assert_rejected() {
    local name=$1 content=$2 fixture

    fixture="$temporary/$name"

    printf '%s\n' "$content" >"$fixture"
    if "$scanner" --secret-values-file "$secret_values" "$fixture" \
        >"$temporary/$name.out" 2>"$temporary/$name.err"; then
        echo "небезопасный файл $name был принят" >&2
        exit 1
    fi
}

assert_rejected password-field '{"password":"hidden"}'
assert_rejected exact-password "request failed: $password"
assert_rejected account-token "Authorization: Bearer $token"
assert_rejected kubeconfig $'clusters:\n- cluster:\n    certificate-authority-data: ZGF0YQ==\nusers:\ncurrent-context: test'
assert_rejected secret-content $'apiVersion: v1\nkind: Secret\ndata:\n  app-password: ZGF0YQ=='

python3 - "$temporary/archive.zip" "$password" <<'PY'
import pathlib
import sys
import zipfile

with zipfile.ZipFile(sys.argv[1], "w") as archive:
    archive.writestr("report.txt", f"password={sys.argv[2]}\n")
PY
if "$scanner" --secret-values-file "$secret_values" "$temporary/archive.zip" \
    >"$temporary/archive.out" 2>"$temporary/archive.err"; then
    echo "секрет внутри ZIP был принят" >&2
    exit 1
fi
rg -q 'секретное значение' "$temporary/archive.err"

python3 - "$temporary/embedded.html" "$temporary/archive.zip" <<'PY'
import base64
import pathlib
import sys

archive = pathlib.Path(sys.argv[2]).read_bytes()
pathlib.Path(sys.argv[1]).write_bytes(
    b'<a href="data:application/zip;base64,' + base64.b64encode(archive) + b'">report</a>'
)
PY
if "$scanner" --secret-values-file "$secret_values" "$temporary/embedded.html" \
    >"$temporary/embedded.out" 2>"$temporary/embedded.err"; then
    echo "секрет внутри встроенного ZIP был принят" >&2
    exit 1
fi
rg -q 'секретное значение' "$temporary/embedded.err"
