import { beforeEach, describe, expect, it, vi } from 'vitest';
import { METRIC_WINDOWS } from '../model/valkey-observability';
import { getValkeyMetrics } from './valkey-metrics';

const INSTANCE_ID = '01930000-0000-7000-8000-000000000002';

function response() {
  return new Response(
    JSON.stringify({
      from: '2026-09-09T10:00:00Z',
      to: '2026-09-09T10:05:00Z',
      step_seconds: 10,
      nodes: [
        {
          ordinal: 1,
          name: 'shop-abc123-1',
          role: 'unknown',
          points: [
            {
              collected_at: '2026-09-09T10:00:00Z',
              used_memory_bytes: 1024,
              cpu_millicores: null,
              connected_clients: 4.5,
              ops_per_sec: 25,
              keyspace_hits: 7,
              keyspace_misses: 2,
              evicted_keys: 0,
            },
          ],
        },
      ],
    }),
    { headers: { 'Content-Type': 'application/json' } }
  );
}

beforeEach(() => {
  localStorage.clear();
  localStorage.setItem('mv_token', 'server-token');
});

describe('клиент метрик Valkey', () => {
  it.each(METRIC_WINDOWS)(
    'для окна %s передаёт выбранный диапазон и идентификатор базы в API',
    async (range) => {
      const fetchMock = vi.spyOn(window, 'fetch').mockResolvedValue(response());
      const controller = new AbortController();

      await getValkeyMetrics({ instanceId: INSTANCE_ID, range, signal: controller.signal });

      expect(fetchMock).toHaveBeenCalledOnce();
      expect(fetchMock.mock.calls[0][0]).toBe(
        `/v1/managed/valkey/instances/${INSTANCE_ID}/metrics?range=${range}`
      );
      expect(fetchMock.mock.calls[0][1]?.signal).toBe(controller.signal);
    }
  );

  it('при получении метрик преобразует серверный DTO без добавления автора или демонстрационного источника', async () => {
    vi.spyOn(window, 'fetch').mockResolvedValue(response());

    const metrics = await getValkeyMetrics({
      instanceId: INSTANCE_ID,
      range: '5m',
      signal: new AbortController().signal,
    });

    expect(metrics).toEqual({
      from: '2026-09-09T10:00:00Z',
      to: '2026-09-09T10:05:00Z',
      stepSeconds: 10,
      nodes: [
        {
          id: '1',
          ordinal: 1,
          name: 'shop-abc123-1',
          role: 'unknown',
          points: [
            {
              collectedAt: '2026-09-09T10:00:00Z',
              usedMemoryBytes: 1024,
              cpuMillicores: null,
              connectedClients: 4.5,
              opsPerSec: 25,
              keyspaceHits: 7,
              keyspaceMisses: 2,
              evictedKeys: 0,
            },
          ],
        },
      ],
    });
    expect(metrics).not.toHaveProperty('ownerId');
    expect(metrics).not.toHaveProperty('source');
  });

  it('при отмене загрузки метрик передаёт исходный AbortSignal в fetch', async () => {
    const fetchMock = vi.spyOn(window, 'fetch').mockImplementation((_, init) => {
      return new Promise((_, reject) => {
        init?.signal?.addEventListener('abort', () => reject(init.signal?.reason), { once: true });
      });
    });
    const controller = new AbortController();
    const pending = getValkeyMetrics({
      instanceId: INSTANCE_ID,
      range: '1h',
      signal: controller.signal,
    });

    controller.abort(new DOMException('Запрос отменён', 'AbortError'));

    await expect(pending).rejects.toMatchObject({ name: 'AbortError' });
    expect(fetchMock).toHaveBeenCalledOnce();
  });
});
