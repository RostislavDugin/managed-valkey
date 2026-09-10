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
password_field = re.compile(rb'(?:\\?")password(?:\\?")[ \t\r\n]*:')
secret_document = re.compile(rb"(?m)^kind:[ \t]*Secret[ \t]*$")
embedded_zip = re.compile(rb"data:application/zip;base64,([A-Za-z0-9+/=]+)")
unsafe = []
secret_values = [b"testpassword"]

for secret_file in secret_files:
    if not secret_file.exists():
        continue
    secret_values.extend(value for value in secret_file.read_bytes().splitlines() if value)


def inspect_bytes(data: bytes, label: str) -> None:
    if password_field.search(data):
        unsafe.append((label, "поле password"))
    if secret_document.search(data):
        unsafe.append((label, "содержимое Secret"))
    if b"certificate-authority-data:" in data or (
        b"current-context:" in data and b"clusters:" in data and b"users:" in data
    ):
        unsafe.append((label, "kubeconfig"))
    if any(value in data for value in secret_values):
        unsafe.append((label, "секретное значение"))
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


for path in artifact_paths:
    if not path.exists():
        continue
    files = [path] if path.is_file() else (item for item in path.rglob("*") if item.is_file())
    for file_path in files:
        inspect_bytes(file_path.read_bytes(), str(file_path))

if unsafe:
    for label, kind in sorted(set(unsafe)):
        print(f"diagnostics: {kind} найдено в {label}", file=sys.stderr)
    raise SystemExit(1)
PY
