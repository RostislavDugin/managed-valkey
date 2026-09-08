import { useId, useState, type FocusEvent, type KeyboardEvent } from 'react';
import type { DotItemDotProps } from 'recharts';
import { AreaChart } from '@mantine/charts';
import { Paper, Stack, Text, Title } from '@mantine/core';
import { formatDateTime, formatRelativeTime } from '@/shared/lib';
import {
  formatMetricAxisTime,
  formatMetricValue,
  getMetricSeriesKey,
  type MetricChartRow,
  type MetricName,
  type MetricWindow,
  type ValkeyMetricNode,
} from '../model/valkey-observability';
import styles from './ValkeyMonitoringPage.module.css';

const NODE_COLORS = ['var(--h3-chart-1)', 'var(--h3-chart-2)', 'var(--h3-chart-3)'];

interface ChartSeries {
  color: string;
  label: string;
  name: string;
}

interface TooltipItem {
  color?: string;
  dataKey?: unknown;
  name?: string | number;
  value?: unknown;
}

function MetricsTooltip({
  label,
  labels,
  metrics,
  payload,
}: {
  label?: unknown;
  labels: Map<string, string>;
  metrics: Map<string, MetricName>;
  payload?: readonly TooltipItem[];
}) {
  if (typeof label !== 'string' || !payload?.length) {
    return null;
  }

  const values = payload.filter((item) => typeof item.value === 'number');
  if (values.length === 0) {
    return null;
  }

  return (
    <Paper className={styles.tooltip} p="h3_sm" shadow="h3_default">
      <Stack gap="h3_xs">
        <div>
          <Text fw="var(--h3-fw-medium)" size="h3_sm">
            {formatDateTime(label)}
          </Text>
          <Text c="h3_text_2" size="h3_xs">
            {formatRelativeTime(label)}
          </Text>
        </div>
        {values.map((item) => {
          const key = String(item.dataKey ?? item.name);
          const metric = metrics.get(key);
          return metric ? (
            <div className={styles.tooltipRow} key={key}>
              <span className={styles.tooltipSwatch} style={{ background: item.color }} />
              <Text size="h3_xs">{labels.get(key) ?? key}</Text>
              <Text fw="var(--h3-fw-medium)" size="h3_xs">
                {formatMetricValue(metric, item.value as number)}
              </Text>
            </div>
          ) : null;
        })}
      </Stack>
    </Paper>
  );
}

