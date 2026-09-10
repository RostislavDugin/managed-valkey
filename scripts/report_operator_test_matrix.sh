#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
results_dir=${1:?задайте каталог результатов тестов}
mode=${2:-full}
catalog=${3:-$repo_root/scripts/operator_test_scenarios.tsv}

python3 - "$results_dir" "$mode" "$catalog" <<'PY'
import csv
import pathlib
import re
import sys

results_dir = pathlib.Path(sys.argv[1])
mode = sys.argv[2]
catalog_path = pathlib.Path(sys.argv[3])
matrix_sources = {}
for prefix, last in (("HA", 8), ("FP", 12), ("ND", 9), ("NT", 6), ("OP", 8), ("RZ", 11), ("PW", 10), ("DL", 4)):
    matrix_sources.update({f"{prefix}-{number:02d}": "k3s" for number in range(1, last + 1)})
matrix_sources.update({f"CT-{number:02d}": "unit+envtest" for number in (1, 2, 6, 7)})
matrix_sources.update({f"CT-{number:02d}": "envtest" for number in (3, 4, 5, 8)})
matrix_sources.update({f"CT-{number:02d}": "k3s" for number in (9, 10, 11)})
matrix_sources.update({"CT-12": "network", "CT-13": "environment", "CT-14": "environment+k3s"})


def ids_from_go_log(path):
    if not path.is_file():
        return set()
    result = set()
    for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
        match = re.match(r"\s*--- PASS: (Test\S+)", line)
        if not match:
            continue
        result.update(
            f"{prefix}-{number}"
            for prefix, number in re.findall(r"(HA|FP|ND|NT|OP|RZ|PW|DL|CT)-?([0-9]{2})", match.group(1))
        )
    return result


def passed_tests_from_go_log(path):
    result = set()
    for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
        match = re.match(r"\s*--- PASS: (Test[A-Za-z0-9_]+)(?:\s|$)", line)
        if match:
            result.add(match.group(1))
    return result


def ids_from_marker_log(path):
    if not path.is_file():
        return set()
    result = set()
    for line in path.read_text(encoding="utf-8", errors="replace").splitlines():
        if re.match(r"^CT-[0-9]{2}(?:\s+CT-[0-9]{2})*:", line):
            result.update(re.findall(r"CT-[0-9]{2}", line))
    return result


def log_has_failure(path):
    if not path.is_file():
        return True
    text = path.read_text(encoding="utf-8", errors="replace")
    return bool(re.search(r"^\s*--- (?:FAIL|SKIP):", text, re.MULTILINE))


def go_log_status(path):
    text = path.read_text(encoding="utf-8", errors="replace")
    if re.search(r"^\s*--- FAIL:", text, re.MULTILINE):
        return "fail"
    if re.search(r"^\s*--- SKIP:", text, re.MULTILINE):
        return "skip"
    return "pass"


if mode not in {"full", "ci"}:
    print(f"неизвестный режим отчёта: {mode}", file=sys.stderr)
    raise SystemExit(1)
if not results_dir.is_dir():
    print(f"каталог результатов тестов не найден: {results_dir}", file=sys.stderr)
    raise SystemExit(1)

print(f"OPERATOR TEST MODE {mode}")

unit_log = results_dir / "unit.log"
envtest_log = results_dir / "envtest.log"
failed = 0
for name, path in (("unit", unit_log), ("envtest", envtest_log)):
    status_path = results_dir / f"{name}.status"
    if not path.is_file() or not status_path.is_file():
        print(f"RESULT {name} MISSING", file=sys.stderr)
        failed += 1
    elif path.stat().st_size == 0:
        print(f"RESULT {name} INVALID", file=sys.stderr)
        failed += 1
    elif (status := status_path.read_text(encoding="utf-8").strip()) not in {"pass", "fail"}:
        print(f"RESULT {name} INVALID status={status or 'empty'}", file=sys.stderr)
        failed += 1
    elif status != "pass":
        print(f"RESULT {name} FAIL status={status}", file=sys.stderr)
        failed += 1
    elif log_has_failure(path):
        print(f"RESULT {name} FAIL", file=sys.stderr)
        failed += 1
    else:
        print(f"RESULT {name} PASS")

durations_path = results_dir / "durations.tsv"
if durations_path.is_file():
    stage_durations = []
    for line in durations_path.read_text(encoding="utf-8").splitlines():
        fields = line.split("\t")
        if len(fields) != 3:
            continue
        try:
            duration = float(fields[2])
        except ValueError:
            continue
        stage_durations.append((duration, fields[0]))
    if stage_durations:
        print("Длительности этапов:")
        for duration, name in sorted(stage_durations, reverse=True):
            print(f"{name}\t{duration:g}s")

if mode == "ci":
    print("MATRIX SCOPE unit/envtest only")
    raise SystemExit(failed != 0)

if not catalog_path.is_file():
    print(f"каталог сценариев не найден: {catalog_path}", file=sys.stderr)
    raise SystemExit(1)

