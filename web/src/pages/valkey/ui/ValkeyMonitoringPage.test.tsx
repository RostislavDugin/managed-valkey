import { act, StrictMode, type ReactNode } from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { formatDateTime, formatRelativeTime } from '@/shared/lib';
import { withProviders } from '../../../../test/render';
import { getValkeyMetrics } from '../api/valkey-metrics';
import {
  METRIC_WINDOW_CONFIG,
  type MetricPoint,
  type MetricWindow,
  type ValkeyMetricsResponse,
} from '../model/valkey-observability';
import { ValkeyMonitoringPage } from './ValkeyMonitoringPage';

const valkeyContext = vi.hoisted(() => ({ instanceId: 'instance-1' }));

vi.mock('../api/valkey-metrics', () => ({ getValkeyMetrics: vi.fn() }));
vi.mock('./ValkeyInstanceLayout', () => ({
  useValkeyInstance: () => ({
    instance: { id: valkeyContext.instanceId, ramGb: 8, vcpu: 2 },
    session: { userId: 'owner-1' },
  }),
}));
vi.mock('@mantine/charts', async () => {
  const { useState } = await import('react');

  function AreaChart(props: {
    areaChartProps: { syncId: string };
    areaProps: (series: { name: string }) => {
      dot?: ((props: Record<string, unknown>) => ReactNode) | false;
    };
    connectNulls: boolean;
    curveType: string;
    data: Array<Record<string, unknown>>;
    series: Array<{ name: string }>;
    strokeDasharray?: string;
    tooltipProps: {
      content: (props: { label: string; payload: unknown[] }) => ReactNode;
    };
    yAxisProps: { domain?: number[] };
  }) {
    const [tooltipVisible, setTooltipVisible] = useState(false);
    const last = props.data.at(-1);
    const firstSeries = props.series[0];
    const dot = firstSeries ? props.areaProps(firstSeries).dot : false;

    return (
      <div
        data-connect-nulls={String(props.connectNulls)}
        data-curve={props.curveType}
        data-points={props.data.length}
        data-series={JSON.stringify(props.series)}
        data-stroke-dasharray={props.strokeDasharray}
        data-sync-id={props.areaChartProps.syncId}
        data-testid="area-chart"
        data-y-domain={JSON.stringify(props.yAxisProps.domain)}
        onMouseEnter={() => setTooltipVisible(true)}
        onMouseLeave={() => setTooltipVisible(false)}
      >
        <svg>
          {typeof dot === 'function'
            ? props.data.map((row, index) => (
                <g key={String(row.collectedAt)}>
                  {dot({
                    cx: index,
                    cy: 10,
                    dataKey: firstSeries?.name,
                    index,
                    payload: row,
                    points: [],
                    value: firstSeries ? row[firstSeries.name] : null,
                  })}
                </g>
              ))
            : null}
        </svg>
        {tooltipVisible && last && firstSeries
          ? props.tooltipProps.content({
              label: String(last.collectedAt),
              payload: [
                {
                  color: 'green',
                  dataKey: firstSeries.name,
                  name: firstSeries.name,
                  value: last[firstSeries.name],
                },
              ],
            })
          : null}
      </div>
    );
  }

  return {
    AreaChart,
  };
});

function point(range: MetricWindow, index: number, value: number): MetricPoint {
  const step = METRIC_WINDOW_CONFIG[range].stepMs;
  return {
    collectedAt: new Date(Date.UTC(2026, 8, 8) + index * step).toISOString(),
    usedMemoryBytes: value * 1024 * 1024,
    cpuMillicores: value,
    connectedClients: value,
    opsPerSec: value,
    keyspaceHits: value,
    keyspaceMisses: value,
    evictedKeys: value,
  };
}

function response(
  range: MetricWindow,
  value = METRIC_WINDOW_CONFIG[range].pointCount
): ValkeyMetricsResponse {
  const points = Array.from({ length: METRIC_WINDOW_CONFIG[range].pointCount }, (_, index) =>
    point(range, index, value)
  );

  return {
    source: 'demo',
    range,
    nodes: [
      { id: 'primary', name: 'valkey-0', role: 'primary', points },
      { id: 'replica-1', name: 'valkey-1', role: 'replica', points },
      { id: 'replica-2', name: 'valkey-2', role: 'replica', points },
    ],
  };
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((resolvePromise) => {
    resolve = resolvePromise;
  });
  return { promise, resolve };
}

function renderPage() {
  return render(withProviders(<ValkeyMonitoringPage />));
}

beforeEach(() => {
  valkeyContext.instanceId = 'instance-1';
  vi.mocked(getValkeyMetrics).mockImplementation(({ range }) => Promise.resolve(response(range)));
});

