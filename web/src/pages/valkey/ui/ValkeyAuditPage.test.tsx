import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { withProviders } from '../../../../test/render';
import type { AuditLogEntry, AuditLogPage } from '../model/valkey-audit';
import { ValkeyAuditPage } from './ValkeyAuditPage';

const mocks = vi.hoisted(() => ({
  getAuditLogPage: vi.fn(),
  instanceId: 'instance-1',
}));

vi.mock('../api/valkey-audit', () => ({ getAuditLogPage: mocks.getAuditLogPage }));
vi.mock('./ValkeyInstanceLayout', () => ({
  useValkeyInstance: () => ({ instance: { id: mocks.instanceId } }),
}));

function auditEvent(id: string, action: AuditLogEntry['action'], createdAt: string): AuditLogEntry {
  return { id, action, createdAt, userEmail: 'owner@example.com' };
}

function deferredPage() {
  let resolve: (page: AuditLogPage) => void = () => undefined;
  const promise = new Promise<AuditLogPage>((complete) => {
    resolve = complete;
  });
  return { promise, resolve };
}

function renderPage() {
  return render(withProviders(<ValkeyAuditPage />));
}

beforeEach(() => {
  mocks.instanceId = 'instance-1';
  mocks.getAuditLogPage.mockReset();
});

afterEach(() => {
  vi.useRealTimers();
});

describe('страница аудита Valkey', () => {
  it('для события аудита показывает действие и автора, а относительное время обновляет раз в минуту', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.setSystemTime(new Date(2026, 8, 9, 12, 0));
    const createdAt = new Date(2026, 8, 9, 11, 55).toISOString();
    mocks.getAuditLogPage.mockResolvedValue({
      items: [auditEvent('event-1', 'instance.password.rotate', createdAt)],
      nextCursor: null,
    });

    renderPage();

    expect(await screen.findByText('Изменён пароль')).toBeVisible();
    expect(screen.getByText('instance.password.rotate')).toBeVisible();
    expect(screen.getByText('owner@example.com')).toBeVisible();
    expect(screen.getByText('5 минут назад')).toBeVisible();

    await act(() => vi.advanceTimersByTimeAsync(60_000));

    expect(screen.getByText('6 минут назад')).toBeVisible();
  });

  it('при загрузке следующей страницы добавляет её один раз и удаляет события с повторяющимся id', async () => {
    const nextPage = deferredPage();
    const first = auditEvent('event-1', 'instance.create', '2026-09-09T10:00:00Z');
    const second = auditEvent('event-2', 'instance.resize', '2026-09-08T10:00:00Z');
    mocks.getAuditLogPage
      .mockResolvedValueOnce({ items: [first], nextCursor: 'cursor-1' })
      .mockReturnValueOnce(nextPage.promise);

    renderPage();
    const loadMore = await screen.findByRole('button', { name: 'Показать ещё' });

    fireEvent.click(loadMore);
    fireEvent.click(loadMore);

    expect(mocks.getAuditLogPage).toHaveBeenCalledTimes(2);
    expect(mocks.getAuditLogPage).toHaveBeenLastCalledWith({
      instanceId: 'instance-1',
      before: 'cursor-1',
      signal: expect.any(AbortSignal),
    });

    nextPage.resolve({ items: [first, second], nextCursor: null });

    expect(await screen.findByText('Изменён тариф')).toBeVisible();
    expect(screen.getAllByText('Создана база')).toHaveLength(1);
    expect(screen.queryByRole('button', { name: 'Показать ещё' })).not.toBeInTheDocument();
  });

  it('при пустом журнале показывает отдельное состояние, а после ошибки позволяет повторить начальную загрузку', async () => {
    const user = userEvent.setup();
    mocks.getAuditLogPage
      .mockRejectedValueOnce(new Error('network'))
      .mockResolvedValueOnce({ items: [], nextCursor: null });

    renderPage();

    expect(await screen.findByText('Не удалось загрузить аудит')).toBeVisible();
    await user.click(screen.getByRole('button', { name: 'Повторить' }));

    expect(await screen.findByText('Действий с базой пока не было')).toBeVisible();
    expect(mocks.getAuditLogPage).toHaveBeenCalledTimes(2);
  });

  it('после перехода к другой базе отменяет прежний запрос аудита и не показывает его поздний ответ', async () => {
    const oldPage = deferredPage();
    const oldEvent = auditEvent('old-event', 'instance.delete', '2026-09-08T10:00:00Z');
    const newEvent = auditEvent('new-event', 'instance.update', '2026-09-09T10:00:00Z');
    let oldSignal: AbortSignal | undefined;
    mocks.getAuditLogPage.mockImplementation(({ instanceId, signal }) => {
      if (instanceId === 'instance-1') {
        oldSignal = signal;
        return oldPage.promise;
      }
      return Promise.resolve({ items: [newEvent], nextCursor: null });
    });

    const rendered = renderPage();
    expect(screen.getByRole('status', { name: 'Загрузка аудита' })).toBeVisible();
    await waitFor(() => expect(mocks.getAuditLogPage).toHaveBeenCalledTimes(1));

    mocks.instanceId = 'instance-2';
    rendered.rerender(withProviders(<ValkeyAuditPage />));

    expect(await screen.findByText('Изменены настройки')).toBeVisible();
    expect(oldSignal?.aborted).toBe(true);

    oldPage.resolve({ items: [oldEvent], nextCursor: null });
    await act(async () => oldPage.promise);

    expect(screen.queryByText('Удалена база')).not.toBeInTheDocument();
  });
});