function useKeyboardPoints({
  data,
  labels,
  metrics,
  pointSeriesName,
  series,
  title,
}: {
  data: MetricChartRow[];
  labels: Map<string, string>;
  metrics: Map<string, MetricName>;
  pointSeriesName?: string;
  series: ChartSeries[];
  title: string;
}) {
  const tooltipId = useId();
  const [focusedIndex, setFocusedIndex] = useState<number | null>(null);
  const indexes = pointSeriesName
    ? data.flatMap((row, index) => (typeof row[pointSeriesName] === 'number' ? [index] : []))
    : [];
  const tabStop = focusedIndex ?? indexes.at(-1) ?? -1;

  const move = (event: KeyboardEvent<SVGCircleElement>, index: number) => {
    const position = indexes.indexOf(index);
    const last = indexes.length - 1;
    const next =
      event.key === 'Home'
        ? 0
        : event.key === 'End'
          ? last
          : event.key === 'ArrowLeft' || event.key === 'ArrowDown'
            ? Math.max(0, position - 1)
            : event.key === 'ArrowRight' || event.key === 'ArrowUp'
              ? Math.min(last, position + 1)
              : null;

    if (next === null || next === position) {
      return;
    }

    event.preventDefault();
    event.currentTarget
      .closest('section')
      ?.querySelector<SVGCircleElement>(`[data-keyboard-point="${indexes[next]}"]`)
      ?.focus();
  };

  const blur = (event: FocusEvent<SVGCircleElement>) => {
    const section = event.currentTarget.closest('section');
    queueMicrotask(() => {
      if (!section?.contains(document.activeElement)) {
        setFocusedIndex(null);
      }
    });
  };

  const renderDot = (props: DotItemDotProps) => {
    if (typeof props.cx !== 'number' || typeof props.cy !== 'number') {
      return null;
    }

    const row = data[props.index];
    const values = series
      .flatMap((item) => {
        const metric = metrics.get(item.name);
        const value = row?.[item.name];
        return metric && typeof value === 'number'
          ? [`${item.label}: ${formatMetricValue(metric, value)}`]
          : [];
      })
      .join('. ');

    return (
      <circle
        aria-describedby={tooltipId}
        aria-label={`${title}. ${formatDateTime(String(row.collectedAt))}. ${values}`}
        className={styles.keyboardPoint}
        cx={props.cx}
        cy={props.cy}
        data-keyboard-point={props.index}
        fill={series[0]?.color}
        onBlur={blur}
        onFocus={() => setFocusedIndex(props.index)}
        onKeyDown={(event) => move(event, props.index)}
        r={10}
        stroke="var(--h3-bg)"
        tabIndex={props.index === tabStop ? 0 : -1}
      />
    );
  };

  const payload =
    focusedIndex === null
      ? []
      : series.map((item) => ({
          color: item.color,
          dataKey: item.name,
          value: data[focusedIndex]?.[item.name],
        }));

  return {
    renderDot,
    tooltip:
      focusedIndex === null ? null : (
        <div aria-live="polite" className={styles.keyboardTooltip} id={tooltipId} role="status">
          <MetricsTooltip
            label={data[focusedIndex]?.collectedAt}
            labels={labels}
            metrics={metrics}
            payload={payload}
          />
        </div>
      ),
  };
}

interface MetricChartProps {
  data: MetricChartRow[];
  maximumValue?: number;
  metric: MetricName;
  nodes: ValkeyMetricNode[];
  range: MetricWindow;
  syncId: string;
  title: string;
}

export function MetricChart({
  data,
  maximumValue,
  metric,
  nodes,
  range,
  syncId,
  title,
}: MetricChartProps) {
  const series = nodes.map((node, index) => ({
    color: NODE_COLORS[index],
    label: node.name,
    name: getMetricSeriesKey(node.id, metric),
  }));
  const labels = new Map(series.map((item) => [item.name, item.label]));
  const metrics = new Map(series.map((item) => [item.name, metric]));
  const keyboard = useKeyboardPoints({
    data,
    labels,
    metrics,
    pointSeriesName: series[0]?.name,
    series,
    title,
  });

  return (
    <section className={styles.chartCard}>
      <Title order={3}>{title}</Title>
      <AreaChart
        areaChartProps={{
          accessibilityLayer: false,
          margin: { left: 4, right: 12, top: 8 },
          syncId,
        }}
        areaProps={(item) => ({
          activeDot: { fill: 'var(--h3-bg)', r: 4, stroke: item.color, strokeWidth: 2 },
          dot: item.name === series[0]?.name ? keyboard.renderDot : false,
        })}
        className={styles.chart}
        connectNulls={false}
        curveType="monotone"
        data={data}
        dataKey="collectedAt"
        fillOpacity={0.22}
        gridAxis="xy"
        series={series}
        strokeWidth={2}
        tooltipProps={{
          content: ({ label, payload }) => (
            <MetricsTooltip label={label} labels={labels} metrics={metrics} payload={payload} />
          ),
        }}
        valueFormatter={(value) => formatMetricValue(metric, value)}
        withDots={false}
        withGradient
        xAxisProps={{
          padding: { left: 10, right: 10 },
          tickFormatter: (value) => formatMetricAxisTime(String(value), range),
        }}
        yAxisProps={
          metric === 'cpuMillicores'
            ? { domain: [0, 100], ticks: [0, 25, 50, 75, 100], width: 48 }
            : metric === 'usedMemoryBytes' && maximumValue !== undefined
              ? { domain: [0, maximumValue], width: 72 }
              : { width: 72 }
        }
      />
      {keyboard.tooltip}
    </section>
  );
}
