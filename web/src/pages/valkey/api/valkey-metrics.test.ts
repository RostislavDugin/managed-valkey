import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { METRIC_WINDOWS, METRIC_WINDOW_CONFIG } from '../model/valkey-observability';
import { getValkeyMetrics } from './valkey-metrics';
import { VALKEY_INSTANCES_KEY } from './valkey-storage';

const OWNER = 'owner-1';

function seedInstance(mode: 'single' | 'ha') {
  localStorage.setItem(
    VALKEY_INSTANCES_KEY,
    JSON.stringify({
      version: 4,
      instances: [
        {
          id: 'instance-1',
          ownerId: OWNER,
          name: 'valkey-1474',
          prefix: 'valkey',
          slug: 'valkey-abc123',
          mode,
          vcpu: 2,
          ramGb: 4,
          isWhitelistEnabled: false,
          whitelistCidrs: [],
          status: 'running',
          createdAt: '2026-09-08T00:00:00.000Z',
          updatedAt: '2026-09-08T00:00:00.000Z',
        },
      ],
      auditLogs: [],
    })
  );
}

async function load(range: (typeof METRIC_WINDOWS)[number], signal = new AbortController().signal) {
  const promise = getValkeyMetrics({ ownerId: OWNER, instanceId: 'instance-1', range, signal });
  await vi.advanceTimersByTimeAsync(400);
  return promise;
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime('2026-09-08T12:34:56.000Z');
});

afterEach(() => {
  vi.useRealTimers();
});

describe('временный клиент метрик', () => {
  it.each(METRIC_WINDOWS)('возвращает заданное число точек для окна %s', async (range) => {
    seedInstance('single');

    const response = await load(range);

    expect(response.nodes[0].points).toHaveLength(METRIC_WINDOW_CONFIG[range].pointCount);
  });

  it('возвращает primary для single и primary с двумя репликами для ha', async () => {
    seedInstance('single');
    const single = await load('5m');

    seedInstance('ha');
    const ha = await load('5m');

    expect(single.nodes.map((node) => node.role)).toEqual(['primary']);
    expect(ha.nodes.map((node) => node.role)).toEqual(['primary', 'replica', 'replica']);
  });

  it('стабильно повторяет значения текущей временной корзины', async () => {
    seedInstance('ha');

    const first = await load('1h');
    const second = await load('1h');

    expect(second).toEqual(first);
  });

  it('соблюдает границы памяти, CPU и неотрицательных счётчиков', async () => {
    seedInstance('ha');

    const response = await load('7d');
    const points = response.nodes.flatMap((node) => node.points);
    const memoryValues = points.flatMap((point) =>
      point.usedMemoryBytes === null ? [] : [point.usedMemoryBytes]
    );
    const cpuValues = points.flatMap((point) =>
      point.cpuMillicores === null ? [] : [point.cpuMillicores]
    );

    expect(memoryValues.every((value) => value >= 0 && value <= 4 * 1024 * 1024 * 1024)).toBe(true);
    expect(cpuValues.every((value) => value >= 0 && value <= 2000)).toBe(true);

    for (const point of points) {
      expect(point.connectedClients === null || point.connectedClients >= 0).toBe(true);
      expect(point.opsPerSec === null || point.opsPerSec >= 0).toBe(true);
      expect(point.keyspaceHits === null || point.keyspaceHits >= 0).toBe(true);
      expect(point.keyspaceMisses === null || point.keyspaceMisses >= 0).toBe(true);
      expect(point.evictedKeys === null || point.evictedKeys >= 0).toBe(true);
    }
  });

  it('отменяет запрос через AbortSignal', async () => {
    seedInstance('single');
    const controller = new AbortController();
    const promise = getValkeyMetrics({
      ownerId: OWNER,
      instanceId: 'instance-1',
      range: '5m',
      signal: controller.signal,
    });

    controller.abort();

    await expect(promise).rejects.toMatchObject({ name: 'AbortError' });
  });
});
