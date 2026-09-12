import { describe, expect, it } from 'vitest';
import { formatValkeyAddress, getValkeyConnectionScheme } from './valkey-connection';

describe('адрес подключения Valkey', () => {
  it('использует TLS-схему для HTTPS-консоли', () => {
    expect(getValkeyConnectionScheme('https:')).toBe('rediss');
    expect(formatValkeyAddress('cache.valkey.test', 41379, 'rediss')).toBe(
      'rediss://cache.valkey.test:41379'
    );
  });

  it('оставляет обычную схему для локальной HTTP-консоли', () => {
    expect(getValkeyConnectionScheme('http:')).toBe('redis');
    expect(formatValkeyAddress('cache.valkey.test', undefined, 'redis')).toBe(
      'redis://cache.valkey.test'
    );
  });
});
