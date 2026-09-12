import { describe, expect, it } from 'vitest';
import {
  buildMetricChartRows,
  buildMetricRenderRows,
  formatMetricAxisTime,
  formatMetricValue,
  getMetricRenderSeriesKey,
  getMetricSeriesKey,
  METRIC_WINDOWS,
  METRIC_WINDOW_CONFIG,
} from './valkey-observability';

const METRIC_VALUES = {
  usedMemoryBytes: 10,
  cpuMillicores: 10,
  connectedClients: 10,
  opsPerSec: 10,
  keyspaceHits: 10,
  keyspaceMisses: 10,
  evictedKeys: 10,
};

function metricPoint(collectedAt: string, value: number | null) {
  return {
    collectedAt,
    ...METRIC_VALUES,
    usedMemoryBytes: value,
  };
}

function renderMemory(points: ReturnType<typeof metricPoint>[], stepSeconds: number) {
  const response = {
    from: points[0]!.collectedAt,
    to: points.at(-1)!.collectedAt,
    stepSeconds,
    nodes: [{ id: '0', ordinal: 0, name: 'valkey-0', role: 'primary' as const, points }],
  };
  const rows = buildMetricChartRows(response.nodes, 1);
  return buildMetricRenderRows(rows, response.nodes, 'usedMemoryBytes');
}

function localIso(year: number, month: number, day: number, hour = 12) {
  return new Date(year, month - 1, day, hour).toISOString();
}

describe('модель метрик Valkey', () => {
  it('для выбора периода предоставляет четыре окна метрик с соответствующим шагом', () => {
    expect(METRIC_WINDOWS).toEqual(['5m', '1h', '24h', '7d']);
    expect(METRIC_WINDOWS.map((range) => METRIC_WINDOW_CONFIG[range].stepMs)).toEqual([
      10_000, 60_000, 300_000, 1_800_000,
    ]);
  });

  it('для значений метрик форматирует байты, проценты, операции и отсутствующие точки', () => {
    expect(formatMetricValue('usedMemoryBytes', 1.5 * 1024 ** 3)).toBe('1,5\u00A0ГБ');
    expect(formatMetricValue('cpuMillicores', 62.7)).toBe('62,7\u00A0%');
    expect(formatMetricValue('connectedClients', 1200)).toMatch(/1\s200/);
    expect(formatMetricValue('opsPerSec', 1200)).toMatch(/1\s200\sоп\/\u0441/);
    expect(formatMetricValue('evictedKeys', null)).toBe('Нет данных');
  });

  it('для недельного окна показывает на оси местный календарный день вместо времени', () => {
    const value = localIso(2026, 9, 8);
    expect(formatMetricAxisTime(value, '7d')).toMatch(/8\sсент/i);
    expect(formatMetricAxisTime(value, '1h')).toMatch(/\d{2}:\d{2}/);
  });

  it('при объединении рядов нод выравнивает точки по времени, переводит CPU в проценты и сохраняет пропуски', () => {
    const collectedAt = localIso(2026, 9, 8);
    const metricPoint = {
      collectedAt,
      usedMemoryBytes: 1,
      cpuMillicores: 1000,
      connectedClients: 2,
      opsPerSec: 3,
      keyspaceHits: 4,
      keyspaceMisses: null,
      evictedKeys: 6,
    };

    expect(
      buildMetricChartRows(
        [
          { id: '0', ordinal: 0, name: 'node-0', role: 'primary', points: [metricPoint] },
          { id: '1', ordinal: 1, name: 'node-1', role: 'unknown', points: [metricPoint] },
        ],
        2
      )[0]
    ).toMatchObject({
      '0:usedMemoryBytes': 1,
      '0:cpuMillicores': 50,
      '0:keyspaceMisses': null,
      '1:usedMemoryBytes': 1,
    });
  });

  it('для отрисовки линейно заполняет ограниченный пропуск ровно в 60 секунд, не меняя исходное значение', () => {
    const rows = renderMemory(
      [
        metricPoint('2026-09-08T12:00:00.000Z', 10),
        metricPoint('2026-09-08T12:00:30.000Z', null),
        metricPoint('2026-09-08T12:01:00.000Z', 30),
      ],
      10
    );

    expect(rows[1]?.[getMetricSeriesKey('0', 'usedMemoryBytes')]).toBeNull();
    expect(rows[1]?.[getMetricRenderSeriesKey('0', 'usedMemoryBytes')]).toBe(20);
  });

  it('для отрисовки оставляет разрыв при интервале 60 001 миллисекунда', () => {
    const rows = renderMemory(
      [
        metricPoint('2026-09-08T12:00:00.000Z', 10),
        metricPoint('2026-09-08T12:00:30.000Z', null),
        metricPoint('2026-09-08T12:01:00.001Z', 30),
      ],
      10
    );

    expect(rows[1]?.[getMetricRenderSeriesKey('0', 'usedMemoryBytes')]).toBeNull();
  });

  it('для одинаковых меток времени принимает одно решение при разных stepSeconds', () => {
    const points = [
      metricPoint('2026-09-08T12:00:00.000Z', 10),
      metricPoint('2026-09-08T12:00:30.000Z', null),
      metricPoint('2026-09-08T12:01:00.000Z', 30),
    ];
    const renderKey = getMetricRenderSeriesKey('0', 'usedMemoryBytes');

    expect(renderMemory(points, 10).map((row) => row[renderKey])).toEqual(
      renderMemory(points, 300).map((row) => row[renderKey])
    );
  });

  it('для краевого пропуска и не возрастающих меток времени сохраняет null', () => {
    const valueKey = getMetricSeriesKey('0', 'usedMemoryBytes');
    const renderKey = getMetricRenderSeriesKey('0', 'usedMemoryBytes');
    const node = { id: '0', ordinal: 0, name: 'valkey-0', role: 'primary' as const, points: [] };
    const rows = buildMetricRenderRows(
      [
        { collectedAt: '2026-09-08T12:00:00.000Z', [valueKey]: null },
        { collectedAt: '2026-09-08T12:00:20.000Z', [valueKey]: 10 },
        { collectedAt: '2026-09-08T12:00:10.000Z', [valueKey]: null },
        { collectedAt: '2026-09-08T12:00:40.000Z', [valueKey]: 30 },
        { collectedAt: '2026-09-08T12:00:50.000Z', [valueKey]: null },
      ],
      [node],
      'usedMemoryBytes'
    );

    expect(rows.map((row) => row[renderKey])).toEqual([null, 10, null, 30, null]);
  });
});
