import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, rmSync, statSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { recordSecret } from '../src/secrets.ts';

test('реестр контрольных секретов создаёт закрытый файл и сохраняет каждое значение', () => {
  const directory = mkdtempSync(join(tmpdir(), 'managed-valkey-e2e-secrets-'));
  const path = join(directory, 'values');
  const previous = process.env.MANAGED_VALKEY_E2E_SECRET_VALUES_FILE;
  process.env.MANAGED_VALKEY_E2E_SECRET_VALUES_FILE = path;
  try {
    recordSecret('account-secret');
    recordSecret('valkey-secret');

    assert.equal(statSync(path).mode & 0o777, 0o600);
    assert.equal(readFileSync(path, 'utf8'), 'account-secret\nvalkey-secret\n');
  } finally {
    if (previous === undefined) {
      delete process.env.MANAGED_VALKEY_E2E_SECRET_VALUES_FILE;
    } else {
      process.env.MANAGED_VALKEY_E2E_SECRET_VALUES_FILE = previous;
    }
    rmSync(directory, { recursive: true, force: true });
  }
});
