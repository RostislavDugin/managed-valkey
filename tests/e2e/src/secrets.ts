import { appendFileSync, chmodSync, mkdirSync, openSync, closeSync } from 'node:fs';
import { dirname } from 'node:path';

export function recordSecret(value: string) {
  if (!value) {
    throw new Error('Пустое значение нельзя зарегистрировать как контрольный секрет');
  }

  const path = process.env.MANAGED_VALKEY_E2E_SECRET_VALUES_FILE;
  if (!path) {
    throw new Error('MANAGED_VALKEY_E2E_SECRET_VALUES_FILE не задан');
  }

  mkdirSync(dirname(path), { recursive: true, mode: 0o700 });
  const descriptor = openSync(path, 'a', 0o600);
  closeSync(descriptor);
  chmodSync(path, 0o600);
  appendFileSync(path, `${value}\n`, { encoding: 'utf8', mode: 0o600 });
}