with catalog_path.open(newline="", encoding="utf-8") as source:
    catalog = list(csv.DictReader(source, delimiter="\t"))

expected = {row["scenario"] for row in catalog}
scenario_root = results_dir / "scenarios"
actual = {path.name for path in scenario_root.iterdir() if path.is_dir()} if scenario_root.is_dir() else set()
for scenario in sorted(actual - expected):
    print(f"RESULT {scenario} UNKNOWN", file=sys.stderr)
    failed += 1

k3s_ids = set()
scenario_marker_ids = set()
durations = []
for row in catalog:
    scenario = row["scenario"]
    expected_test = row["test_pattern"][1:-1]
    attempts_root = scenario_root / scenario / "attempts"
    attempts = sorted(
        (path for path in attempts_root.iterdir() if path.is_dir() and path.name.isdigit()),
        key=lambda path: int(path.name),
    ) if attempts_root.is_dir() else []
    if not attempts:
        print(f"RESULT {scenario} MISSING", file=sys.stderr)
        failed += 1
        continue

    statuses = []
    logs = []
    scenario_duration = 0.0
    incomplete = False
    for attempt in attempts:
        status_path = attempt / "status"
        log_path = attempt / "log"
        duration_path = attempt / "duration"
        if not status_path.is_file() or not log_path.is_file() or not duration_path.is_file():
            incomplete = True
            continue
        status = status_path.read_text(encoding="utf-8").strip()
        log_status = go_log_status(log_path)
        if status == "pass" and log_status != "pass":
            status = log_status
        statuses.append(status)
        logs.append(log_path)
        try:
            duration = float(duration_path.read_text(encoding="utf-8").strip())
            if duration < 0:
                raise ValueError
            scenario_duration += duration
        except ValueError:
            incomplete = True

    durations.append((scenario_duration, scenario))
    if incomplete or len(statuses) != len(attempts):
        print(f"RESULT {scenario} INVALID duration={scenario_duration:g}s", file=sys.stderr)
        failed += 1
        continue
    if any(status == "fail" for status in statuses):
        detail = " original-failure-preserved" if statuses[-1] == "pass" else ""
        print(f"RESULT {scenario} FAIL duration={scenario_duration:g}s{detail}", file=sys.stderr)
        failed += 1
        continue
    if any(status == "skip" for status in statuses):
        print(f"RESULT {scenario} SKIP duration={scenario_duration:g}s", file=sys.stderr)
        failed += 1
        continue
    if any(status != "pass" for status in statuses):
        print(f"RESULT {scenario} INVALID duration={scenario_duration:g}s", file=sys.stderr)
        failed += 1
        continue
    passed_tests = set()
    for log_path in logs:
        passed_tests.update(passed_tests_from_go_log(log_path))
    if expected_test not in passed_tests:
        print(f"RESULT {scenario} INVALID: нет PASS для {expected_test}", file=sys.stderr)
        failed += 1
        continue

    print(f"RESULT {scenario} PASS duration={scenario_duration:g}s")
    for log_path in logs:
        k3s_ids.update(ids_from_go_log(log_path))
        scenario_marker_ids.update(ids_from_marker_log(log_path))

seen = {
    "unit": ids_from_go_log(unit_log),
    "envtest": ids_from_go_log(envtest_log),
    "k3s": k3s_ids,
    "network": set(),
}
network_log = results_dir / "network-faults.log"
network_status_path = results_dir / "network-faults.status"
if not network_status_path.is_file() or not network_log.is_file():
    print("RESULT network-faults MISSING", file=sys.stderr)
    failed += 1
else:
    network_status = network_status_path.read_text(encoding="utf-8").strip()
    network_ids = ids_from_marker_log(network_log)
    if network_status != "pass":
        print(f"RESULT network-faults FAIL status={network_status or 'empty'}", file=sys.stderr)
        failed += 1
    elif "CT-12" not in network_ids:
        print("RESULT network-faults INVALID: нет маркера CT-12", file=sys.stderr)
        failed += 1
    else:
        print("RESULT network-faults PASS")
        seen["network"].add("CT-12")
seen["environment"] = set()
for path in results_dir.glob("environment-*.log"):
    seen["environment"].update(ids_from_marker_log(path))
seen["environment"].update(matrix_id for matrix_id in scenario_marker_ids if matrix_id in {"CT-13", "CT-14"})

passed_matrix = 0
for matrix_id, source in matrix_sources.items():
    sources = source.split("+")
    if all(matrix_id in seen[item] for item in sources):
        print(f"MATRIX {matrix_id:<5} PASS ({source})")
        passed_matrix += 1
    else:
        print(f"MATRIX {matrix_id:<5} FAIL: нет успешного результата в {source}", file=sys.stderr)
        failed += 1

print(f"MATRIX TOTAL {passed_matrix}/{len(matrix_sources)} PASS")
print("Самые долгие сценарии:")
for duration, scenario in sorted(durations, reverse=True)[:15]:
    print(f"{scenario}\t{duration:g}s")

raise SystemExit(failed != 0)
PY
