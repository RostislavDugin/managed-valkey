import { beforeEach, describe, expect, it, vi } from 'vitest';
import {
  AUTH_INVALIDATED_EVENT,
  ApiError,
  apiRequest,
  executeApiRequest,
  parseApiResponse,
  type RequestExecutorDependencies,
} from './client';

function jsonResponse(body: unknown, status = 200, headers?: HeadersInit) {
  return new Response(JSON.stringify(body), { status, headers });
}

function dependencies(fetchMock: typeof fetch, sleep = vi.fn().mockResolvedValue(undefined)) {
  return {
    fetch: fetchMock,
    now: () => 0,
    random: () => 0.5,
    sleep,
  } satisfies RequestExecutorDependencies;
}

beforeEach(() => {
  localStorage.clear();
});

describe('apiRequest', () => {
  it('добавляет URL, JSON-заголовки, request id и JWT', async () => {
    localStorage.setItem('mv_token', 'server-token');
    const fetchMock = vi.spyOn(window, 'fetch').mockResolvedValue(jsonResponse({ ok: true }));

    await expect(apiRequest('/probe', { method: 'POST', body: '{}' })).resolves.toEqual({
      ok: true,
    });

    const [url, init] = fetchMock.mock.calls[0];
    const headers = new Headers(init?.headers);
    expect(url).toBe('/v1/probe');
    expect(headers.get('Accept')).toBe('application/json');
    expect(headers.get('Content-Type')).toBe('application/json');
    expect(headers.get('Authorization')).toBe('Bearer server-token');
    expect(headers.get('X-Request-Id')).toMatch(/^[0-9a-f-]{36}$/);
  });
});

