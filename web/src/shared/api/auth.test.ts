import { beforeEach, describe, expect, it, vi } from 'vitest';
import { AUTH_TOKEN_KEY, checkEmail, getSession, login, register } from './auth';
import { ApiError } from './client';

const USER_ID = '01992b7e-7818-7000-8000-000000000001';

function createToken(exp = Math.floor(Date.now() / 1000) + 3600) {
  const encode = (value: object) =>
    btoa(JSON.stringify(value)).replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/, '');
  return `${encode({ alg: 'HS256', typ: 'JWT' })}.${encode({ sub: USER_ID, exp })}.signature`;
}

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

beforeEach(() => {
  localStorage.clear();
});

describe('регистрация и вход', () => {
  it('проверяет почту через API', async () => {
    const fetchMock = vi.spyOn(window, 'fetch').mockResolvedValue(jsonResponse({ exists: true }));

    await expect(checkEmail({ email: 'User@Example.com' })).resolves.toEqual({ exists: true });

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [url, init] = fetchMock.mock.calls[0];
    expect(url).toBe('/v1/auth/check-email');
    expect(init?.method).toBe('POST');
    expect(init?.body).toBe(JSON.stringify({ email: 'User@Example.com' }));
  });

  it('повторяет регистрацию с тем же Idempotency-Key и сохраняет серверный JWT', async () => {
    const token = createToken();
    const fetchMock = vi
      .spyOn(window, 'fetch')
      .mockResolvedValueOnce(jsonResponse({ error: { code: 'UNAVAILABLE' } }, 503))
      .mockResolvedValueOnce(jsonResponse({ token }));
    vi.spyOn(Math, 'random').mockReturnValue(0);

    await expect(register({ email: 'user@example.com', password: 'password1' })).resolves.toEqual({
      token,
    });

    expect(fetchMock).toHaveBeenCalledTimes(2);
    const first = fetchMock.mock.calls[0][1];
    const second = fetchMock.mock.calls[1][1];
    const firstHeaders = new Headers(first?.headers);
    const secondHeaders = new Headers(second?.headers);
    expect(firstHeaders.get('Idempotency-Key')).toMatch(/^[0-9a-f-]{36}$/);
    expect(secondHeaders.get('Idempotency-Key')).toBe(firstHeaders.get('Idempotency-Key'));
    expect(secondHeaders.get('X-Request-Id')).toBe(firstHeaders.get('X-Request-Id'));
    expect(second?.body).toBe(first?.body);
    expect(localStorage.getItem(AUTH_TOKEN_KEY)).toBe(token);
  });

  it('передаёт CONFLICT и UNAUTHORIZED вызывающему коду', async () => {
    vi.spyOn(window, 'fetch')
      .mockResolvedValueOnce(
        jsonResponse({ error: { code: 'CONFLICT', message: 'Аккаунт уже существует' } }, 409)
      )
      .mockResolvedValueOnce(
        jsonResponse({ error: { code: 'UNAUTHORIZED', message: 'Неверные данные' } }, 401)
      );

    await expect(
      register({ email: 'user@example.com', password: 'password1' })
    ).rejects.toMatchObject({
      code: 'CONFLICT',
    });
    await expect(
      login({ email: 'user@example.com', password: 'wrong-password' })
    ).rejects.toMatchObject({
      code: 'UNAUTHORIZED',
    });
  });
});

describe('восстановление сессии', () => {
  it('проверяет действующий токен через /me', async () => {
    const token = createToken();
    localStorage.setItem(AUTH_TOKEN_KEY, token);
    const fetchMock = vi.spyOn(window, 'fetch').mockResolvedValue(
      jsonResponse({
        user: { id: USER_ID, email: 'user@example.com' },
        quota: { max_vcpu: 4, max_ram_gb: 16 },
        usage: { used_vcpu: 0, used_ram_gb: 0 },
      })
    );

    await expect(getSession()).resolves.toEqual({
      userId: USER_ID,
      email: 'user@example.com',
      expiresAt: expect.any(Number),
      token,
    });
    expect(fetchMock).toHaveBeenCalledWith('/v1/me', expect.any(Object));
  });

  it('удаляет истёкший токен без запроса', async () => {
    localStorage.setItem(AUTH_TOKEN_KEY, createToken(Math.floor(Date.now() / 1000) - 1));
    const fetchMock = vi.spyOn(window, 'fetch');

    await expect(getSession()).rejects.toMatchObject({ code: 'UNAUTHORIZED' });
    expect(fetchMock).not.toHaveBeenCalled();
    expect(localStorage.getItem(AUTH_TOKEN_KEY)).toBeNull();
  });

  it('удаляет токен после блокировки и передаёт временную ошибку после повторов', async () => {
    localStorage.setItem(AUTH_TOKEN_KEY, createToken());
    vi.spyOn(window, 'fetch').mockResolvedValueOnce(
      jsonResponse({ error: { code: 'UNAUTHORIZED', message: 'Сессия недействительна' } }, 401)
    );

    await expect(getSession()).rejects.toBeInstanceOf(ApiError);
    expect(localStorage.getItem(AUTH_TOKEN_KEY)).toBeNull();

    localStorage.setItem(AUTH_TOKEN_KEY, createToken());
    vi.spyOn(Math, 'random').mockReturnValue(0);
    vi.spyOn(window, 'fetch').mockResolvedValue(
      jsonResponse({ error: { code: 'UNAVAILABLE', message: 'Недоступно' } }, 503)
    );
    await expect(getSession()).rejects.toMatchObject({ code: 'UNAVAILABLE' });
  });

  it('без токена сессии нет', async () => {
    await expect(getSession()).resolves.toBeNull();
  });
});
