import { apiRequest } from '@/shared/api';
import type { AuditAction, AuditLogPage } from '../model/valkey-audit';

interface AuditLogEntryDto {
  id: string;
  action: AuditAction;
  user_email: string;
  created_at: string;
}

interface AuditLogPageDto {
  items: AuditLogEntryDto[];
  next_cursor: string | null;
}

export interface GetAuditLogPageInput {
  instanceId: string;
  limit?: number;
  before?: string | null;
  signal?: AbortSignal;
}

export async function getAuditLogPage({
  instanceId,
  limit,
  before,
  signal,
}: GetAuditLogPageInput): Promise<AuditLogPage> {
  const query = new URLSearchParams();
  if (limit !== undefined) {
    query.set('limit', String(limit));
  }
  if (before !== undefined && before !== null) {
    query.set('before', before);
  }

  const suffix = query.size > 0 ? `?${query}` : '';
  const page = await apiRequest<AuditLogPageDto>(
    `/managed/valkey/instances/${instanceId}/audit${suffix}`,
    { signal }
  );

  return {
    items: page.items.map((item) => ({
      id: item.id,
      action: item.action,
      userEmail: item.user_email,
      createdAt: item.created_at,
    })),
    nextCursor: page.next_cursor,
  };
}
