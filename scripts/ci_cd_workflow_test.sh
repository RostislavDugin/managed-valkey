#!/usr/bin/env bash
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
workflow="$repo_root/.github/workflows/ci-cd.yml"

python3 - "$workflow" <<'PY'
import pathlib
import re
import sys

workflow = pathlib.Path(sys.argv[1]).read_text()


def job(name: str, following: str) -> str:
    match = re.search(rf"(?ms)^  {re.escape(name)}:\n(.*?)(?=^  {re.escape(following)}:)", workflow)
    if match is None:
        raise SystemExit(f"job {name} не найден")
    return match.group(1)


integration = job("test-integrations", "test-e2e")
for required in (
    "needs: [lint-api, lint-operator, lint-web]",
    "timeout-minutes: 30",
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

for build_name, following in (("build-api", "build-web"), ("build-web", "deploy")):
    build = job(build_name, following)
    if "test-integrations" not in re.search(r"needs: \[(.*?)\]", build).group(1):
        raise SystemExit(f"{build_name} не зависит от test-integrations")

deploy = re.search(r"(?ms)^  deploy:\n(.*)$", workflow)
if deploy is None or "needs: [build-api, build-web]" not in deploy.group(1):
    raise SystemExit("deploy не зависит от обязательных build jobs")
PY
