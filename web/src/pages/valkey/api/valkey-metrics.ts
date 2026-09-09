import { apiRequest } from '@/shared/api';
import type {
  MetricPoint,
  MetricWindow,
  ValkeyMetricsResponse,
  ValkeyNodeRole,
} from '../model/valkey-observability';

interface MetricPointDto {
  collected_at: string;
  used_memory_bytes: number | null;
  cpu_millicores: number | null;
  connected_clients: number | null;
  ops_per_sec: number | null;
  keyspace_hits: number | null;
  keyspace_misses: number | null;
  evicted_keys: number | null;
}

interface ValkeyMetricsDto {
  from: string;
  to: string;
  step_seconds: number;
  nodes: Array<{
    ordinal: number;
    name: string;
    role: ValkeyNodeRole;
    points: MetricPointDto[];
  }>;
}

export interface GetValkeyMetricsInput {
  instanceId: string;
  range: MetricWindow;
  signal: AbortSignal;
}

function mapPoint(point: MetricPointDto): MetricPoint {
  return {
    collectedAt: point.collected_at,
    usedMemoryBytes: point.used_memory_bytes,
    cpuMillicores: point.cpu_millicores,
    connectedClients: point.connected_clients,
    opsPerSec: point.ops_per_sec,
    keyspaceHits: point.keyspace_hits,
    keyspaceMisses: point.keyspace_misses,
    evictedKeys: point.evicted_keys,
  };
}

export async function getValkeyMetrics({
  instanceId,
  range,
  signal,
}: GetValkeyMetricsInput): Promise<ValkeyMetricsResponse> {
  const dto = await apiRequest<ValkeyMetricsDto>(
    `/managed/valkey/instances/${instanceId}/metrics?range=${encodeURIComponent(range)}`,
    { signal }
  );

  return {
    from: dto.from,
    to: dto.to,
    stepSeconds: dto.step_seconds,
    nodes: dto.nodes.map((node) => ({
      id: String(node.ordinal),
      ordinal: node.ordinal,
      name: node.name,
      role: node.role,
      points: node.points.map(mapPoint),
    })),
  };
}
