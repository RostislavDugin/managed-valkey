import { describe, expect, it } from 'vitest';
import { getConnectionExamples } from './ConnectionExamples';

describe('примеры подключения Valkey', () => {
  it('включает TLS для HTTPS-консоли', () => {
    const examples = getConnectionExamples(
      'cache.valkey.test',
      'cache-ro.valkey.test',
      41379,
      true
    );

    expect(examples.javascript).toContain('tls: true');
    expect(examples.python).toContain('ssl=True');
    expect(examples.go).toContain('TLSConfig: &tls.Config');
  });

  it('не включает TLS для локальной HTTP-консоли', () => {
    const examples = getConnectionExamples(
      'cache.valkey.localhost',
      'cache-ro.valkey.localhost',
      41379,
      false
    );

    expect(examples.javascript).not.toContain('tls: true');
    expect(examples.python).not.toContain('ssl=True');
    expect(examples.go).not.toContain('TLSConfig');
  });
});