describe('повторы', () => {
  it('возвращает успех первой попытки без ожидания', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(jsonResponse({ ok: true }));
    const sleep = vi.fn().mockResolvedValue(undefined);

    await expect(executeApiRequest('/probe', {}, dependencies(fetchMock, sleep))).resolves.toEqual({
      ok: true,
    });
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(sleep).not.toHaveBeenCalled();
  });

  it.each([408, 425, 500, 502, 503, 504])('повторяет временный статус %d', async (status) => {
    const fetchMock = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(jsonResponse({ error: { code: 'TEMPORARY' } }, status))
      .mockResolvedValueOnce(jsonResponse({ ok: true }));
    const sleep = vi.fn().mockResolvedValue(undefined);

    await expect(executeApiRequest('/probe', {}, dependencies(fetchMock, sleep))).resolves.toEqual({
      ok: true,
    });
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(sleep).toHaveBeenCalledWith(125, undefined);
  });

  it('останавливается после трёх сетевых ошибок', async () => {
    const networkError = new TypeError('network failed');
    const fetchMock = vi.fn<typeof fetch>().mockRejectedValue(networkError);
    const sleep = vi.fn().mockResolvedValue(undefined);

    await expect(executeApiRequest('/probe', {}, dependencies(fetchMock, sleep))).rejects.toBe(
      networkError
    );
    expect(fetchMock).toHaveBeenCalledTimes(3);
    expect(sleep.mock.calls.map(([delay]) => delay)).toEqual([125, 250]);
  });

  it.each([400, 401, 403, 404, 409, 422, 429])('не повторяет статус %d', async (status) => {
    const fetchMock = vi
      .fn<typeof fetch>()
      .mockResolvedValue(jsonResponse({ error: { code: 'REJECTED' } }, status));

    await expect(executeApiRequest('/probe', {}, dependencies(fetchMock))).rejects.toBeInstanceOf(
      ApiError
    );
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it('не повторяет изменяющий запрос без политики или ключа', async () => {
    const fetchMock = vi.fn<typeof fetch>().mockRejectedValue(new TypeError('network failed'));

    await expect(
      executeApiRequest('/probe', { method: 'POST', body: '{}' }, dependencies(fetchMock))
    ).rejects.toBeInstanceOf(TypeError);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it('повторяет явно разрешённый POST и не повторяет ReadableStream', async () => {
    const retryingFetch = vi
      .fn<typeof fetch>()
      .mockRejectedValueOnce(new TypeError('network failed'))
      .mockResolvedValueOnce(jsonResponse({ ok: true }));
    await expect(
      executeApiRequest(
        '/safe-post',
        { method: 'POST', body: '{}', retry: true },
        dependencies(retryingFetch)
      )
    ).resolves.toEqual({ ok: true });
    expect(retryingFetch).toHaveBeenCalledTimes(2);

    const streamFetch = vi.fn<typeof fetch>().mockRejectedValue(new TypeError('network failed'));
    await expect(
      executeApiRequest(
        '/stream',
        { method: 'POST', body: new ReadableStream(), retry: true },
        dependencies(streamFetch)
      )
    ).rejects.toBeInstanceOf(TypeError);
    expect(streamFetch).toHaveBeenCalledTimes(1);
  });

  it('сохраняет метод, путь, тело и заголовки запроса с ключом', async () => {
    const headers = new Headers({
      'Idempotency-Key': 'operation-key',
      'X-Request-Id': 'request-id',
    });
    const fetchMock = vi
      .fn<typeof fetch>()
      .mockResolvedValueOnce(jsonResponse({ error: { code: 'TEMPORARY' } }, 503))
      .mockResolvedValueOnce(jsonResponse({ ok: true }));

    await executeApiRequest(
      '/operation',
      { method: 'POST', body: '{"value":1}', headers },
      dependencies(fetchMock)
    );

    expect(fetchMock.mock.calls[0][0]).toBe('/operation');
    expect(fetchMock.mock.calls[1][0]).toBe('/operation');
    for (const [, init] of fetchMock.mock.calls) {
      expect(init?.method).toBe('POST');
      expect(init?.body).toBe('{"value":1}');
      const attemptHeaders = new Headers(init?.headers);
      expect(attemptHeaders.get('Idempotency-Key')).toBe('operation-key');
      expect(attemptHeaders.get('X-Request-Id')).toBe('request-id');
    }
  });
});

describe('отмена', () => {
  it('не начинает отменённый запрос', async () => {
    const controller = new AbortController();
    controller.abort();
    const fetchMock = vi.fn<typeof fetch>();

    await expect(
      executeApiRequest('/probe', { signal: controller.signal }, dependencies(fetchMock))
    ).rejects.toMatchObject({ name: 'AbortError' });
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('не преобразует AbortError из fetch', async () => {
    const abortError = new DOMException('aborted', 'AbortError');
    const fetchMock = vi.fn<typeof fetch>().mockRejectedValue(abortError);

    await expect(executeApiRequest('/probe', {}, dependencies(fetchMock))).rejects.toBe(abortError);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it('останавливает активный fetch через AbortSignal', async () => {
    const controller = new AbortController();
    const fetchMock = vi.fn<typeof fetch>(
      (_input, init) =>
        new Promise((_resolve, reject) => {
          init?.signal?.addEventListener('abort', () => reject(new TypeError('fetch stopped')));
        })
    );
    const result = executeApiRequest(
      '/probe',
      { signal: controller.signal },
      dependencies(fetchMock)
    );
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    controller.abort();

    await expect(result).rejects.toMatchObject({ name: 'AbortError' });
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it('останавливается между попытками', async () => {
    const controller = new AbortController();
    const fetchMock = vi.fn<typeof fetch>().mockRejectedValue(new TypeError('network failed'));
    const sleep = vi.fn(
      (_milliseconds: number, signal?: AbortSignal | null) =>
        new Promise<void>((_resolve, reject) => {
          signal?.addEventListener('abort', () =>
            reject(new DOMException('aborted', 'AbortError'))
          );
        })
    );
    const result = executeApiRequest(
      '/probe',
      { signal: controller.signal },
      dependencies(fetchMock, sleep)
    );
    await vi.waitFor(() => expect(sleep).toHaveBeenCalledTimes(1));
    controller.abort();

    await expect(result).rejects.toMatchObject({ name: 'AbortError' });
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});

describe('ошибки ответа', () => {
  it('берёт retry_after из JSON, затем заголовка, затем использует 60 секунд', async () => {
    await expect(
      parseApiResponse(
        jsonResponse({ error: { code: 'RATE_LIMITED', details: { retry_after: 17 } } }, 429)
      )
    ).rejects.toMatchObject({ details: { retry_after: 17 } });
    await expect(
      parseApiResponse(new Response('', { status: 429, headers: { 'Retry-After': '23' } }))
    ).rejects.toMatchObject({ code: 'RATE_LIMITED', details: { retry_after: 23 } });
    await expect(
      parseApiResponse(
        new Response('', {
          status: 429,
          headers: { 'Retry-After': 'Thu, 01 Jan 1970 00:00:09 GMT' },
        }),
        () => 5000
      )
    ).rejects.toMatchObject({ code: 'RATE_LIMITED', details: { retry_after: 4 } });
    await expect(parseApiResponse(new Response('', { status: 429 }))).rejects.toMatchObject({
      code: 'RATE_LIMITED',
      details: { retry_after: 60 },
    });
  });

  it('отмечает недействительный JSON', async () => {
    await expect(parseApiResponse(new Response('not-json', { status: 500 }))).rejects.toMatchObject(
      {
        code: 'INVALID_RESPONSE',
      }
    );
  });

  it('удаляет токен и отправляет событие при 401', async () => {
    localStorage.setItem('mv_token', 'server-token');
    const listener = vi.fn();
    window.addEventListener(AUTH_INVALIDATED_EVENT, listener, { once: true });

    await expect(
      parseApiResponse(jsonResponse({ error: { code: 'UNAUTHORIZED' } }, 401))
    ).rejects.toMatchObject({ code: 'UNAUTHORIZED' });
    expect(localStorage.getItem('mv_token')).toBeNull();
    expect(listener).toHaveBeenCalledTimes(1);
  });
});
