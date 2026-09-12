export const METRIC_WINDOWS = ['5m', '1h', '24h', '7d'] as const;

export type MetricWindow = (typeof METRIC_WINDOWS)[number];
export type ValkeyNodeRole = 'primary' | 'replica' | 'unknown';

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
  ordinal: number;
  name: string;
  role: ValkeyNodeRole;
  points: MetricPoint[];
}

export interface ValkeyMetricsResponse {
  from: string;
  to: string;
  stepSeconds: number;
  nodes: ValkeyMetricNode[];
}

export type MetricChartRow = { collectedAt: string } & Record<string, string | number | null>;

export const METRIC_WINDOW_CONFIG: Record<MetricWindow, { label: string; stepMs: number }> = {
  '5m': { label: '5 минут', stepMs: 10_000 },
  '1h': { label: '1 час', stepMs: 60_000 },
  '24h': { label: '24 часа', stepMs: 300_000 },
  '7d': { label: '7 дней', stepMs: 1_800_000 },
};

const NUMBER_FORMAT = new Intl.NumberFormat('ru-RU', { maximumFractionDigits: 1 });
const MEMORY_FORMAT = new Intl.NumberFormat('ru-RU', { maximumFractionDigits: 2 });
const INTEGER_FORMAT = new Intl.NumberFormat('ru-RU', { maximumFractionDigits: 0 });
const TIME_FORMAT = new Intl.DateTimeFormat('ru-RU', {
  hour: '2-digit',
  minute: '2-digit',
});
const DAY_FORMAT = new Intl.DateTimeFormat('ru-RU', { day: 'numeric', month: 'short' });
const NBSP = '\u00A0';
const GIBIBYTE = 1024 ** 3;
const MAX_RENDER_GAP_MS = 60_000;

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

export function getMetricRenderSeriesKey(nodeId: string, metric: MetricName) {
  return `${getMetricSeriesKey(nodeId, metric)}:render`;
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

export function buildMetricRenderRows(
  rows: MetricChartRow[],
  nodes: ValkeyMetricNode[],
  metric: MetricName
): MetricChartRow[] {
  const renderRows = rows.map((row) => ({ ...row }));

  for (const node of nodes) {
    const valueKey = getMetricSeriesKey(node.id, metric);
    const renderKey = getMetricRenderSeriesKey(node.id, metric);

    for (const row of renderRows) {
      row[renderKey] = typeof row[valueKey] === 'number' ? row[valueKey] : null;
    }

    let index = 0;
    while (index < renderRows.length) {
      if (typeof renderRows[index]?.[valueKey] === 'number') {
        index += 1;
        continue;
      }

      const gapStart = index;
      while (index < renderRows.length && typeof renderRows[index]?.[valueKey] !== 'number') {
        index += 1;
      }

      const leftIndex = gapStart - 1;
      const rightIndex = index;
      if (leftIndex < 0 || rightIndex >= renderRows.length) {
        continue;
      }

      const timestamps = renderRows
        .slice(leftIndex, rightIndex + 1)
        .map((row) => Date.parse(row.collectedAt));
      const timestampsIncrease = timestamps.every(
        (timestamp, timestampIndex) =>
          Number.isFinite(timestamp) &&
          (timestampIndex === 0 || timestamp > (timestamps[timestampIndex - 1] ?? timestamp))
      );
      const elapsed = (timestamps.at(-1) ?? 0) - (timestamps[0] ?? 0);
      const leftValue = renderRows[leftIndex]?.[valueKey];
      const rightValue = renderRows[rightIndex]?.[valueKey];

      if (
        !timestampsIncrease ||
        elapsed > MAX_RENDER_GAP_MS ||
        typeof leftValue !== 'number' ||
        typeof rightValue !== 'number'
      ) {
        continue;
      }

      for (let gapIndex = gapStart; gapIndex < rightIndex; gapIndex += 1) {
        const offset = (timestamps[gapIndex - leftIndex] ?? 0) - (timestamps[0] ?? 0);
        renderRows[gapIndex]![renderKey] =
          leftValue + ((rightValue - leftValue) * offset) / elapsed;
      }
    }
  }

  return renderRows;
}
