export const VALKEY_PASSWORD_BYTE_LENGTH = 24;
export const VALKEY_PASSWORD_LENGTH = 32;
export const VALKEY_PASSWORD_PATTERN = /^[A-Za-z0-9_-]{32}$/;

export interface ValkeyCredentials {
  username: 'app';
  passwordHint: string;
  passwordVersion: number;
  appliedPasswordVersion: number;
}

export interface EphemeralValkeyPassword {
  instanceId: string;
  password: string;
  source: 'creation' | 'rotation';
}

function encodeBase64Url(bytes: Uint8Array) {
  let binary = '';

  for (const byte of bytes) {
    binary += String.fromCharCode(byte);
  }

  return btoa(binary).replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/, '');
}

export function generateValkeyPassword() {
  const bytes = crypto.getRandomValues(new Uint8Array(VALKEY_PASSWORD_BYTE_LENGTH));
  return encodeBase64Url(bytes);
}

export function validateValkeyPassword(password: string) {
  return VALKEY_PASSWORD_PATTERN.test(password) ? null : 'Пароль имеет неверный формат';
}

export function getValkeyPasswordHint(password: string) {
  return `${password.slice(0, 4)}*****`;
}
