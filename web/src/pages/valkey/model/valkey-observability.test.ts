import { describe, expect, it } from 'vitest';
import {
  buildMetricChartRows,
  formatMetricAxisTime,
  formatMetricValue,
  METRIC_WINDOWS,
  METRIC_WINDOW_CONFIG,
} from './valkey-observability';

function localIso(year: number, month: number, day: number, hour = 12) {
  return new Date(year, month - 1, day, hour).toISOString();
}

describe('модель метрик Valkey', () => {
  it('задаёт четыре окна и их шаг', () => {
    expect(METRIC_WINDOWS).toEqual(['5m', '1h', '24h', '7d']);
    expect(METRIC_WINDOWS.map((range) => METRIC_WINDOW_CONFIG[range].stepMs)).toEqual([
      10_000, 60_000, 300_000, 1_800_000,
    ]);
  });

  it('форматирует единицы метрик и пропуски', () => {
    expect(formatMetricValue('usedMemoryBytes', 1.5 * 1024 ** 3)).toBe('1,5\u00A0ГБ');
    expect(formatMetricValue('cpuMillicores', 62.7)).toBe('62,7\u00A0%');
    expect(formatMetricValue('connectedClients', 1200)).toMatch(/1\s200/);
    expect(formatMetricValue('opsPerSec', 1200)).toMatch(/1\s200\sоп\/\u0441/);
    expect(formatMetricValue('evictedKeys', null)).toBe('Нет данных');
  });

  it('показывает календарный день на недельной оси', () => {
    const value = localIso(2026, 9, 8);
    expect(formatMetricAxisTime(value, '7d')).toMatch(/8\sсент/i);
    expect(formatMetricAxisTime(value, '1h')).toMatch(/\d{2}:\d{2}/);
  });

  it('сводит ряды нод по времени, переводит CPU в проценты и сохраняет null', () => {
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
});
