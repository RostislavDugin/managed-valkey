export type ApiErrorCode =
  | 'CONFLICT'
  | 'RATE_LIMITED'
  | 'UNAUTHORIZED'
  | 'VALIDATION_FAILED'
  | (string & {});

export interface ApiErrorPayload {
  error: {
    code: ApiErrorCode;
    message?: string;
    details?: Record<string, unknown>;
  };
}

export class ApiError extends Error {
  readonly code: ApiErrorCode;
  readonly details?: Record<string, unknown>;
  readonly status?: number;

  constructor(
    code: ApiErrorCode,
    message = 'Запрос завершился с ошибкой',
    options: { details?: Record<string, unknown>; status?: number } = {}
  ) {
    super(message);
    this.name = 'ApiError';
    this.code = code;
    this.details = options.details;
    this.status = options.status;
  }
}

export const AUTH_INVALIDATED_EVENT = 'managed-valkey:auth-invalidated';

const env = (import.meta as ImportMeta & { env?: Record<string, string | undefined> }).env;
const API_BASE_URL = env?.VITE_API_BASE_URL ?? '/v1';

function getToken() {
  return typeof localStorage === 'undefined' ? null : localStorage.getItem('mv_token');
}

function invalidateSession() {
  if (typeof localStorage !== 'undefined') {
    localStorage.removeItem('mv_token');
  }

  if (typeof window !== 'undefined') {
    window.dispatchEvent(new Event(AUTH_INVALIDATED_EVENT));
  }
}

function isApiErrorPayload(value: unknown): value is ApiErrorPayload {
  if (!value || typeof value !== 'object' || !('error' in value)) {
    return false;
  }

  const error = value.error;
  return Boolean(
    error && typeof error === 'object' && 'code' in error && typeof error.code === 'string'
  );
}

export async function parseApiResponse<T>(response: Response): Promise<T> {
  const body = await response.text();
  let data: unknown;

  if (body) {
    try {
      data = JSON.parse(body);
    } catch {
      throw new ApiError('INVALID_RESPONSE', 'Api вернул ответ в неизвестном формате', {
        status: response.status,
      });
    }
  }

  if (response.ok) {
    return data as T;
  }

  if (isApiErrorPayload(data)) {
    const { code, details, message } = data.error;
    if (code === 'UNAUTHORIZED') {
      invalidateSession();
    }
    throw new ApiError(code, message, { details, status: response.status });
  }

  throw new ApiError('REQUEST_FAILED', `Api ответил со статусом ${response.status}`, {
    status: response.status,
  });
}

export async function apiRequest<T>(path: string, init: RequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  const token = getToken();

  headers.set('Accept', 'application/json');
  if (init.body && !(init.body instanceof FormData) && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json');
  }
  if (token) {
    headers.set('Authorization', `Bearer ${token}`);
  }

  const response = await fetch(`${API_BASE_URL}${path}`, { ...init, headers });
  return parseApiResponse<T>(response);
}
