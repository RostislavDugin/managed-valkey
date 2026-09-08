import { createUuidV7, isUuid } from '@/shared/lib';
import { ApiError } from './client';

export const AUTH_TOKEN_KEY = 'mv_token';
export const AUTH_USERS_KEY = 'mv_users';

const RESPONSE_DELAY_MS = 400;
const TOKEN_TTL_SECONDS = 7 * 24 * 60 * 60;
const EMAIL_PATTERN = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

interface StoredUser {
  id: string;
  email: string;
  password: string;
}

interface TokenPayload {
  sub: string;
  exp: number;
}

export interface Session {
  userId: string;
  email: string;
  expiresAt: number;
  token: string;
}

export interface AuthResult {
  token: string;
}

const delay = () => new Promise((resolve) => setTimeout(resolve, RESPONSE_DELAY_MS));

function storage() {
  if (typeof localStorage === 'undefined') {
    throw new Error('Хранилище браузера недоступно');
  }
  return localStorage;
}

function normalizeEmail(email: string) {
  return email.trim().toLowerCase();
}

function assertCredentials(email: string, password?: string) {
  if (!EMAIL_PATTERN.test(email) || (password !== undefined && password.length < 8)) {
    throw new ApiError('VALIDATION_FAILED', 'Проверьте введённые данные');
  }
}

/**
 * Запись без `id` получает UUIDv7 и сразу сохраняется: иначе идентификатор
 * менялся бы при каждом чтении и владение ресурсами терялось бы.
 */
function readUsers(): StoredUser[] {
  const value = storage().getItem(AUTH_USERS_KEY);
  if (!value) {
    return [];
  }

  let parsed: unknown;
  try {
    parsed = JSON.parse(value);
  } catch {
    return [];
  }

  if (!Array.isArray(parsed)) {
    return [];
  }

  let migrated = false;
  const users = parsed.map((entry) => {
    const user = entry as Partial<StoredUser>;
    if (typeof user.id === 'string' && user.id) {
      return user as StoredUser;
    }

    migrated = true;
    return { ...user, id: createUuidV7() } as StoredUser;
  });

  if (migrated) {
    writeUsers(users);
  }

  return users;
}

function writeUsers(users: StoredUser[]) {
  storage().setItem(AUTH_USERS_KEY, JSON.stringify(users));
}

function encodeBase64Url(value: object) {
  const bytes = new TextEncoder().encode(JSON.stringify(value));
  let binary = '';
  for (const byte of bytes) {
    binary += String.fromCharCode(byte);
  }
  return btoa(binary).replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/, '');
}

function decodeBase64Url(value: string) {
  const normalized = value.replaceAll('-', '+').replaceAll('_', '/');
  const padding = '='.repeat((4 - (normalized.length % 4)) % 4);
  const binary = atob(`${normalized}${padding}`);
  const bytes = Uint8Array.from(binary, (character) => character.charCodeAt(0));
  return JSON.parse(new TextDecoder().decode(bytes)) as unknown;
}

function createToken(
  userId: string,
  expiresAt = Math.floor(Date.now() / 1000) + TOKEN_TTL_SECONDS
) {
  return `${encodeBase64Url({ alg: 'none', typ: 'JWT' })}.${encodeBase64Url({
    sub: userId,
    exp: expiresAt,
  })}.demo`;
}

function readTokenPayload(token: string): TokenPayload {
  const parts = token.split('.');
  if (parts.length !== 3) {
    throw new ApiError('UNAUTHORIZED', 'Сессия недействительна');
  }

  try {
    const payload = decodeBase64Url(parts[1]);
    if (
      !payload ||
      typeof payload !== 'object' ||
      !('sub' in payload) ||
      typeof payload.sub !== 'string' ||
      !('exp' in payload) ||
      typeof payload.exp !== 'number'
    ) {
      throw new Error('Invalid payload');
    }
    return { sub: payload.sub, exp: payload.exp };
  } catch {
    throw new ApiError('UNAUTHORIZED', 'Сессия недействительна');
  }
}

function saveToken(userId: string) {
  const token = createToken(userId);
  storage().setItem(AUTH_TOKEN_KEY, token);
  return token;
}

export async function checkEmail(input: { email: string }): Promise<{ exists: boolean }> {
  await delay();
  const email = normalizeEmail(input.email);
  assertCredentials(email);
  return { exists: readUsers().some((user) => user.email === email) };
}

export async function register(input: { email: string; password: string }): Promise<AuthResult> {
  await delay();
  const email = normalizeEmail(input.email);
  assertCredentials(email, input.password);
  const users = readUsers();

  if (users.some((user) => user.email === email)) {
    throw new ApiError('CONFLICT', 'Аккаунт с такой почтой уже существует');
  }

  const user: StoredUser = { id: createUuidV7(), email, password: input.password };
  writeUsers([...users, user]);
  return { token: saveToken(user.id) };
}

export async function login(input: { email: string; password: string }): Promise<AuthResult> {
  await delay();
  const email = normalizeEmail(input.email);
  assertCredentials(email, input.password);
  const user = readUsers().find((candidate) => candidate.email === email);

  if (!user || user.password !== input.password) {
    throw new ApiError('UNAUTHORIZED', 'Неверный пароль');
  }

  return { token: saveToken(user.id) };
}

export async function logout() {
  storage().removeItem(AUTH_TOKEN_KEY);
}

/**
 * В браузере может лежать токен с почтой в `sub`: он заменяется версией с
 * `user_id` и прежним `exp`.
 */
function resolveSession(payload: TokenPayload, token: string): Session {
  const users = readUsers();
  const expiresAt = payload.exp * 1000;

  if (isUuid(payload.sub)) {
    const user = users.find((candidate) => candidate.id === payload.sub);
    if (!user) {
      throw new ApiError('UNAUTHORIZED', 'Сессия недействительна');
    }
    return { userId: user.id, email: user.email, expiresAt, token };
  }

  const user = users.find((candidate) => candidate.email === normalizeEmail(payload.sub));
  if (!user) {
    throw new ApiError('UNAUTHORIZED', 'Сессия недействительна');
  }

  const upgraded = createToken(user.id, payload.exp);
  storage().setItem(AUTH_TOKEN_KEY, upgraded);
  return { userId: user.id, email: user.email, expiresAt, token: upgraded };
}

export async function getSession(): Promise<Session | null> {
  await delay();
  const token = storage().getItem(AUTH_TOKEN_KEY);
  if (!token) {
    return null;
  }

  try {
    const payload = readTokenPayload(token);
    if (payload.exp * 1000 <= Date.now()) {
      throw new ApiError('UNAUTHORIZED', 'Срок сессии истёк', { details: { reason: 'expired' } });
    }
    return resolveSession(payload, token);
  } catch (error) {
    storage().removeItem(AUTH_TOKEN_KEY);
    throw error;
  }
}
