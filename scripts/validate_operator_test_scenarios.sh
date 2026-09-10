#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
default_catalog=$repo_root/scripts/operator_test_scenarios.tsv
catalog=${1:-$default_catalog}
discovered_file=${2:-}

python3 - "$repo_root" "$catalog" "$discovered_file" <<'PY'
import csv
import pathlib
import re
import subprocess
import sys

repo_root = pathlib.Path(sys.argv[1])
catalog_path = pathlib.Path(sys.argv[2])
discovered_path = pathlib.Path(sys.argv[3]) if sys.argv[3] else None
columns = [
    "scenario",
    "test_pattern",
    "nodes",
    "envoy_replicas",
    "memory_mib",
    "estimated_seconds",
    "timeout",
]
required_matrix = {
    *(f"HA-{number:02d}" for number in range(1, 9)),
    *(f"FP-{number:02d}" for number in range(1, 13)),
    *(f"ND-{number:02d}" for number in range(1, 10)),
    *(f"NT-{number:02d}" for number in range(1, 7)),
    *(f"OP-{number:02d}" for number in range(1, 9)),
    *(f"RZ-{number:02d}" for number in range(1, 12)),
    *(f"PW-{number:02d}" for number in range(1, 11)),
    *(f"DL-{number:02d}" for number in range(1, 5)),
    "CT-09",
    "CT-10",
    "CT-11",
    "CT-14",
}


def fail(messages):
    for message in messages:
        print(f"каталог тестов: {message}", file=sys.stderr)
    raise SystemExit(1)


if not catalog_path.is_file():
    fail([f"файл не найден: {catalog_path}"])

with catalog_path.open(newline="", encoding="utf-8") as source:
    reader = csv.DictReader(source, delimiter="\t")
    if reader.fieldnames != columns:
        fail([f"ожидались столбцы {','.join(columns)}"])
    rows = list(reader)

errors = []
if not rows:
    errors.append("нет назначенных сценариев")

scenario_rows = {}
test_rows = {}
matrix_ids = set()
for line_number, row in enumerate(rows, 2):
    if None in row or any(row[field] is None for field in columns):
        errors.append(f"строка {line_number}: неверное число полей")
        continue
    scenario = row["scenario"]
    pattern = row["test_pattern"]
    scenario_rows.setdefault(scenario, []).append(line_number)

    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_-]*", scenario):
        errors.append(f"строка {line_number}: небезопасное имя сценария {scenario!r}")

    match = re.fullmatch(r"\^(Test[A-Za-z0-9_]+)\$", pattern)
    if not match:
        errors.append(f"строка {line_number}: выражение должно точно выбирать один Test: {pattern!r}")
        continue

    test_name = match.group(1)
    test_rows.setdefault(test_name, []).append(line_number)
    matrix_ids.update(
        f"{prefix}-{number}"
        for prefix, number in re.findall(r"(HA|FP|ND|NT|OP|RZ|PW|DL|CT)([0-9]{2})", test_name)
    )

    for field, minimum, maximum in (
        ("nodes", 1, 4),
        ("envoy_replicas", 1, 2),
        ("memory_mib", 1, None),
        ("estimated_seconds", 1, None),
    ):
        try:
            value = int(row[field])
        except ValueError:
            errors.append(f"строка {line_number}: {field} должно быть целым числом")
            continue
        if value < minimum or maximum is not None and value > maximum:
            expected = f"{minimum}..{maximum}" if maximum is not None else f">= {minimum}"
            errors.append(f"строка {line_number}: {field} должно быть {expected}")

    try:
        nodes = int(row["nodes"])
        envoy_replicas = int(row["envoy_replicas"])
        if envoy_replicas > nodes:
            errors.append(f"строка {line_number}: реплик Envoy больше, чем нод")
    except ValueError:
        pass

    if not re.fullmatch(r"[1-9][0-9]*(?:ms|s|m|h)", row["timeout"]):
        errors.append(f"строка {line_number}: неверный timeout {row['timeout']!r}")

for scenario, line_numbers in scenario_rows.items():
    if len(line_numbers) > 1:
        errors.append(f"сценарий {scenario!r} повторяется в строках {line_numbers}")
for test_name, line_numbers in test_rows.items():
    if len(line_numbers) > 1:
        errors.append(f"тест {test_name} назначен повторно в строках {line_numbers}")

if discovered_path:
    if not discovered_path.is_file():
        fail([f"файл обнаруженных тестов не найден: {discovered_path}"])
    discovery_output = discovered_path.read_text(encoding="utf-8")
else:
    process = subprocess.run(
        ["go", "test", "-tags=integration", "-list", "^Test", "./operator/integration"],
        cwd=repo_root,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
    )
    if process.returncode:
        print(process.stdout, end="", file=sys.stderr)
        fail(["не удалось получить список тестов Go"])
    discovery_output = process.stdout

discovered = {
    line.strip()
    for line in discovery_output.splitlines()
    if re.fullmatch(r"Test[A-Za-z0-9_]+", line.strip())
}
if not discovered:
    errors.append("обнаружен пустой набор тестов")

assigned = set(test_rows)
for test_name in sorted(discovered - assigned):
    errors.append(f"тест не назначен: {test_name}")
for test_name in sorted(assigned - discovered):
    errors.append(f"неизвестный тест в каталоге: {test_name}")
for matrix_id in sorted(required_matrix - matrix_ids):
    errors.append(f"обязательная проверка не представлена в каталоге: {matrix_id}")

if errors:
    fail(errors)

print(f"каталог тестов: {len(rows)} сценариев, назначения полные")
PY
