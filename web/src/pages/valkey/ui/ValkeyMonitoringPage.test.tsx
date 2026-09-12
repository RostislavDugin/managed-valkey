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
const POINT_COUNTS: Record<MetricWindow, number> = { '5m': 30, '1h': 60, '24h': 288, '7d': 336 };

vi.mock('../api/valkey-metrics', () => ({ getValkeyMetrics: vi.fn() }));
vi.mock('./ValkeyInstanceLayout', () => ({
  useValkeyInstance: () => ({
    instance: { id: valkeyContext.instanceId, ramGb: 8, vcpu: 2 },
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
    series: Array<{ name: string; rawName?: string }>;
    strokeDasharray?: string;
    tooltipProps: {
      content: (props: { label: string; payload: unknown[] }) => ReactNode;
    };
    yAxisProps: { domain?: number[] };
  }) {
    const [tooltipIndex, setTooltipIndex] = useState<number | null>(null);
    const firstSeries = props.series[0];
    const dot = firstSeries ? props.areaProps(firstSeries).dot : false;
    const seriesGaps = Object.fromEntries(
      props.series.map((series) => [
        series.name,
        props.data.some((row) => row[series.name] === null),
      ])
    );
    const tooltipRow = tooltipIndex === null ? undefined : props.data[tooltipIndex];

    return (
      <div
        data-connect-nulls={String(props.connectNulls)}
        data-curve={props.curveType}
        data-has-gap={String(Boolean(firstSeries && seriesGaps[firstSeries.name]))}
        data-points={props.data.length}
        data-series-gaps={JSON.stringify(seriesGaps)}
        data-series={JSON.stringify(props.series)}
        data-stroke-dasharray={props.strokeDasharray}
        data-sync-id={props.areaChartProps.syncId}
        data-testid="area-chart"
        data-y-domain={JSON.stringify(props.yAxisProps.domain)}
        onDoubleClick={() =>
          setTooltipIndex(
            props.data.findIndex(
              (row) =>
                firstSeries?.rawName &&
                row[firstSeries.rawName] === null &&
                typeof row[firstSeries.name] === 'number'
            )
          )
        }
        onMouseEnter={() => setTooltipIndex(props.data.length - 1)}
        onMouseLeave={() => setTooltipIndex(null)}
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
        {tooltipRow && firstSeries
          ? props.tooltipProps.content({
              label: String(tooltipRow.collectedAt),
              payload: props.series.map((series) => ({
                color: 'green',
                dataKey: series.name,
                name: series.name,
                value: tooltipRow[series.name],
              })),
            })
          : null}
      </div>
    );
  }

  return { AreaChart };
});

function point(range: MetricWindow, index: number, value: number): MetricPoint {
  const step = METRIC_WINDOW_CONFIG[range].stepMs;
  return {
    collectedAt: new Date(Date.UTC(2026, 8, 8) + index * step).toISOString(),
    usedMemoryBytes: index === 1 ? null : value * 1024 * 1024,
    cpuMillicores: value,
    connectedClients: value,
    opsPerSec: value,
    keyspaceHits: value,
    keyspaceMisses: value,
    evictedKeys: value,
  };
}

function response(range: MetricWindow, value = POINT_COUNTS[range]): ValkeyMetricsResponse {
  const points = Array.from({ length: POINT_COUNTS[range] }, (_, index) =>
    point(range, index, value)
  );

  return {
    from: points[0]?.collectedAt ?? '2026-09-08T00:00:00.000Z',
    to: new Date(Date.UTC(2026, 8, 8) + POINT_COUNTS[range] * 10_000).toISOString(),
    stepSeconds: METRIC_WINDOW_CONFIG[range].stepMs / 1000,
    nodes: [
      { id: '0', ordinal: 0, name: 'valkey-0', role: 'primary', points },
      { id: '1', ordinal: 1, name: 'valkey-1', role: 'replica', points },
      { id: '2', ordinal: 2, name: 'valkey-2', role: 'unknown', points },
    ],
  };
}

function gapPoint(collectedAt: string, value: number | null): MetricPoint {
  return {
    collectedAt,
    usedMemoryBytes: value,
    cpuMillicores: value,
    connectedClients: value,
    opsPerSec: value,
    keyspaceHits: value,
    keyspaceMisses: value,
    evictedKeys: value,
  };
}

function responseWithGap(elapsedMs: number, replicaGapIsUnbounded = false): ValkeyMetricsResponse {
  const startedAt = Date.UTC(2026, 8, 8, 12);
  const timestamps = [0, 30_000, elapsedMs].map((offset) =>
    new Date(startedAt + offset).toISOString()
  );
  const primaryPoints = [
    gapPoint(timestamps[0]!, 10),
    gapPoint(timestamps[1]!, null),
    gapPoint(timestamps[2]!, 30),
  ];
  const replicaPoints = [
    gapPoint(timestamps[0]!, 40),
    gapPoint(timestamps[1]!, null),
    gapPoint(timestamps[2]!, replicaGapIsUnbounded ? null : 60),
  ];

  return {
    from: timestamps[0]!,
    to: timestamps[2]!,
    stepSeconds: 10,
    nodes: [
      { id: '0', ordinal: 0, name: 'valkey-0', role: 'primary', points: primaryPoints },
      { id: '1', ordinal: 1, name: 'valkey-1', role: 'replica', points: replicaPoints },
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
  vi.mocked(getValkeyMetrics).mockReset();
  vi.mocked(getValkeyMetrics).mockImplementation(({ range }) => Promise.resolve(response(range)));
});

describe('экран мониторинга', () => {
  it('после загрузки метрик показывает сводку, четыре графика, единицы измерения, роли нод и соединение короткого пропуска', async () => {
    renderPage();

    expect(await screen.findAllByTestId('area-chart')).toHaveLength(4);
    expect(screen.getAllByText(/30\sоп\/\u0441/)).toHaveLength(3);
    expect(screen.getAllByRole('heading', { name: 'Подключения' })).toHaveLength(2);
    expect(screen.getByRole('radio', { name: '5 минут' })).toBeChecked();
    expect(screen.getByRole('button', { name: /valkey-0, primary, показана/ })).toBeVisible();
    expect(screen.getByRole('button', { name: /valkey-1, реплика, показана/ })).toBeVisible();
    expect(
      screen.getByRole('button', { name: /valkey-2, роль неизвестна, показана/ })
    ).toBeVisible();

    for (const chart of screen.getAllByTestId('area-chart')) {
      expect(chart).toHaveAttribute('data-connect-nulls', 'false');
      expect(chart).toHaveAttribute('data-curve', 'monotone');
      expect(chart).toHaveAttribute('data-sync-id', 'valkey-metrics-instance-1');
      expect(chart).not.toHaveAttribute('data-stroke-dasharray');
    }
    expect(screen.getAllByTestId('area-chart')[0]).toHaveAttribute('data-has-gap', 'false');
    expect(screen.getAllByTestId('area-chart')[0]).toHaveAttribute(
      'data-y-domain',
      '[0,8589934592]'
    );
    expect(screen.getAllByTestId('area-chart')[1]).toHaveAttribute('data-y-domain', '[0,100]');
  });

  it('если известные значения разделяет не больше минуты, соединяет пропуск на всех четырёх графиках только при отрисовке', async () => {
    const metrics = responseWithGap(60_000);
    vi.mocked(getValkeyMetrics).mockResolvedValue(metrics);
    renderPage();

    const charts = await screen.findAllByTestId('area-chart');
    for (const chart of charts) {
      expect(chart).toHaveAttribute('data-has-gap', 'false');
      expect(chart.querySelectorAll('[data-keyboard-point]')).toHaveLength(2);
    }

    fireEvent.doubleClick(charts[0]!);
    expect(screen.queryByText(formatDateTime(metrics.nodes[0]!.points[1]!.collectedAt))).toBeNull();
  });

  it('если известные значения разделяет больше минуты, оставляет разрыв на всех четырёх графиках', async () => {
    vi.mocked(getValkeyMetrics).mockResolvedValue(responseWithGap(60_001));
    renderPage();

    for (const chart of await screen.findAllByTestId('area-chart')) {
      expect(chart).toHaveAttribute('data-has-gap', 'true');
    }
  });

  it('для каждой ноды независимо соединяет только ограниченный с двух сторон минутный пропуск', async () => {
    vi.mocked(getValkeyMetrics).mockResolvedValue(responseWithGap(60_000, true));
    renderPage();

    for (const chart of await screen.findAllByTestId('area-chart')) {
      expect(Object.values(JSON.parse(chart.dataset.seriesGaps ?? '{}'))).toEqual([false, true]);
    }
  });

  it('при первом ответе в StrictMode выбирает все ноды и не теряет выбор после повторного эффекта', async () => {
    render(<StrictMode>{withProviders(<ValkeyMonitoringPage />)}</StrictMode>);

    expect(
      await screen.findByRole('button', { name: /valkey-0, primary, показана/ })
    ).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getAllByRole('button', { name: /показана/ })).toHaveLength(3);
  });

  it('при управлении с клавиатуры меняет видимость ноды и сохраняет выбранные ноды после смены окна', async () => {
    const user = userEvent.setup();
    renderPage();
    const replica = await screen.findByRole('button', { name: /valkey-1, реплика, показана/ });

    replica.focus();
    await user.keyboard('{Enter}');
    await user.click(screen.getByText('7 дней'));

    expect(
      await screen.findByRole('button', { name: /valkey-1, реплика, скрыта/ })
    ).toHaveAttribute('aria-pressed', 'false');
    expect(screen.getAllByTestId('area-chart')[0]).toHaveAttribute('data-points', '336');
  });

  it('при работе с графиком открывает подсказку мышью и перемещает фокус между точками с клавиатуры', async () => {
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
  ] as const)(
    'после выбора периода %s загружает окно %s и показывает ожидаемое число точек',
    async (label, range, count) => {
      const user = userEvent.setup();
      renderPage();
      await screen.findAllByTestId('area-chart');

      await user.click(screen.getByText(label));

      await waitFor(() =>
        expect(getValkeyMetrics).toHaveBeenLastCalledWith(expect.objectContaining({ range }))
      );
      expect(screen.getAllByTestId('area-chart')[0]).toHaveAttribute('data-points', String(count));
    }
  );

  it('если сервер не вернул ни одной ноды, показывает отдельное пустое состояние без графиков', async () => {
    vi.mocked(getValkeyMetrics).mockResolvedValue({
      from: '2026-09-09T10:00:00Z',
      to: '2026-09-09T10:05:00Z',
      stepSeconds: 10,
      nodes: [],
    });

    renderPage();

    expect(await screen.findByRole('heading', { name: 'Метрик пока нет' })).toBeVisible();
  });

  it('после пустого первого ответа автоматически выбирает ноды, появившиеся при следующей загрузке', async () => {
    vi.mocked(getValkeyMetrics)
      .mockResolvedValueOnce({
        from: '2026-09-09T10:00:00Z',
        to: '2026-09-09T10:05:00Z',
        stepSeconds: 10,
        nodes: [],
      })
      .mockResolvedValueOnce(response('1h'));
    const user = userEvent.setup();

    renderPage();
    expect(await screen.findByText('Метрик пока нет')).toBeVisible();
    await user.click(screen.getByText('1 час'));

    expect(
      await screen.findByRole('button', { name: /valkey-0, primary, показана/ })
    ).toHaveAttribute('aria-pressed', 'true');
  });

  it('после ошибки загрузки показывает сообщение и по кнопке повторяет запрос метрик', async () => {
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

  it('после смены окна отбрасывает поздний ответ предыдущего запроса и показывает актуальные метрики', async () => {
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
    expect(await screen.findAllByText(/5\sоп\/\u0441/)).toHaveLength(3);
    await user.click(screen.getByText('1 час'));
    await user.click(screen.getByText('24 часа'));

    await act(async () => hour.resolve(response('1h', 60)));
    expect(screen.queryAllByText(/60\sоп\/\u0441/)).toHaveLength(0);
    await act(async () => day.resolve(response('24h', 24)));
    expect(await screen.findAllByText(/24\sоп\/\u0441/)).toHaveLength(3);
  });

  it('при периодическом обновлении запрашивает метрики каждые пять секунд и не создаёт параллельных запросов', async () => {
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
