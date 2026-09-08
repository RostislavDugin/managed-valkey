export const METRIC_WINDOWS = ['5m', '1h', '24h', '7d'] as const;

export type MetricWindow = (typeof METRIC_WINDOWS)[number];
export type ValkeyNodeRole = 'primary' | 'replica';
export type AuditAction =
  | 'instance.create'
  | 'instance.update'
  | 'instance.resize'
  | 'instance.password.rotate'
  | 'instance.delete';

export type MetricName =
  | 'usedMemoryBytes'
  | 'cpuMillicores'
  | 'connectedClients'
  | 'opsPerSec'
  | 'keyspaceHits'
  | 'keyspaceMisses'
  | 'evictedKeys';

export interface MetricPoint extends Record<MetricName, number | null> {
  collectedAt: string;
}

export interface ValkeyMetricNode {
  id: string;
  name: string;
  role: ValkeyNodeRole;
  points: MetricPoint[];
}

export interface ValkeyMetricsResponse {
  source: 'demo';
  range: MetricWindow;
  nodes: ValkeyMetricNode[];
}

export interface AuditLogEntry {
  id: string;
  instanceId: string;
  action: AuditAction;
  userEmail: string;
  createdAt: string;
}

export interface AuditLogPage {
  items: AuditLogEntry[];
  nextCursor: string | null;
}

export interface AuditLogGroup {
  key: string;
  label: string;
  items: AuditLogEntry[];
}

export type MetricChartRow = { collectedAt: string } & Record<string, string | number | null>;

export const METRIC_WINDOW_CONFIG: Record<
  MetricWindow,
  { label: string; pointCount: number; stepMs: number }
> = {
  '5m': { label: '5 минут', pointCount: 30, stepMs: 10_000 },
  '1h': { label: '1 час', pointCount: 60, stepMs: 60_000 },
  '24h': { label: '24 часа', pointCount: 288, stepMs: 300_000 },
  '7d': { label: '7 дней', pointCount: 336, stepMs: 1_800_000 },
};

export const AUDIT_ACTION_LABELS: Record<AuditAction, string> = {
  'instance.create': 'Создана база',
  'instance.update': 'Изменено имя',
  'instance.resize': 'Изменён тариф',
  'instance.password.rotate': 'Изменён пароль',
  'instance.delete': 'Удалена база',
};

const NUMBER_FORMAT = new Intl.NumberFormat('ru-RU', { maximumFractionDigits: 1 });
const MEMORY_FORMAT = new Intl.NumberFormat('ru-RU', { maximumFractionDigits: 2 });
const INTEGER_FORMAT = new Intl.NumberFormat('ru-RU', { maximumFractionDigits: 0 });
const TIME_FORMAT = new Intl.DateTimeFormat('ru-RU', {
  hour: '2-digit',
  minute: '2-digit',
});
const DAY_FORMAT = new Intl.DateTimeFormat('ru-RU', { day: 'numeric', month: 'short' });
const DAY_WITH_YEAR_FORMAT = new Intl.DateTimeFormat('ru-RU', {
  day: 'numeric',
  month: 'short',
  year: 'numeric',
});
const NBSP = '\u00A0';
const GIBIBYTE = 1024 ** 3;

function localDateKey(value: string | Date) {
  const date = value instanceof Date ? value : new Date(value);
  const year = date.getFullYear();
  const month = String(date.getMonth() + 1).padStart(2, '0');
  const day = String(date.getDate()).padStart(2, '0');
  return `${year}-${month}-${day}`;
}

export function formatMetricValue(metric: MetricName, value: number | null) {
  if (value === null) {
    return 'Нет данных';
  }

  if (metric === 'usedMemoryBytes') {
    return `${MEMORY_FORMAT.format(value / GIBIBYTE)}${NBSP}ГБ`;
  }

  if (metric === 'cpuMillicores') {
    return `${NUMBER_FORMAT.format(value)}${NBSP}%`;
  }

  if (metric === 'opsPerSec') {
    return `${INTEGER_FORMAT.format(value)}${NBSP}оп/с`;
  }

  return INTEGER_FORMAT.format(value);
}

export function toDisplayMetricValue(metric: MetricName, value: number | null, vcpu: number) {
  if (value === null || metric !== 'cpuMillicores') {
    return value;
  }

  return Math.min(100, Math.max(0, (value / (vcpu * 1000)) * 100));
}

export function formatMetricAxisTime(value: string, range: MetricWindow) {
  const date = new Date(value);
  return range === '7d' ? DAY_FORMAT.format(date) : TIME_FORMAT.format(date);
}

export function getMetricSeriesKey(nodeId: string, metric: MetricName) {
  return `${nodeId}:${metric}`;
}

export function buildMetricChartRows(nodes: ValkeyMetricNode[], vcpu: number): MetricChartRow[] {
  const rows = new Map<string, MetricChartRow>();

  for (const node of nodes) {
    for (const point of node.points) {
      const row: MetricChartRow = rows.get(point.collectedAt) ?? {
        collectedAt: point.collectedAt,
      };

      for (const metric of Object.keys(point) as Array<keyof MetricPoint>) {
        if (metric !== 'collectedAt') {
          row[getMetricSeriesKey(node.id, metric)] = toDisplayMetricValue(
            metric,
            point[metric],
            vcpu
          );
        }
      }

      rows.set(point.collectedAt, row);
    }
  }

  return [...rows.values()].sort((left, right) =>
    left.collectedAt.localeCompare(right.collectedAt)
  );
}

export function getAuditGroupLabel(value: string | Date, now = new Date()) {
  const date = value instanceof Date ? value : new Date(value);
  const todayKey = localDateKey(now);
  const yesterday = new Date(now);
  yesterday.setDate(yesterday.getDate() - 1);

  if (localDateKey(date) === todayKey) {
    return 'Сегодня';
  }

  if (localDateKey(date) === localDateKey(yesterday)) {
    return 'Вчера';
  }

  return date.getFullYear() === now.getFullYear()
    ? DAY_FORMAT.format(date)
    : DAY_WITH_YEAR_FORMAT.format(date);
}

export function groupAuditLogs(items: AuditLogEntry[], now = new Date()): AuditLogGroup[] {
  const groups = new Map<string, AuditLogGroup>();

  for (const item of items) {
    const key = localDateKey(item.createdAt);
    const group = groups.get(key);

    if (group) {
      group.items.push(item);
    } else {
      groups.set(key, { key, label: getAuditGroupLabel(item.createdAt, now), items: [item] });
    }
  }

  return [...groups.values()];
}
