import { createUuidV7 } from '@/shared/lib';

export type ApiErrorCode =
  | 'CONFLICT'
  | 'IDEMPOTENCY_MISMATCH'
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
const RETRYABLE_STATUSES = new Set([408, 425, 500, 502, 503, 504]);
const SAFE_METHODS = new Set(['GET', 'HEAD', 'OPTIONS']);
const MAX_ATTEMPTS = 3;
const BASE_DELAY_MS = 250;
const HTTP_UNAUTHORIZED = 401;
const HTTP_TOO_MANY_REQUESTS = 429;

export interface ApiRequestInit extends RequestInit {
  retry?: boolean;
}

export interface RequestExecutorDependencies {
  fetch: typeof fetch;
  now: () => number;
  random: () => number;
  sleep: (milliseconds: number, signal?: AbortSignal | null) => Promise<void>;
}

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

function rateLimitDetails(
  response: Response,
  now: () => number,
  details?: Record<string, unknown>
) {
  if (typeof details?.retry_after === 'number' && Number.isFinite(details.retry_after)) {
    return details;
  }

  const header = response.headers.get('Retry-After') ?? '';
  if (/^\d+$/.test(header)) {
    return { ...details, retry_after: Number.parseInt(header, 10) };
  }

  const retryAt = Date.parse(header);
  const seconds = Math.ceil((retryAt - now()) / 1000);
  return { ...details, retry_after: Number.isFinite(seconds) && seconds >= 0 ? seconds : 60 };
}

export async function parseApiResponse<T>(
  response: Response,
  now: () => number = Date.now
): Promise<T> {
  if (response.status === HTTP_UNAUTHORIZED) {
    invalidateSession();
  }

  const body = await response.text();
  let data: unknown;

  if (body) {
    try {
      data = JSON.parse(body);
    } catch {
      if (response.status !== HTTP_TOO_MANY_REQUESTS) {
        throw new ApiError('INVALID_RESPONSE', 'API вернул ответ в неизвестном формате', {
          status: response.status,
        });
      }
    }
  }

  if (response.ok) {
    return data as T;
  }

  if (isApiErrorPayload(data)) {
    const { code, message } = data.error;
    const details =
      response.status === HTTP_TOO_MANY_REQUESTS
        ? rateLimitDetails(response, now, data.error.details)
        : data.error.details;
    throw new ApiError(code, message, { details, status: response.status });
  }

  if (response.status === HTTP_TOO_MANY_REQUESTS) {
    throw new ApiError('RATE_LIMITED', 'Слишком много запросов', {
      details: rateLimitDetails(response, now),
      status: response.status,
    });
  }

  throw new ApiError('REQUEST_FAILED', `API ответил со статусом ${response.status}`, {
    status: response.status,
  });
}

function canRetry(method: string, headers: Headers, explicitlySafe: boolean | undefined) {
  return SAFE_METHODS.has(method) || explicitlySafe === true || headers.has('Idempotency-Key');
}

function isAbortError(error: unknown) {
  return error instanceof DOMException && error.name === 'AbortError';
}

function throwIfAborted(signal?: AbortSignal | null) {
  if (signal?.aborted) {
    throw signal.reason instanceof Error
      ? signal.reason
      : new DOMException('Запрос отменён', 'AbortError');
  }
}

export async function executeApiRequest<T>(
  path: string,
  init: ApiRequestInit,
  dependencies: RequestExecutorDependencies
): Promise<T> {
  const method = (init.method ?? 'GET').toUpperCase();
  const headers = new Headers(init.headers);
  const retryAllowed =
    canRetry(method, headers, init.retry) && !(init.body instanceof ReadableStream);

  for (let attempt = 0; attempt < MAX_ATTEMPTS; attempt += 1) {
    throwIfAborted(init.signal);

    let response: Response | undefined;
    try {
      response = await dependencies.fetch(path, { ...init, method, headers });
    } catch (error) {
      if (init.signal?.aborted) {
        throwIfAborted(init.signal);
      }
      if (isAbortError(error)) {
        throw error;
      }
      if (!retryAllowed || attempt === MAX_ATTEMPTS - 1) {
        throw error;
      }
    }

    if (
      response &&
      (!RETRYABLE_STATUSES.has(response.status) || !retryAllowed || attempt === MAX_ATTEMPTS - 1)
    ) {
      return parseApiResponse<T>(response, dependencies.now);
    }

    throwIfAborted(init.signal);
    const maximumDelay = BASE_DELAY_MS * 2 ** attempt;
    await dependencies.sleep(dependencies.random() * maximumDelay, init.signal);
  }

  throw new ApiError('REQUEST_FAILED');
}

function browserSleep(milliseconds: number, signal?: AbortSignal | null) {
  return new Promise<void>((resolve, reject) => {
    throwIfAborted(signal);

    const timeout = window.setTimeout(finish, milliseconds);
    signal?.addEventListener('abort', abort, { once: true });

    function finish() {
      signal?.removeEventListener('abort', abort);
      resolve();
    }

    function abort() {
      window.clearTimeout(timeout);
      signal?.removeEventListener('abort', abort);
      reject(
        signal?.reason instanceof Error
          ? signal.reason
          : new DOMException('Запрос отменён', 'AbortError')
      );
    }
  });
}

export async function apiRequest<T>(path: string, init: ApiRequestInit = {}): Promise<T> {
  const headers = new Headers(init.headers);
  const token = getToken();

  headers.set('Accept', 'application/json');
  if (!headers.has('X-Request-Id')) {
    headers.set('X-Request-Id', createUuidV7());
  }
  if (init.body && !(init.body instanceof FormData) && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json');
  }
  if (token) {
    headers.set('Authorization', `Bearer ${token}`);
  }

  return executeApiRequest<T>(
    `${API_BASE_URL}${path}`,
    { ...init, headers },
    {
      fetch: window.fetch.bind(window),
      now: Date.now,
      random: Math.random,
      sleep: browserSleep,
    }
  );
}
