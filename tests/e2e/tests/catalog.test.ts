import assert from 'node:assert/strict';
import test from 'node:test';
import {
  requiredScenarios,
  validateCatalog,
  validateScenarioGroup,
} from '../src/catalog.ts';

function completeCatalog() {
  return requiredScenarios.map((scenario) => ({
    title: scenario.title,
    tags: [scenario.tag],
  }));
}

test('проверка каталога принимает семь обязательных title и tag', () => {
  assert.doesNotThrow(() => validateCatalog(completeCatalog()));
});

test('проверка каталога отклоняет пустой список', () => {
  assert.throws(() => validateCatalog([]), /Каталог Playwright пуст/);
});

test('проверка каталога отклоняет удалённый сценарий', () => {
  assert.throws(() => validateCatalog(completeCatalog().slice(1)), /должен встречаться/);
});

test('проверка каталога отклоняет неверный tag', () => {
  const entries = completeCatalog();
  entries[0] = { ...entries[0], tags: ['local'] };
  assert.throws(() => validateCatalog(entries), /должен иметь тег @prod/);
});

test('проверка каталога принимает три непересекающиеся группы', () => {
  const entries = completeCatalog();
  for (const group of ['lifecycle', 'quotas', 'failures']) {
    assert.doesNotThrow(() => validateCatalog(entries, group));
  }
});

test('проверка каталога отклоняет пересечение групп', () => {
  const entries = completeCatalog();
  entries[0] = { ...entries[0], tags: ['prod', 'local'] };
  assert.throws(() => validateCatalog(entries), /должен входить ровно в одну группу e2e/);
});

test('проверка группы отклоняет пустой набор', () => {
  const entries = completeCatalog().filter((entry) => !entry.tags.includes('local'));
  assert.throws(
    () => validateScenarioGroup(entries, 'quotas'),
    /Группа e2e quotas не содержит сценариев/
  );
});

test('проверка каталога отклоняет неизвестную группу', () => {
  assert.throws(() => validateCatalog(completeCatalog(), 'unknown'), /Неизвестная группа e2e/);
});
