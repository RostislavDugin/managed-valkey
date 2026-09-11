#!/usr/bin/env bash
set -euo pipefail

secret_value_files=()
artifact_paths=()
while (($# > 0)); do
    case "$1" in
    --secret-values-file)
        (($# >= 2)) || {
            echo "usage: $0 [--secret-values-file <path>] <artifact-path>..." >&2
            exit 2
        }
        secret_value_files+=("$2")
        shift 2
        ;;
    *)
        artifact_paths+=("$1")
        shift
        ;;
    esac
done
((${#artifact_paths[@]} > 0)) || {
    echo "usage: $0 [--secret-values-file <path>] <artifact-path>..." >&2
    exit 2
}

python3 - "${#secret_value_files[@]}" "${secret_value_files[@]}" -- "${artifact_paths[@]}" <<'PY'
import base64
import io
import pathlib
import re
import sys
import zipfile

secret_file_count = int(sys.argv[1])
secret_files = [pathlib.Path(value) for value in sys.argv[2 : 2 + secret_file_count]]
artifact_paths = [pathlib.Path(value) for value in sys.argv[3 + secret_file_count :]]
replacement = b"<redacted>"
embedded_zip = re.compile(rb"data:application/zip;base64,([A-Za-z0-9+/=]+)")
secret_values = [b"testpassword"]

for secret_file in secret_files:
    if secret_file.exists():
        secret_values.extend(value for value in secret_file.read_bytes().splitlines() if value)
secret_values = sorted(set(secret_values), key=len, reverse=True)


def sanitize_zip(data: bytes) -> bytes:
    source = io.BytesIO(data)
    target = io.BytesIO()
    with zipfile.ZipFile(source) as archive, zipfile.ZipFile(target, "w") as sanitized:
        for info in archive.infolist():
            content = archive.read(info.filename)
            sanitized.writestr(info, sanitize_bytes(content))
    return target.getvalue()


def sanitize_bytes(data: bytes) -> bytes:
    if data.startswith(b"PK"):
        try:
            data = sanitize_zip(data)
        except zipfile.BadZipFile:
            pass
    for value in secret_values:
        data = data.replace(value, replacement)

    def sanitize_embedded(match: re.Match[bytes]) -> bytes:
        archive = base64.b64decode(match.group(1))
        encoded = base64.b64encode(sanitize_zip(archive))
        return b"data:application/zip;base64," + encoded

    return embedded_zip.sub(sanitize_embedded, data)


for path in artifact_paths:
    if not path.exists():
        continue
    files = [path] if path.is_file() else (item for item in path.rglob("*") if item.is_file())
    for file_path in files:
        original = file_path.read_bytes()
        sanitized = sanitize_bytes(original)
        if sanitized != original:
            file_path.write_bytes(sanitized)
PY
