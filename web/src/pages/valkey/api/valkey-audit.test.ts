import { beforeEach, describe, expect, it, vi } from 'vitest';
import { getAuditLogPage } from './valkey-audit';

const INSTANCE_ID = '01930000-0000-7000-8000-000000000002';

beforeEach(() => {
  localStorage.clear();
  localStorage.setItem('mv_token', 'server-token');
});

describe('клиент аудита Valkey', () => {
  it('при запросе аудита передаёт фильтры в URL и преобразует серверные события в клиентскую модель', async () => {
    const controller = new AbortController();
    const fetchMock = vi.spyOn(window, 'fetch').mockResolvedValue(
      new Response(
        JSON.stringify({
          items: [
            {
              id: '01993000-0000-7000-8000-000000000001',
              action: 'instance.whitelist.update',
              user_email: 'owner@example.com',
              created_at: '2026-09-09T10:11:12.345678Z',
            },
          ],
          next_cursor: 'opaque-cursor',
        }),
        { status: 200, headers: { 'Content-Type': 'application/json' } }
      )
    );

    await expect(
      getAuditLogPage({
        instanceId: INSTANCE_ID,
        limit: 25,
        before: 'previous/cursor',
        signal: controller.signal,
      })
    ).resolves.toEqual({
      items: [
        {
          id: '01993000-0000-7000-8000-000000000001',
          action: 'instance.whitelist.update',
          userEmail: 'owner@example.com',
          createdAt: '2026-09-09T10:11:12.345678Z',
        },
      ],
      nextCursor: 'opaque-cursor',
    });

    expect(fetchMock).toHaveBeenCalledOnce();
    expect(fetchMock.mock.calls[0][0]).toBe(
      `/v1/managed/valkey/instances/${INSTANCE_ID}/audit?limit=25&before=previous%2Fcursor`
    );
    expect(fetchMock.mock.calls[0][1]?.signal).toBe(controller.signal);
  });

  it('при отсутствии необязательных фильтров не добавляет их в URL запроса аудита', async () => {
    const fetchMock = vi.spyOn(window, 'fetch').mockResolvedValue(
      new Response(JSON.stringify({ items: [], next_cursor: null }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      })
    );

    await expect(getAuditLogPage({ instanceId: INSTANCE_ID })).resolves.toEqual({
      items: [],
      nextCursor: null,
    });
    expect(fetchMock.mock.calls[0][0]).toBe(`/v1/managed/valkey/instances/${INSTANCE_ID}/audit`);
  });
});
