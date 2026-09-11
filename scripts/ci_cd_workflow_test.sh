#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
workflow="$repo_root/.github/workflows/ci-cd.yml"

python3 - "$workflow" <<'PY'
import pathlib
import re
import sys

workflow = pathlib.Path(sys.argv[1]).read_text()
jobs_source = workflow.split("\njobs:\n", 1)[1]


def jobs() -> dict[str, str]:
    matches = list(re.finditer(r"(?m)^  ([a-z0-9-]+):\n", jobs_source))
    result = {}
    for index, match in enumerate(matches):
        end = matches[index + 1].start() if index + 1 < len(matches) else len(jobs_source)
        result[match.group(1)] = jobs_source[match.end() : end]
    return result


job = jobs()
independent = {
    "lint-api",
    "lint-operator",
    "lint-web",
    "test-api",
    "test-operator",
    "test-web",
    "test-integrations",
    "test-e2e",
    "build-api",
    "build-web",
}
if not independent.issubset(job):
    missing = ", ".join(sorted(independent - job.keys()))
    raise SystemExit(f"не найдены независимые jobs: {missing}")
for name in sorted(independent):
    if re.search(r"(?m)^    needs:", job[name]):
        raise SystemExit(f"{name} не должен ждать другое задание")

for name in ("test-api", "test-integrations", "test-e2e"):
    if "sudo apt-get update && sudo apt-get install --yes ripgrep" not in job[name]:
        raise SystemExit(f"{name} не устанавливает обязательный rg")


integration = job["test-integrations"]
for required in (
    "timeout-minutes: 30",
    'MV_TEST_MEMORY_MIB: "2048"',
    'MV_TEST_MIN_AVAILABLE_MIB: "1024"',
    "run: just test-integrations",
    "scripts/check_test_artifacts.sh",
    "tmp/k3s/integration/*/diagnostics/**",
):
    if required not in integration:
        raise SystemExit(f"в test-integrations отсутствует {required}")
if "integration-secret-values" in re.search(
    r"(?ms)- name: Сохранить диагностику\n(.*)", integration
).group(1):
    raise SystemExit("файл контрольных секретов попал в публикуемый артефакт")

e2e = job["test-e2e"]
for required in (
    "name: Полный сквозной набор Playwright",
    "timeout-minutes: 60",
    'MV_TEST_MEMORY_MIB: "3072"',
    'MV_TEST_MIN_AVAILABLE_MIB: "1024"',
    'VALKEY_INTEGRATION_REQUEST_CPU: "100m"',
    'VALKEY_INTEGRATION_REQUEST_MEMORY: "128Mi"',
    "fail-fast: false",
    "group: [lifecycle, quotas, failures]",
    "name: Освободить место для трёхнодового стенда",
    "/usr/local/lib/android",
    "/usr/share/dotnet",
    "/opt/ghc",
    "/opt/az",
    'scripts/check_e2e_catalog.sh "${{ matrix.group }}"',
    'run: just test-e2e-group "${{ matrix.group }}"',
    "e2e-test-diagnostics-${{ matrix.group }}-${{ github.sha }}",
    "e2e-secret-values",
    "scripts/check_e2e_artifacts.sh",
    "tmp/playwright/e2e/**",
    "tmp/k3s/e2e/*/diagnostics/**",
):
    if required not in e2e:
        raise SystemExit(f"в test-e2e отсутствует {required}")
published_e2e = re.search(r"(?ms)- name: Сохранить диагностику\n(.*)", e2e)
if published_e2e is None:
    raise SystemExit("в test-e2e отсутствует публикация диагностики")
if "e2e-secret-values" in published_e2e.group(1):
    raise SystemExit("файл контрольных секретов e2e попал в публикуемый артефакт")

barrier_name = "ci-success"
if barrier_name not in job:
    raise SystemExit(f"job {barrier_name} не найден")
barrier = job[barrier_name]
if not re.search(r"(?m)^    if: always\(\)$", barrier):
    raise SystemExit(f"{barrier_name} должен запускаться через always()")
needs_match = re.search(r"(?ms)^    needs:\n((?:      - [a-z0-9-]+\n)+)", barrier)
if needs_match is None:
    raise SystemExit(f"у {barrier_name} отсутствует список needs")
barrier_needs = set(re.findall(r"(?m)^      - ([a-z0-9-]+)$", needs_match.group(1)))
if barrier_needs != independent:
    missing = ", ".join(sorted(independent - barrier_needs))
    extra = ", ".join(sorted(barrier_needs - independent))
    raise SystemExit(f"неполный fan-in: отсутствуют [{missing}], лишние [{extra}]")
for name in sorted(independent):
    env_name = name.replace("-", "_").upper() + "_RESULT"
    mapping = f"{env_name}: ${{{{ needs.{name}.result }}}}"
    if mapping not in barrier:
        raise SystemExit(f"{barrier_name} не проверяет результат {name}")
    if f'"{name}=${env_name}"' not in barrier:
        raise SystemExit(f"{barrier_name} не включает {name} в итоговую проверку")
if "if [[ ${result#*=} != success ]]" not in barrier or 'exit "$failed"' not in barrier:
    raise SystemExit(f"{barrier_name} не отклоняет failure, cancelled и skipped")

deploy = job.get("deploy")
if deploy is None or "needs: [ci-success]" not in deploy:
    raise SystemExit("deploy не зависит от итоговой проверки CI")
if "github.event_name == 'push' && github.ref == 'refs/heads/main'" not in deploy:
    raise SystemExit("deploy должен запускаться только после push в main")
PY
