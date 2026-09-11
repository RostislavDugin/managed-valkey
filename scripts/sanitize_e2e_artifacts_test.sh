#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
temporary=$(mktemp -d)
trap 'rm -rf -- "$temporary"' EXIT

secret_values="$temporary/secret-values"
first_secret=0123456789abcdef0123456789abcdef
second_secret=temporary-valkey-password
printf '%s\n' "$first_secret" "$second_secret" >"$secret_values"
chmod 0600 "$secret_values"

artifacts="$temporary/artifacts"
mkdir -p "$artifacts"
printf 'first=%s second=%s account=testpassword\n' \
    "$first_secret" "$second_secret" >"$artifacts/report.txt"
python3 - "$artifacts/archive.zip" "$second_secret" <<'PY'
import sys
import zipfile

with zipfile.ZipFile(sys.argv[1], "w") as archive:
    archive.writestr("trace.txt", f"password={sys.argv[2]}\n")
PY
python3 - "$artifacts/embedded.html" "$artifacts/archive.zip" <<'PY'
import base64
import pathlib
import sys

archive = pathlib.Path(sys.argv[2]).read_bytes()
pathlib.Path(sys.argv[1]).write_bytes(
    b'<a href="data:application/zip;base64,' + base64.b64encode(archive) + b'">trace</a>'
)
PY

"$repo_root/scripts/sanitize_e2e_artifacts.sh" \
    --secret-values-file "$secret_values" "$artifacts"
"$repo_root/scripts/check_e2e_artifacts.sh" \
    --secret-values-file "$secret_values" "$artifacts"

for secret in testpassword "$first_secret" "$second_secret"; do
    ! rg --fixed-strings --quiet "$secret" "$artifacts"
done
rg --fixed-strings --quiet '<redacted>' "$artifacts/report.txt"
python3 - "$artifacts/archive.zip" <<'PY'
import sys
import zipfile

with zipfile.ZipFile(sys.argv[1]) as archive:
    content = archive.read("trace.txt")
if b"<redacted>" not in content:
    raise SystemExit("ZIP не очищен")
PY
