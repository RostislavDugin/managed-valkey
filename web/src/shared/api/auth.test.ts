import { beforeEach, describe, expect, it } from 'vitest';
import { AUTH_TOKEN_KEY, AUTH_USERS_KEY, getSession, login, register } from './auth';
import { ApiError } from './client';

const UUID_V7_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

function readTokenPayload(token: string) {
  const normalized = token.split('.')[1].replaceAll('-', '+').replaceAll('_', '/');
  const padding = '='.repeat((4 - (normalized.length % 4)) % 4);
  return JSON.parse(atob(`${normalized}${padding}`)) as { sub: string; exp: number };
}

function createLegacyToken(sub: string, exp: number) {
  const encode = (value: object) =>
    btoa(JSON.stringify(value)).replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/, '');
  return `${encode({ alg: 'none', typ: 'JWT' })}.${encode({ sub, exp })}.demo`;
}

function readUsers() {
  return JSON.parse(localStorage.getItem(AUTH_USERS_KEY) ?? '[]') as Array<{
    id?: string;
    email: string;
    password: string;
  }>;
}

beforeEach(() => {
  localStorage.clear();
});

describe('регистрация и вход', () => {
  it('выдаёт аккаунту UUIDv7 и кладёт его в sub', async () => {
    const { token } = await register({ email: 'User@Example.com', password: 'password1' });

    const [user] = readUsers();
    expect(user.id).toMatch(UUID_V7_PATTERN);
    expect(user.email).toBe('user@example.com');
    expect(readTokenPayload(token).sub).toBe(user.id);
  });

  it('вход выдаёт токен с идентификатором того же аккаунта', async () => {
    await register({ email: 'user@example.com', password: 'password1' });
    const [user] = readUsers();

    const { token } = await login({ email: 'user@example.com', password: 'password1' });

    expect(readTokenPayload(token).sub).toBe(user.id);
  });
});

describe('восстановление сессии', () => {
  it('возвращает userId и почту после перезагрузки', async () => {
    await register({ email: 'user@example.com', password: 'password1' });
    const [user] = readUsers();

    const session = await getSession();

    expect(session?.userId).toBe(user.id);
    expect(session?.email).toBe('user@example.com');
  });

  it('без токена сессии нет', async () => {
    await expect(getSession()).resolves.toBeNull();
  });
});

describe('миграция данных браузера', () => {
  it('назначает UUIDv7 записи без идентификатора и сохраняет её', async () => {
    localStorage.setItem(
      AUTH_USERS_KEY,
      JSON.stringify([{ email: 'user@example.com', password: 'password1' }])
    );

    const { token } = await login({ email: 'user@example.com', password: 'password1' });

    const [user] = readUsers();
    expect(user.id).toMatch(UUID_V7_PATTERN);
    expect(user.email).toBe('user@example.com');
    expect(user.password).toBe('password1');
    expect(readTokenPayload(token).sub).toBe(user.id);
  });

  it('заменяет токен с почтой в sub токеном с user_id и прежним exp', async () => {
    const exp = Math.floor(Date.now() / 1000) + 3600;
    localStorage.setItem(
      AUTH_USERS_KEY,
      JSON.stringify([{ email: 'user@example.com', password: 'password1' }])
    );
    localStorage.setItem(AUTH_TOKEN_KEY, createLegacyToken('user@example.com', exp));

    const session = await getSession();

    const [user] = readUsers();
    expect(session?.userId).toBe(user.id);
    expect(session?.email).toBe('user@example.com');

    const stored = readTokenPayload(localStorage.getItem(AUTH_TOKEN_KEY) as string);
    expect(stored.sub).toBe(user.id);
    expect(stored.exp).toBe(exp);
  });

  it('удаляет токен, который нельзя связать с аккаунтом', async () => {
    const exp = Math.floor(Date.now() / 1000) + 3600;
    localStorage.setItem(AUTH_TOKEN_KEY, createLegacyToken('missing@example.com', exp));

    await expect(getSession()).rejects.toBeInstanceOf(ApiError);
    expect(localStorage.getItem(AUTH_TOKEN_KEY)).toBeNull();
  });
});
