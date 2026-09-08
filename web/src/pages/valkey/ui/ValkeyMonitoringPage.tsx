import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Check, RotateCw } from 'lucide-react';
import {
  Alert,
  Button,
  Container,
  Group,
  Loader,
  SegmentedControl,
  Skeleton,
  Stack,
  Text,
  Title,
} from '@mantine/core';
import { buttonVariants } from '@/shared/config';
import { getValkeyMetrics } from '../api/valkey-metrics';
import { getRequestErrorMessage } from '../model/valkey-form';
import {
  buildMetricChartRows,
  formatMetricValue,
  METRIC_WINDOWS,
  METRIC_WINDOW_CONFIG,
  toDisplayMetricValue,
  type MetricName,
  type MetricWindow,
  type ValkeyMetricNode,
  type ValkeyMetricsResponse,
} from '../model/valkey-observability';
import { MetricChart } from './MetricChart';
import { useValkeyInstance } from './ValkeyInstanceLayout';
import styles from './ValkeyMonitoringPage.module.css';
import pageStyles from './ValkeyPage.module.css';

const NODE_COLORS = ['var(--h3-chart-1)', 'var(--h3-chart-2)', 'var(--h3-chart-3)'];
const ROLE_LABELS = { primary: 'primary', replica: 'реплика' } as const;
const METRICS_REFRESH_INTERVAL_MS = 5_000;
const BYTES_PER_GIBIBYTE = 1024 ** 3;
const SUMMARY_METRICS: Array<{ metric: MetricName; title: string }> = [
  { metric: 'usedMemoryBytes', title: 'Память' },
  { metric: 'cpuMillicores', title: 'CPU' },
  { metric: 'connectedClients', title: 'Подключения' },
  { metric: 'opsPerSec', title: 'Операции' },
];

function latestValue(node: ValkeyMetricNode, metric: MetricName) {
  for (let index = node.points.length - 1; index >= 0; index -= 1) {
    const value = node.points[index][metric];
    if (value !== null) {
      return value;
    }
  }

  return null;
}

function SummaryCard({
  metric,
  nodes,
  title,
  vcpu,
}: {
  metric: MetricName;
  nodes: ValkeyMetricNode[];
  title: string;
  vcpu: number;
}) {
  return (
    <section className={styles.summaryCard}>
      <Stack gap="h3_sm">
        <Title order={3}>{title}</Title>
        <div className={styles.summaryValues}>
          {nodes.map((node) => (
            <div className={styles.summaryRow} key={node.id}>
              <Text c="h3_text_2" size="h3_sm">
                {node.name}
              </Text>
              <Text fw="var(--h3-fw-medium)" size="h3_sm">
                {formatMetricValue(
                  metric,
                  toDisplayMetricValue(metric, latestValue(node, metric), vcpu)
                )}
              </Text>
            </div>
          ))}
        </div>
      </Stack>
    </section>
  );
}

