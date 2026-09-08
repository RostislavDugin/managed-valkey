import { ApiError } from '@/shared/api';
import type { AuditLogEntry, AuditLogPage } from '../model/valkey-observability';
import { readAuditSnapshot } from './valkey-storage';

const RESPONSE_DELAY_MS = 400;
const DEFAULT_LIMIT = 50;
const MAX_LIMIT = 200;

interface AuditCursor {
  createdAt: string;
  id: string;
}

export interface GetAuditLogPageInput {
  ownerId: string;
  instanceId: string;
  limit?: number;
  before?: string | null;
}

const delay = () => new Promise((resolve) => setTimeout(resolve, RESPONSE_DELAY_MS));

function encodeCursor(item: AuditLogEntry) {
  return btoa(JSON.stringify({ createdAt: item.createdAt, id: item.id }))
    .replaceAll('+', '-')
    .replaceAll('/', '_')
    .replace(/=+$/, '');
}

function decodeCursor(value: string): AuditCursor {
  try {
    const base64 = value.replaceAll('-', '+').replaceAll('_', '/');
    const padding = '='.repeat((4 - (base64.length % 4)) % 4);
    const parsed = JSON.parse(atob(base64 + padding)) as Record<string, unknown>;

    if (typeof parsed.createdAt === 'string' && typeof parsed.id === 'string') {
      return { createdAt: parsed.createdAt, id: parsed.id };
    }
  } catch {
    throw new ApiError('VALIDATION_FAILED', 'Курсор журнала аудита недействителен');
  }

  throw new ApiError('VALIDATION_FAILED', 'Курсор журнала аудита недействителен');
}

function compareAuditLogs(left: AuditLogEntry, right: AuditLogEntry) {
  return right.createdAt.localeCompare(left.createdAt) || right.id.localeCompare(left.id);
}

function comesAfterCursor(item: AuditLogEntry, cursor: AuditCursor) {
  return (
    item.createdAt < cursor.createdAt ||
    (item.createdAt === cursor.createdAt && item.id < cursor.id)
  );
}

export async function getAuditLogPage({
  ownerId,
  instanceId,
  limit = DEFAULT_LIMIT,
  before,
}: GetAuditLogPageInput): Promise<AuditLogPage> {
  if (!Number.isInteger(limit) || limit < 1 || limit > MAX_LIMIT) {
    throw new ApiError('VALIDATION_FAILED', 'Лимит журнала должен быть от 1 до 200');
  }

  const cursor = before ? decodeCursor(before) : null;
  await delay();

  const sorted = readAuditSnapshot(ownerId, instanceId).sort(compareAuditLogs);
  const available = cursor ? sorted.filter((item) => comesAfterCursor(item, cursor)) : sorted;
  const items = available.slice(0, limit);

  return {
    items,
    nextCursor: available.length > limit ? encodeCursor(items.at(-1) as AuditLogEntry) : null,
  };
}
