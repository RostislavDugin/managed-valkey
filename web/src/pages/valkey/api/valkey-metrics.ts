import {
  METRIC_WINDOW_CONFIG,
  type MetricName,
  type MetricPoint,
  type MetricWindow,
  type ValkeyMetricNode,
  type ValkeyMetricsResponse,
  type ValkeyNodeRole,
} from '../model/valkey-observability';
import { readInstanceSnapshot } from './valkey-storage';

const RESPONSE_DELAY_MS = 400;
const MEBIBYTE = 1024 * 1024;
const GIBIBYTE = 1024 * MEBIBYTE;

export interface GetValkeyMetricsInput {
  ownerId: string;
  instanceId: string;
  range: MetricWindow;
  signal: AbortSignal;
}

function abortError() {
  return new DOMException('Запрос отменён', 'AbortError');
}

function delay(signal: AbortSignal) {
  return new Promise<void>((resolve, reject) => {
    if (signal.aborted) {
      reject(abortError());
      return;
    }

    const timeout = setTimeout(() => {
      signal.removeEventListener('abort', abort);
      resolve();
    }, RESPONSE_DELAY_MS);
    const abort = () => {
      clearTimeout(timeout);
      reject(abortError());
    };

    signal.addEventListener('abort', abort, { once: true });
  });
}

function hash(value: string) {
  let result = 2166136261;

  for (let index = 0; index < value.length; index += 1) {
    result ^= value.charCodeAt(index);
    result = Math.imul(result, 16777619);
  }

  return result >>> 0;
}

function deterministicValue(seed: string, minimum: number, maximum: number) {
  return minimum + (hash(seed) / 0xffffffff) * (maximum - minimum);
}

function smoothMetricValue(
  nodeSeed: string,
  metric: MetricName,
  timestamp: number,
  stepMs: number,
  minimum: number,
  maximum: number,
  integer = false
) {
  const bucket = timestamp / stepMs;
  const phase = deterministicValue(`${nodeSeed}:${metric}:phase`, 0, Math.PI * 2);
  const period = Math.round(deterministicValue(`${nodeSeed}:${metric}:period`, 24, 48));
  const secondaryPeriod = Math.round(
    deterministicValue(`${nodeSeed}:${metric}:secondary-period`, 9, 18)
  );
  const normalized =
    0.5 +
    Math.sin((bucket / period) * Math.PI * 2 + phase) * 0.3 +
    Math.sin((bucket / secondaryPeriod) * Math.PI * 2 + phase / 2) * 0.12;
  const value = minimum + Math.min(1, Math.max(0, normalized)) * (maximum - minimum);
  return integer ? Math.round(value) : Math.round(value * 10) / 10;
}

function buildPoint(
  nodeSeed: string,
  timestamp: number,
  stepMs: number,
  collectedAt: string,
  vcpu: number,
  ramGb: number
): MetricPoint {
  const pointSeed = `${nodeSeed}:${timestamp}`;
  if (hash(`${pointSeed}:missing`) % 37 === 0) {
    return {
      collectedAt,
      usedMemoryBytes: null,
      cpuMillicores: null,
      connectedClients: null,
      opsPerSec: null,
      keyspaceHits: null,
      keyspaceMisses: null,
      evictedKeys: null,
    };
  }

  return {
    collectedAt,
    usedMemoryBytes: smoothMetricValue(
      nodeSeed,
      'usedMemoryBytes',
      timestamp,
      stepMs,
      ramGb * GIBIBYTE * 0.3,
      ramGb * GIBIBYTE * 0.86
    ),
    cpuMillicores:
      hash(`${pointSeed}:cpu-gap`) % 19 === 0
        ? null
        : smoothMetricValue(nodeSeed, 'cpuMillicores', timestamp, stepMs, vcpu * 35, vcpu * 920),
    connectedClients: smoothMetricValue(
      nodeSeed,
      'connectedClients',
      timestamp,
      stepMs,
      2,
      600,
      true
    ),
    opsPerSec: smoothMetricValue(nodeSeed, 'opsPerSec', timestamp, stepMs, 40, 12_000, true),
    keyspaceHits: smoothMetricValue(nodeSeed, 'keyspaceHits', timestamp, stepMs, 20, 8_000, true),
    keyspaceMisses: smoothMetricValue(nodeSeed, 'keyspaceMisses', timestamp, stepMs, 0, 900, true),
    evictedKeys: smoothMetricValue(nodeSeed, 'evictedKeys', timestamp, stepMs, 0, 40, true),
  };
}

function buildNode(
  instanceId: string,
  slug: string,
  ordinal: number,
  role: ValkeyNodeRole,
  vcpu: number,
  ramGb: number,
  start: number,
  pointCount: number,
  stepMs: number
): ValkeyMetricNode {
  const name = `${slug}-${ordinal}`;
  const nodeSeed = `${instanceId}:${name}`;
  const points = Array.from({ length: pointCount }, (_, index) => {
    const timestamp = start + index * stepMs;
    const collectedAt = new Date(timestamp).toISOString();
    return buildPoint(nodeSeed, timestamp, stepMs, collectedAt, vcpu, ramGb);
  });

  return { id: `${instanceId}:${ordinal}`, name, role, points };
}

export async function getValkeyMetrics({
  ownerId,
  instanceId,
  range,
  signal,
}: GetValkeyMetricsInput): Promise<ValkeyMetricsResponse> {
  await delay(signal);
  const instance = readInstanceSnapshot(ownerId, instanceId);
  const { pointCount, stepMs } = METRIC_WINDOW_CONFIG[range];
  const end = Math.floor(Date.now() / stepMs) * stepMs;
  const start = end - (pointCount - 1) * stepMs;
  const roles: ValkeyNodeRole[] =
    instance.mode === 'ha' ? ['primary', 'replica', 'replica'] : ['primary'];

  return {
    source: 'demo',
    range,
    nodes: roles.map((role, ordinal) =>
      buildNode(
        instance.id,
        instance.slug,
        ordinal,
        role,
        instance.vcpu,
        instance.ramGb,
        start,
        pointCount,
        stepMs
      )
    ),
  };
}
