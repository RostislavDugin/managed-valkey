import { act } from 'react';
import { fireEvent, render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { formatDateTime } from '@/shared/lib';
import { withProviders } from '../../../../test/render';
import { getAuditLogPage } from '../api/valkey-audit';
import type { AuditAction, AuditLogEntry, AuditLogPage } from '../model/valkey-observability';
import { ValkeyAuditPage } from './ValkeyAuditPage';

vi.mock('../api/valkey-audit', () => ({ getAuditLogPage: vi.fn() }));
vi.mock('./ValkeyInstanceLayout', () => ({
  useValkeyInstance: () => ({
    instance: { id: 'instance-1' },
    session: { userId: 'owner-1' },
  }),
}));

function event(id: string, action: AuditAction, createdAt: string): AuditLogEntry {
  return {
    id,
    instanceId: 'instance-1',
    action,
    userEmail: 'owner@example.com',
    createdAt,
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
  return render(withProviders(<ValkeyAuditPage />));
}

beforeEach(() => {
  vi.mocked(getAuditLogPage).mockResolvedValue({
    items: [event('1', 'instance.update', '2026-09-08T11:55:00.000Z')],
    nextCursor: null,
  });
});

afterEach(() => {
  vi.useRealTimers();
});

describe('экран аудита', () => {
  it('показывает действие, техническое имя, почту, точное и относительное время', async () => {
    vi.useFakeTimers();
    vi.setSystemTime('2026-09-08T12:00:00.000Z');

    await act(async () => renderPage());

    expect(screen.getByText('Изменено имя')).toBeVisible();
    expect(screen.getByText('instance.update')).toBeVisible();
    expect(screen.getByText('owner@example.com')).toBeVisible();
    expect(screen.getByText(formatDateTime('2026-09-08T11:55:00.000Z'))).toBeVisible();
    expect(screen.getByText('5 минут назад')).toBeVisible();

    await act(async () => vi.advanceTimersByTimeAsync(60_000));

    expect(screen.getByText('6 минут назад')).toBeVisible();
  });

  it('группирует сегодня, вчера и старый календарный год', async () => {
    vi.useFakeTimers();
    vi.setSystemTime('2026-09-08T12:00:00.000Z');
    vi.mocked(getAuditLogPage).mockResolvedValue({
      items: [
        event('1', 'instance.create', '2026-09-08T10:00:00.000Z'),
        event('2', 'instance.resize', '2026-09-07T10:00:00.000Z'),
        event('3', 'instance.delete', '2025-08-20T10:00:00.000Z'),
      ],
      nextCursor: null,
    });

    await act(async () => renderPage());

    expect(screen.getByRole('heading', { name: 'Сегодня' })).toBeVisible();
    expect(screen.getByRole('heading', { name: 'Вчера' })).toBeVisible();
    expect(screen.getByRole('heading', { name: /20 авг.*2025/i })).toBeVisible();
  });

  it('добавляет следующую страницу, сливает группу даты и не принимает два нажатия', async () => {
    const nextPage = deferred<AuditLogPage>();
    vi.mocked(getAuditLogPage)
      .mockResolvedValueOnce({
        items: [event('2', 'instance.resize', new Date().toISOString())],
        nextCursor: 'opaque-cursor',
      })
      .mockImplementationOnce(() => nextPage.promise);

    renderPage();
    const button = await screen.findByRole('button', { name: 'Показать ещё' });

    fireEvent.click(button);
    fireEvent.click(button);

    expect(getAuditLogPage).toHaveBeenCalledTimes(2);

    await act(async () =>
      nextPage.resolve({
        items: [
          event('1', 'instance.create', new Date(Date.now() - 60_000).toISOString()),
          event('2', 'instance.resize', new Date().toISOString()),
        ],
        nextCursor: null,
      })
    );

    expect(screen.getAllByRole('heading', { name: 'Сегодня' })).toHaveLength(1);
    expect(screen.getAllByText('owner@example.com')).toHaveLength(2);
    expect(screen.queryByRole('button', { name: 'Показать ещё' })).toBeNull();
  });

  it('показывает пустой журнал', async () => {
    vi.mocked(getAuditLogPage).mockResolvedValue({ items: [], nextCursor: null });

    renderPage();

    expect(
      await screen.findByRole('heading', { name: 'Действий с базой пока не было' })
    ).toBeVisible();
  });

  it('показывает ошибку и повторяет первый запрос', async () => {
    vi.mocked(getAuditLogPage)
      .mockRejectedValueOnce(new Error('Сеть недоступна'))
      .mockResolvedValueOnce({
        items: [event('1', 'instance.create', new Date().toISOString())],
        nextCursor: null,
      });
    const user = userEvent.setup();

    renderPage();
    expect(
      await screen.findByText('Не удалось выполнить запрос. Попробуйте ещё раз.')
    ).toBeVisible();

    await user.click(screen.getByRole('button', { name: 'Повторить' }));

    expect(await screen.findByText('Создана база')).toBeVisible();
  });
});
