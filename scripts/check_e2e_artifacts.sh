#!/usr/bin/env bash
set -euo pipefail

(($# > 0)) || {
    echo "usage: $0 <artifact-path>..." >&2
    exit 2
}

python3 - "$@" <<'PY'
import base64
import io
import pathlib
import re
import sys
import zipfile

needle = b"testpassword"
auth_body = re.compile(rb'(?:\\?")password(?:\\?")[ \t\r\n]*:')
embedded_zip = re.compile(rb"data:application/zip;base64,([A-Za-z0-9+/=]+)")
unsafe = []


def inspect_bytes(data: bytes, label: str) -> None:
    if needle in data:
        unsafe.append((label, "пароль"))
    if auth_body.search(data):
        unsafe.append((label, "тело запроса авторизации"))
    if data.startswith(b"PK"):
        try:
            with zipfile.ZipFile(io.BytesIO(data)) as archive:
                for name in archive.namelist():
                    if not name.endswith("/"):
                        inspect_bytes(archive.read(name), f"{label}:{name}")
        except zipfile.BadZipFile:
            pass
    for match in embedded_zip.finditer(data):
        inspect_bytes(base64.b64decode(match.group(1)), f"{label}:embedded-report")


for raw_path in sys.argv[1:]:
    path = pathlib.Path(raw_path)
    if not path.exists():
        continue
    files = [path] if path.is_file() else (item for item in path.rglob("*") if item.is_file())
    for file_path in files:
        inspect_bytes(file_path.read_bytes(), str(file_path))

if unsafe:
    for label, kind in sorted(set(unsafe)):
        print(f"e2e: {kind} найдено в диагностике: {label}", file=sys.stderr)
    raise SystemExit(1)
PY
