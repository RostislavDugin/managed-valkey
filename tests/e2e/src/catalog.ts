import { spawnSync } from 'node:child_process';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const firstScenarioTitle = 'пользователь проходит полный жизненный цикл single-инстанса';

export const requiredScenarios = [
  { title: firstScenarioTitle, tag: 'prod' },
  {
    title: 'новый пользователь регистрируется через форму и попадает в авторизованную консоль',
    tag: 'prod',
  },
  { title: 'пользователь проходит полный жизненный цикл HA-инстанса', tag: 'prod' },
  {
    title: 'личная квота запрещает недопустимое создание и изменение размера',
    tag: 'local',
  },
  {
    title: 'общая ёмкость кластера отклоняет новый инстанс без влияния на работающие',
    tag: 'local',
  },
  { title: 'HA-инстанс восстанавливается после потери primary Pod', tag: 'local-k8s' },
  {
    title: 'HA-инстанс восстанавливается после потери worker-ноды через прежний адрес',
    tag: 'local-k8s',
  },
] as const;

export const scenarioGroups = {
  lifecycle: 'prod',
  quotas: 'local',
  failures: 'local-k8s',
} as const;

export type ScenarioGroup = 'all' | keyof typeof scenarioGroups;

export interface CatalogEntry {
  title: string;
  tags: string[];
}

function parseScenarioGroup(value: string): ScenarioGroup {
  if (value === 'all' || value in scenarioGroups) {
    return value as ScenarioGroup;
  }
  throw new Error(`Неизвестная группа e2e: ${value}`);
}

export function validateScenarioGroup(entries: readonly CatalogEntry[], value: string) {
  const group = parseScenarioGroup(value);
  if (group === 'all') {
    return;
  }
  const tag = scenarioGroups[group];
  if (!entries.some((entry) => entry.tags.includes(tag))) {
    throw new Error(`Группа e2e ${group} не содержит сценариев с тегом @${tag}`);
  }
}

interface JsonSuite {
  suites?: JsonSuite[];
  specs?: CatalogEntry[];
}

function collectEntries(suites: readonly JsonSuite[], target: CatalogEntry[]) {
  for (const suite of suites) {
    target.push(...(suite.specs ?? []));
    collectEntries(suite.suites ?? [], target);
  }
}

export function validateCatalog(entries: readonly CatalogEntry[], value = 'all') {
  const group = parseScenarioGroup(value);
  if (entries.length === 0) {
    throw new Error('Каталог Playwright пуст');
  }

  const groupTags = new Set<string>(Object.values(scenarioGroups));
  for (const required of requiredScenarios) {
    const matches = entries.filter((entry) => entry.title === required.title);
    if (matches.length !== 1) {
      throw new Error(`Сценарий «${required.title}» должен встречаться в каталоге ровно один раз`);
    }
    if (!matches[0].tags.includes(required.tag)) {
      throw new Error(`Сценарий «${required.title}» должен иметь тег @${required.tag}`);
    }
    const matchedGroupTags = matches[0].tags.filter((tag) => groupTags.has(tag));
    if (matchedGroupTags.length !== 1) {
      throw new Error(`Сценарий «${required.title}» должен входить ровно в одну группу e2e`);
    }
  }
  if (entries[0].title !== firstScenarioTitle) {
    throw new Error(`Первым сценарием e2e должен быть «${firstScenarioTitle}»`);
  }
  validateScenarioGroup(entries, group);
}

export function parseCatalogReport(value: string) {
  const report = JSON.parse(value) as { suites?: JsonSuite[] };
  const entries: CatalogEntry[] = [];
  collectEntries(report.suites ?? [], entries);
  return entries;
}

export function checkCatalog(value = 'all') {
  const e2eRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..');
  const result = spawnSync('pnpm', ['exec', 'playwright', 'test', '--list', '--reporter=json'], {
    cwd: e2eRoot,
    encoding: 'utf8',
  });
  if (result.status !== 0) {
    throw new Error(result.stderr.trim() || 'Playwright не смог построить каталог сценариев');
  }
  validateCatalog(parseCatalogReport(result.stdout), value);
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  checkCatalog(process.argv[2]);
}
