import { describe, expect, it } from 'vitest';
import {
  AUDIT_ACTION_LABELS,
  buildMetricChartRows,
  formatMetricAxisTime,
  formatMetricValue,
  getAuditGroupLabel,
  groupAuditLogs,
  METRIC_WINDOWS,
  METRIC_WINDOW_CONFIG,
  type AuditLogEntry,
} from './valkey-observability';

function localIso(year: number, month: number, day: number, hour = 12) {
  return new Date(year, month - 1, day, hour).toISOString();
}

function event(id: string, createdAt: string): AuditLogEntry {
  return {
    id,
    instanceId: 'instance-1',
    action: 'instance.create',
    userEmail: 'user@example.com',
    createdAt,
  };
}

describe('модель наблюдаемости Valkey', () => {
  it('задаёт четыре окна и точное число точек', () => {
    expect(METRIC_WINDOWS).toEqual(['5m', '1h', '24h', '7d']);
    expect(METRIC_WINDOWS.map((range) => METRIC_WINDOW_CONFIG[range].pointCount)).toEqual([
      30, 60, 288, 336,
    ]);
  });

  it('форматирует единицы метрик и пропуски', () => {
    expect(formatMetricValue('usedMemoryBytes', 1.5 * 1024 ** 3)).toBe('1,5\u00A0ГБ');
    expect(formatMetricValue('cpuMillicores', 62.7)).toBe('62,7\u00A0%');
    expect(formatMetricValue('connectedClients', 1200)).toMatch(/1\s200/);
    expect(formatMetricValue('opsPerSec', 1200)).toMatch(/1\s200\sоп\/с/);
    expect(formatMetricValue('evictedKeys', null)).toBe('Нет данных');
  });

  it('сопоставляет технические действия русским подписям', () => {
    expect(AUDIT_ACTION_LABELS).toEqual({
      'instance.create': 'Создана база',
      'instance.update': 'Изменено имя',
      'instance.resize': 'Изменён тариф',
      'instance.password.rotate': 'Изменён пароль',
      'instance.delete': 'Удалена база',
    });
  });

  it('группирует события по местной дате на границе дня', () => {
    const now = new Date(2026, 8, 8, 0, 5);
    const today = event('today', localIso(2026, 9, 8, 0));
    const yesterday = event('yesterday', localIso(2026, 9, 7, 23));

    expect(groupAuditLogs([today, yesterday], now).map((group) => group.label)).toEqual([
      'Сегодня',
      'Вчера',
    ]);
  });

  it('добавляет год только для события из другого года', () => {
    const now = new Date(2026, 8, 8, 12);

    expect(getAuditGroupLabel(localIso(2026, 8, 20), now)).not.toContain('2026');
    expect(getAuditGroupLabel(localIso(2025, 8, 20), now)).toContain('2025');
  });

  it('показывает календарный день на недельной оси', () => {
    const value = localIso(2026, 9, 8);
    expect(formatMetricAxisTime(value, '7d')).toMatch(/8\sсент/i);
    expect(formatMetricAxisTime(value, '1h')).toMatch(/\d{2}:\d{2}/);
  });

  it('сводит ряды нод по времени и сохраняет null', () => {
    const collectedAt = localIso(2026, 9, 8);
    const metricPoint = {
      collectedAt,
      usedMemoryBytes: 1,
      cpuMillicores: 1000,
      connectedClients: 2,
      opsPerSec: 3,
      keyspaceHits: 4,
      keyspaceMisses: 5,
      evictedKeys: 6,
    };

    expect(
      buildMetricChartRows(
        [
          { id: 'primary', name: 'node-0', role: 'primary', points: [metricPoint] },
          { id: 'replica', name: 'node-1', role: 'replica', points: [metricPoint] },
        ],
        2
      )[0]
    ).toMatchObject({
      'primary:usedMemoryBytes': 1,
      'primary:cpuMillicores': 50,
      'replica:usedMemoryBytes': 1,
    });
  });
});
