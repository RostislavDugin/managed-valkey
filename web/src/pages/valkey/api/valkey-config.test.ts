import { afterEach, describe, expect, it, vi } from 'vitest';
import { getValkeyDomain } from './valkey-config';

afterEach(() => {
  vi.unstubAllEnvs();
});

describe('домен инстансов', () => {
  it('отдаёт значения прода, пока переменных окружения нет', async () => {
    await expect(getValkeyDomain()).resolves.toEqual({
      domain: 'valkey.h3llo-demo.com',
      port: 41379,
    });
  });

  it('берёт домен и порт из переменных окружения', async () => {
    vi.stubEnv('VITE_VALKEY_BASE_DOMAIN', 'valkey.localhost');
    vi.stubEnv('VITE_VALKEY_PUBLIC_PORT', '31379');

    await expect(getValkeyDomain()).resolves.toEqual({
      domain: 'valkey.localhost',
      port: 31379,
    });
  });

  it('игнорирует порт, который не разобрать', async () => {
    vi.stubEnv('VITE_VALKEY_PUBLIC_PORT', 'вечером');

    await expect(getValkeyDomain()).resolves.toMatchObject({ port: 41379 });
  });
});
