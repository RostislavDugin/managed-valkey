import { describe, expect, it, vi } from 'vitest';
import {
  generateValkeyPassword,
  getValkeyPasswordHint,
  VALKEY_PASSWORD_BYTE_LENGTH,
  VALKEY_PASSWORD_LENGTH,
  VALKEY_PASSWORD_PATTERN,
  validateValkeyPassword,
} from './valkey-credentials';

describe('учётные данные Valkey', () => {
  it('кодирует 24 случайных байта в 32 символа base64url', () => {
    const getRandomValues = vi.spyOn(crypto, 'getRandomValues').mockImplementation((value) => {
      const bytes = value as Uint8Array;
      bytes.fill(255);
      return value;
    });

    const password = generateValkeyPassword();

    expect(getRandomValues).toHaveBeenCalledOnce();
    expect((getRandomValues.mock.calls[0][0] as Uint8Array).byteLength).toBe(
      VALKEY_PASSWORD_BYTE_LENGTH
    );
    expect(password).toBe('_'.repeat(VALKEY_PASSWORD_LENGTH));
    expect(password).toMatch(VALKEY_PASSWORD_PATTERN);
  });

  it('проверяет формат и строит маску из первых четырёх символов', () => {
    const password = 'abcdEFGHijklMNOPqrstUVWXyz01_234';

    expect(validateValkeyPassword(password)).toBeNull();
    expect(validateValkeyPassword(`${password}x`)).toBe('Пароль имеет неверный формат');
    expect(getValkeyPasswordHint(password)).toBe('abcd*****');
  });
});
