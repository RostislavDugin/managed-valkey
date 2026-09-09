#!/usr/bin/env bash
set -euo pipefail

(($# > 0)) || {
    echo "usage: $0 <artifact-path>..." >&2
    exit 2
}

MANAGED_VALKEY_E2E_ACCOUNT_PASSWORD=${MANAGED_VALKEY_E2E_ACCOUNT_PASSWORD:?MANAGED_VALKEY_E2E_ACCOUNT_PASSWORD не задан} \
    python3 - "$@" <<'PY'
import base64
import io
import os
import pathlib
import re
import sys
import zipfile

needle = os.environ["MANAGED_VALKEY_E2E_ACCOUNT_PASSWORD"].encode()
replacement = b"<redacted>"
embedded_zip = re.compile(rb"data:application/zip;base64,([A-Za-z0-9+/=]+)")


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
    data = data.replace(needle, replacement)

    def sanitize_embedded(match: re.Match[bytes]) -> bytes:
        archive = base64.b64decode(match.group(1))
        encoded = base64.b64encode(sanitize_zip(archive))
        return b"data:application/zip;base64," + encoded

    return embedded_zip.sub(sanitize_embedded, data)


for raw_path in sys.argv[1:]:
    path = pathlib.Path(raw_path)
    if not path.exists():
        continue
    files = [path] if path.is_file() else (item for item in path.rglob("*") if item.is_file())
    for file_path in files:
        original = file_path.read_bytes()
        sanitized = sanitize_bytes(original)
        if sanitized != original:
            file_path.write_bytes(sanitized)
PY