function ValkeyMonitoringContent({ context }: { context: ReturnType<typeof useValkeyInstance> }) {
  const { instance, session } = context;
  const [range, setRange] = useState<MetricWindow>('5m');
  const [response, setResponse] = useState<ValkeyMetricsResponse | null>(null);
  const [visibleNodeIds, setVisibleNodeIds] = useState<string[]>([]);
  const [error, setError] = useState<Error | null>(null);
  const [loading, setLoading] = useState(true);
  const [revision, setRevision] = useState(0);
  const requestIdRef = useRef(0);
  const visibilityInitializedRef = useRef(false);

  useEffect(() => {
    const controller = new AbortController();
    let refreshTimer: ReturnType<typeof setTimeout> | undefined;
    let stopped = false;

    const load = async () => {
      const startedAt = Date.now();
      const requestId = requestIdRef.current + 1;
      requestIdRef.current = requestId;
      setLoading(true);
      setError(null);

      try {
        const nextResponse = await getValkeyMetrics({
          ownerId: session.userId,
          instanceId: instance.id,
          range,
          signal: controller.signal,
        });
        if (stopped || requestIdRef.current !== requestId) {
          return;
        }

        setResponse(nextResponse);
        const available = new Set(nextResponse.nodes.map((node) => node.id));
        if (!visibilityInitializedRef.current) {
          visibilityInitializedRef.current = true;
          setVisibleNodeIds([...available]);
        } else {
          setVisibleNodeIds((current) => current.filter((id) => available.has(id)));
        }
      } catch (requestError: unknown) {
        if (
          stopped ||
          requestIdRef.current !== requestId ||
          (requestError instanceof DOMException && requestError.name === 'AbortError')
        ) {
          return;
        }

        setError(requestError instanceof Error ? requestError : new Error(String(requestError)));
      } finally {
        if (requestIdRef.current === requestId) {
          setLoading(false);
        }

        if (!stopped) {
          const elapsed = Date.now() - startedAt;
          refreshTimer = setTimeout(
            () => void load(),
            Math.max(0, METRICS_REFRESH_INTERVAL_MS - elapsed)
          );
        }
      }
    };

    void load();

    return () => {
      stopped = true;
      clearTimeout(refreshTimer);
      controller.abort();
    };
  }, [instance.id, range, revision, session.userId]);

  const visibleNodes = useMemo(
    () => response?.nodes.filter((node) => visibleNodeIds.includes(node.id)) ?? [],
    [response, visibleNodeIds]
  );
  const chartRows = useMemo(
    () => buildMetricChartRows(visibleNodes, instance.vcpu),
    [instance.vcpu, visibleNodes]
  );
  const toggleNode = useCallback((nodeId: string) => {
    setVisibleNodeIds((current) =>
      current.includes(nodeId) ? current.filter((id) => id !== nodeId) : [...current, nodeId]
    );
  }, []);
  const retry = useCallback(() => setRevision((current) => current + 1), []);
  const isEmpty = response !== null && response.nodes.every((node) => node.points.length === 0);
  const syncId = `valkey-metrics-${instance.id}`;

  return (
    <Container
      className={`${pageStyles.page} ${pageStyles.instanceContent} ${styles.page}`}
      component="section"
      fluid
    >
      <Stack gap="h3_lg">
        <div className={styles.toolbar}>
          <Group gap="h3_sm">
            <Title order={2}>Мониторинг</Title>
            {loading && response ? <Loader aria-label="Обновление метрик" size="xs" /> : null}
          </Group>

          <SegmentedControl
            aria-label="Окно метрик"
            data={METRIC_WINDOWS.map((value) => ({
              value,
              label: METRIC_WINDOW_CONFIG[value].label,
            }))}
            onChange={(value) => setRange(value as MetricWindow)}
            value={range}
          />
        </div>

        {response ? (
          <div aria-label="Видимость нод" className={styles.nodeControls} role="group">
            {response.nodes.map((node, index) => {
              const selected = visibleNodeIds.includes(node.id);
              return (
                <Button
                  aria-pressed={selected}
                  className={styles.nodeButton}
                  key={node.id}
                  leftSection={
                    selected ? (
                      <Check aria-hidden="true" size={16} strokeWidth={2} />
                    ) : (
                      <span
                        aria-hidden="true"
                        className={styles.nodeColor}
                        style={{ background: NODE_COLORS[index] }}
                      />
                    )
                  }
                  onClick={() => toggleNode(node.id)}
                  variant="default"
                >
                  {node.name}, {ROLE_LABELS[node.role]}, {selected ? 'показана' : 'скрыта'}
                </Button>
              );
            })}
          </div>
        ) : null}

        {error ? (
          <Alert color="red" title="Не удалось загрузить метрики">
            <Stack align="flex-start" gap="h3_sm">
              <Text size="h3_sm">{getRequestErrorMessage(error)}</Text>
              <Button
                leftSection={<RotateCw aria-hidden="true" size={16} strokeWidth={1.5} />}
                onClick={retry}
                variant={buttonVariants.secondary}
              >
                Повторить
              </Button>
            </Stack>
          </Alert>
        ) : null}

        {!response && loading ? (
          <Stack aria-label="Загрузка метрик" className={styles.state} gap="h3_md">
            <Skeleton height={96} />
            <Skeleton height={272} />
            <Skeleton height={272} />
          </Stack>
        ) : null}

        {isEmpty ? (
          <Stack className={styles.state} gap="h3_sm">
            <Title order={3}>Измерений пока нет</Title>
            <Text c="h3_text_2">Попробуйте выбрать другое окно или обновить данные позже.</Text>
          </Stack>
        ) : null}

        {response && !isEmpty && visibleNodes.length === 0 ? (
          <Text c="h3_text_2">Выберите хотя бы один инстанс, чтобы увидеть метрики.</Text>
        ) : null}

        {response && !isEmpty && visibleNodes.length > 0 ? (
          <>
            <div className={styles.summaryGrid}>
              {SUMMARY_METRICS.map((item) => (
                <SummaryCard
                  key={item.metric}
                  nodes={visibleNodes}
                  vcpu={instance.vcpu}
                  {...item}
                />
              ))}
            </div>

            <div className={styles.chartsGrid}>
              <MetricChart
                data={chartRows}
                maximumValue={instance.ramGb * BYTES_PER_GIBIBYTE}
                metric="usedMemoryBytes"
                nodes={visibleNodes}
                range={range}
                syncId={syncId}
                title="Использованная память"
              />
              <MetricChart
                data={chartRows}
                metric="cpuMillicores"
                nodes={visibleNodes}
                range={range}
                syncId={syncId}
                title="CPU"
              />
              <MetricChart
                data={chartRows}
                metric="connectedClients"
                nodes={visibleNodes}
                range={range}
                syncId={syncId}
                title="Подключения"
              />
              <MetricChart
                data={chartRows}
                metric="opsPerSec"
                nodes={visibleNodes}
                range={range}
                syncId={syncId}
                title="Операции в секунду"
              />
            </div>
          </>
        ) : null}
      </Stack>
    </Container>
  );
}

export function ValkeyMonitoringPage() {
  const context = useValkeyInstance();
  return <ValkeyMonitoringContent context={context} key={context.instance.id} />;
}
