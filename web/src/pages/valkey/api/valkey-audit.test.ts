import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { AuditLogEntry } from '../model/valkey-observability';
import { getAuditLogPage } from './valkey-audit';
import { VALKEY_INSTANCES_KEY } from './valkey-storage';

const OWNER = 'owner-1';
const INSTANCE_ID = 'instance-1';

function audit(id: string, createdAt: string): AuditLogEntry {
  return {
    id,
    instanceId: INSTANCE_ID,
    action: 'instance.update',
    userEmail: 'owner@example.com',
    createdAt,
  };
}

function seedAudit(auditLogs: AuditLogEntry[]) {
  localStorage.setItem(
    VALKEY_INSTANCES_KEY,
    JSON.stringify({
      version: 4,
      instances: [
        {
          id: INSTANCE_ID,
          ownerId: OWNER,
          name: 'valkey-1474',
          prefix: 'valkey',
          slug: 'valkey-abc123',
          mode: 'single',
          vcpu: 1,
          ramGb: 1,
          isWhitelistEnabled: false,
          whitelistCidrs: [],
          status: 'running',
          createdAt: '2026-09-08T00:00:00.000Z',
          updatedAt: '2026-09-08T00:00:00.000Z',
        },
      ],
      auditLogs,
    })
  );
}

async function resolve<T>(promise: Promise<T>) {
  await vi.advanceTimersByTimeAsync(400);
  return promise;
}

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

describe('временный клиент аудита', () => {
  it('по умолчанию возвращает не больше 50 новых событий', async () => {
    seedAudit(
      Array.from({ length: 55 }, (_, index) =>
        audit(String(index).padStart(2, '0'), new Date(2026, 8, 8, 0, index).toISOString())
      )
    );

    const page = await resolve(getAuditLogPage({ ownerId: OWNER, instanceId: INSTANCE_ID }));

    expect(page.items).toHaveLength(50);
    expect(page.items[0].id).toBe('54');
    expect(page.nextCursor).toBeTruthy();
    expect(page.nextCursor).not.toContain('=');
  });

  it('не пропускает и не повторяет события с одинаковым временем', async () => {
    const createdAt = '2026-09-08T12:00:00.000Z';
    seedAudit(['a', 'd', 'b', 'c'].map((id) => audit(id, createdAt)));

    const first = await resolve(
      getAuditLogPage({ ownerId: OWNER, instanceId: INSTANCE_ID, limit: 2 })
    );
    const second = await resolve(
      getAuditLogPage({
        ownerId: OWNER,
        instanceId: INSTANCE_ID,
        limit: 2,
        before: first.nextCursor,
      })
    );

    expect([...first.items, ...second.items].map((item) => item.id)).toEqual(['d', 'c', 'b', 'a']);
    expect(second.nextCursor).toBeNull();
  });

  it('отклоняет курсор, который нельзя разобрать', async () => {
    seedAudit([]);

    await expect(
      getAuditLogPage({ ownerId: OWNER, instanceId: INSTANCE_ID, before: 'broken' })
    ).rejects.toMatchObject({ code: 'VALIDATION_FAILED' });
  });
});