describe('экран мониторинга', () => {
  it('показывает сводку и четыре площадных графика со сплошными линиями', async () => {
    renderPage();

    expect(await screen.findAllByTestId('area-chart')).toHaveLength(4);
    expect(screen.queryByText('Демонстрационные данные')).not.toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'Ключи' })).not.toBeInTheDocument();
    expect(screen.getAllByRole('heading', { name: 'Подключения' })).toHaveLength(2);
    expect(screen.getByRole('radio', { name: '5 минут' })).toBeChecked();

    for (const chart of screen.getAllByTestId('area-chart')) {
      expect(chart).toHaveAttribute('data-connect-nulls', 'false');
      expect(chart).toHaveAttribute('data-curve', 'monotone');
      expect(chart).toHaveAttribute('data-sync-id', 'valkey-metrics-instance-1');
      expect(chart).not.toHaveAttribute('data-stroke-dasharray');
    }

    const series = screen.getAllByTestId('area-chart')[0].getAttribute('data-series') ?? '';
    expect(series).not.toContain('strokeDasharray');
    expect(screen.getAllByTestId('area-chart')[0]).toHaveAttribute(
      'data-y-domain',
      '[0,8589934592]'
    );
    expect(screen.getAllByTestId('area-chart')[1]).toHaveAttribute('data-y-domain', '[0,100]');

    const primary = screen.getByRole('button', { name: /valkey-0, primary, показана/ });
    expect(primary).toHaveAttribute('aria-pressed', 'true');
  });

  it('выбирает все ноды при первом запросе в StrictMode', async () => {
    render(<StrictMode>{withProviders(<ValkeyMonitoringPage />)}</StrictMode>);

    expect(
      await screen.findByRole('button', { name: /valkey-0, primary, показана/ })
    ).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getAllByRole('button', { name: /показана/ })).toHaveLength(3);
  });

  it('сбрасывает выбранные ноды при переходе к другой базе', async () => {
    const rendered = renderPage();
    expect(
      await screen.findByRole('button', { name: /valkey-0, primary, показана/ })
    ).toBeVisible();

    const nextResponse = response('5m');
    nextResponse.nodes = nextResponse.nodes.map((node, index) => ({
      ...node,
      id: `next-${node.id}`,
      name: `next-${index}`,
    }));
    vi.mocked(getValkeyMetrics).mockResolvedValueOnce(nextResponse);
    valkeyContext.instanceId = 'instance-2';
    rendered.rerender(withProviders(<ValkeyMonitoringPage />));

    expect(
      await screen.findByRole('button', { name: /next-0, primary, показана/ })
    ).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getAllByRole('button', { name: /показана/ })).toHaveLength(3);
  });

  it('меняет видимость ряда с клавиатуры и сохраняет выбор при смене окна', async () => {
    const user = userEvent.setup();
    renderPage();
    const replica = await screen.findByRole('button', { name: /valkey-1, реплика, показана/ });

    replica.focus();
    await user.keyboard('{Enter}');

    expect(screen.getByRole('button', { name: /valkey-1, реплика, скрыта/ })).toHaveAttribute(
      'aria-pressed',
      'false'
    );

    await user.click(screen.getByText('7 дней'));

    expect(
      await screen.findByRole('button', { name: /valkey-1, реплика, скрыта/ })
    ).toHaveAttribute('aria-pressed', 'false');
    expect(screen.getAllByTestId('area-chart')[0]).toHaveAttribute('data-points', '336');
  });

  it('открывает подсказку мышью и переводит фокус между настоящими точками', async () => {
    const user = userEvent.setup();
    renderPage();
    const charts = await screen.findAllByTestId('area-chart');
    const timestamp = response('5m').nodes[0].points.at(-1)!.collectedAt;

    await user.hover(charts[0]);
    expect(screen.getByText(formatDateTime(timestamp))).toBeVisible();
    expect(screen.getByText(formatRelativeTime(timestamp))).toBeVisible();

    await user.unhover(charts[0]);
    const points = [...charts[0].querySelectorAll<SVGCircleElement>('[data-keyboard-point]')];
    const firstPoint = points[0];
    const lastPoint = points.at(-1);
    const previousPoint = points.at(-2);
    expect(firstPoint).toBeDefined();
    expect(lastPoint).toBeDefined();
    expect(previousPoint).toBeDefined();
    fireEvent.focus(lastPoint as SVGCircleElement);
    expect(screen.getByText(formatDateTime(timestamp))).toBeVisible();

    const focusPrevious = vi.spyOn(previousPoint as SVGCircleElement, 'focus');
    const focusFirst = vi.spyOn(firstPoint as SVGCircleElement, 'focus');
    const focusLast = vi.spyOn(lastPoint as SVGCircleElement, 'focus');
    fireEvent.keyDown(lastPoint as SVGCircleElement, { key: 'ArrowLeft' });
    expect(focusPrevious).toHaveBeenCalledOnce();

    fireEvent.keyDown(previousPoint as SVGCircleElement, { key: 'ArrowRight' });
    expect(focusLast).toHaveBeenCalledOnce();

    fireEvent.keyDown(previousPoint as SVGCircleElement, { key: 'Home' });
    expect(focusFirst).toHaveBeenCalledOnce();

    fireEvent.keyDown(firstPoint as SVGCircleElement, { key: 'End' });
    expect(focusLast).toHaveBeenCalledTimes(2);
  });

  it.each([
    ['1 час', '1h', 60],
    ['24 часа', '24h', 288],
    ['7 дней', '7d', 336],
  ] as const)('загружает окно %s с %s точками', async (label, range, count) => {
    const user = userEvent.setup();
    renderPage();
    await screen.findAllByTestId('area-chart');

    await user.click(screen.getByText(label));

    await waitFor(() =>
      expect(getValkeyMetrics).toHaveBeenLastCalledWith(expect.objectContaining({ range }))
    );
    expect(screen.getAllByTestId('area-chart')[0]).toHaveAttribute('data-points', String(count));
  });

  it('показывает пустой ответ', async () => {
    vi.mocked(getValkeyMetrics).mockResolvedValue({ source: 'demo', range: '1h', nodes: [] });

    renderPage();

    expect(await screen.findByRole('heading', { name: 'Измерений пока нет' })).toBeVisible();
  });

  it('показывает ошибку и повторяет запрос', async () => {
    vi.mocked(getValkeyMetrics)
      .mockRejectedValueOnce(new Error('Сеть недоступна'))
      .mockResolvedValueOnce(response('5m'));
    const user = userEvent.setup();

    renderPage();
    expect(
      await screen.findByText('Не удалось выполнить запрос. Попробуйте ещё раз.')
    ).toBeVisible();

    await user.click(screen.getByRole('button', { name: 'Повторить' }));

    expect(await screen.findByRole('heading', { name: 'Использованная память' })).toBeVisible();
  });

  it('оставляет текущие данные при смене окна и отбрасывает поздний ответ', async () => {
    const hour = deferred<ValkeyMetricsResponse>();
    const day = deferred<ValkeyMetricsResponse>();
    vi.mocked(getValkeyMetrics).mockImplementation(({ range }) => {
      if (range === '5m') {
        return Promise.resolve(response('5m', 5));
      }
      if (range === '1h') {
        return hour.promise;
      }
      if (range === '24h') {
        return day.promise;
      }
      return Promise.resolve(response(range));
    });
    const user = userEvent.setup();

    renderPage();
    expect(await screen.findAllByText(/5\sоп\/с/)).toHaveLength(3);

    await user.click(screen.getByText('1 час'));
    expect(screen.getAllByText(/5\sоп\/с/)).toHaveLength(3);
    await user.click(screen.getByText('24 часа'));

    await act(async () => hour.resolve(response('1h', 60)));
    expect(screen.queryAllByText(/60\sоп\/с/)).toHaveLength(0);

    await act(async () => day.resolve(response('24h', 24)));
    expect(await screen.findAllByText(/24\sоп\/с/)).toHaveLength(3);
  });

  it('обновляет метрики раз в пять секунд без параллельных запросов', async () => {
    vi.useFakeTimers();
    const first = deferred<ValkeyMetricsResponse>();
    const second = deferred<ValkeyMetricsResponse>();
    vi.mocked(getValkeyMetrics)
      .mockReturnValueOnce(first.promise)
      .mockReturnValueOnce(second.promise);
    const rendered = renderPage();

    try {
      expect(getValkeyMetrics).toHaveBeenCalledTimes(1);
      await act(() => vi.advanceTimersByTimeAsync(10_000));
      expect(getValkeyMetrics).toHaveBeenCalledTimes(1);

      await act(async () => first.resolve(response('5m')));
      await act(() => vi.advanceTimersByTimeAsync(0));
      expect(getValkeyMetrics).toHaveBeenCalledTimes(2);

      await act(() => vi.advanceTimersByTimeAsync(10_000));
      expect(getValkeyMetrics).toHaveBeenCalledTimes(2);
      await act(async () => second.resolve(response('5m')));
    } finally {
      rendered.unmount();
      vi.useRealTimers();
    }
  });
});
