import { createUuidV7 } from '@/shared/lib';
import { ApiError, apiRequest } from './client';

export const AUTH_TOKEN_KEY = 'mv_token';

interface TokenPayload {
  sub: string;
  exp: number;
}

interface CurrentUserResponse {
  user: { id: string; email: string };
  quota: { max_vcpu: number; max_ram_gb: number };
  usage: { used_vcpu: number; used_ram_gb: number };
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

function storage() {
  if (typeof localStorage === 'undefined') {
    throw new Error('Хранилище браузера недоступно');
  }
  return localStorage;
}

function decodeBase64Url(value: string) {
  const normalized = value.replaceAll('-', '+').replaceAll('_', '/');
  const padding = '='.repeat((4 - (normalized.length % 4)) % 4);
  const binary = atob(`${normalized}${padding}`);
  const bytes = Uint8Array.from(binary, (character) => character.charCodeAt(0));
  return JSON.parse(new TextDecoder().decode(bytes)) as unknown;
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

function saveToken(result: AuthResult) {
  storage().setItem(AUTH_TOKEN_KEY, result.token);
  return result;
}

export function checkEmail(input: { email: string }): Promise<{ exists: boolean }> {
  return apiRequest('/auth/check-email', {
    method: 'POST',
    body: JSON.stringify(input),
    retry: true,
  });
}

export async function register(input: { email: string; password: string }): Promise<AuthResult> {
  const result = await apiRequest<AuthResult>('/auth/register', {
    method: 'POST',
    body: JSON.stringify(input),
    headers: { 'Idempotency-Key': createUuidV7() },
  });
  return saveToken(result);
}

export async function login(input: { email: string; password: string }): Promise<AuthResult> {
  const result = await apiRequest<AuthResult>('/auth/login', {
    method: 'POST',
    body: JSON.stringify(input),
    retry: true,
  });
  return saveToken(result);
}

export async function logout() {
  storage().removeItem(AUTH_TOKEN_KEY);
}

export async function getSession(): Promise<Session | null> {
  const token = storage().getItem(AUTH_TOKEN_KEY);
  if (!token) {
    return null;
  }

  try {
    const payload = readTokenPayload(token);
    if (payload.exp * 1000 <= Date.now()) {
      throw new ApiError('UNAUTHORIZED', 'Срок сессии истёк', { details: { reason: 'expired' } });
    }

    const current = await apiRequest<CurrentUserResponse>('/me');
    return {
      userId: current.user.id,
      email: current.user.email,
      expiresAt: payload.exp * 1000,
      token,
    };
  } catch (error) {
    if (error instanceof ApiError && error.code === 'UNAUTHORIZED') {
      storage().removeItem(AUTH_TOKEN_KEY);
    }
    throw error;
  }
}
