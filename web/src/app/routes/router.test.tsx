import { fireEvent, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { AUTH_INVALIDATED_EVENT, AUTH_TOKEN_KEY } from '@/shared/api';
import { renderRoutes, seedInstances, seedSession, TEST_USER_ID } from '../../../test/render';
import { routeTable } from './router';

const WAIT = { timeout: 10_000 };

function renderAt(path: string) {
  return renderRoutes(routeTable, path);
}

function crumbText() {
  const crumbs = screen.getByRole('navigation', { name: 'Хлебные крошки' });
  return (crumbs.textContent ?? '').replace(/\s+/g, ' ').trim();
}

function seedOne() {
  seedInstances([{ id: 'instance-1', name: 'valkey-1474', mode: 'single', vcpu: 1, ramGb: 2 }]);
}

beforeEach(() => {
  seedSession();
});

describe('маршруты раздела Valkey', () => {
  it('перенаправляет корень на список баз', async () => {
    renderAt('/');

    expect(
      await screen.findByRole('heading', { name: 'Управляемые базы Valkey на DDR5' }, WAIT)
    ).toBeVisible();
  });

  it('перенаправляет /valkey на список баз', async () => {
    renderAt('/valkey');

    expect(
      await screen.findByRole('heading', { name: 'Управляемые базы Valkey на DDR5' }, WAIT)
    ).toBeVisible();
  });

  it('открывает форму создания по прямой ссылке', async () => {
    renderAt('/valkey/management/new');

    expect(await screen.findByRole('heading', { name: 'Новая Valkey база' }, WAIT)).toBeVisible();
  });

  it('открывает карточку базы по прямой ссылке', async () => {
    seedOne();

    renderAt('/valkey/management/instance-1');

    expect(await screen.findByRole('heading', { name: 'valkey-1474' }, WAIT)).toBeVisible();
    expect(screen.getByRole('tab', { name: 'Управление' })).toHaveAttribute(
      'aria-selected',
      'true'
    );
  });

  it('открывает мониторинг базы по прямой ссылке', async () => {
    seedOne();

    renderAt('/valkey/management/instance-1/monitoring');

    expect(await screen.findByRole('heading', { name: 'Мониторинг' }, WAIT)).toBeVisible();
    expect(screen.getByText('Данных мониторинга пока нет')).toBeVisible();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.queryByText('Демонстрационные данные')).not.toBeInTheDocument();
    expect(screen.getByRole('tab', { name: 'Мониторинг' })).toHaveAttribute(
      'aria-selected',
      'true'
    );
    expect(
      vi.mocked(window.fetch).mock.calls.some(([input]) => String(input).includes('/metrics'))
    ).toBe(false);
  });

  it('открывает аудит базы по прямой ссылке', async () => {
    seedOne();

    renderAt('/valkey/management/instance-1/audit-logs');

    expect(await screen.findByRole('heading', { name: 'Аудит' }, WAIT)).toBeVisible();
    expect(screen.getByText('Событий аудита пока нет')).toBeVisible();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.getByRole('tab', { name: 'Аудит' })).toHaveAttribute('aria-selected', 'true');
    expect(
      vi.mocked(window.fetch).mock.calls.some(([input]) => String(input).includes('/audit'))
    ).toBe(false);
  });
});

describe('вкладки принадлежат выбранной базе', () => {
  it('не показывает вкладки в списке баз', async () => {
    seedOne();
    renderAt('/valkey/management');

    await screen.findByRole('table', {}, WAIT);
    expect(screen.queryAllByRole('tab')).toHaveLength(0);
  });

  it('не показывает вкладки на создании базы', async () => {
    renderAt('/valkey/management/new');

    await screen.findByRole('heading', { name: 'Новая Valkey база' }, WAIT);
    expect(screen.queryAllByRole('tab')).toHaveLength(0);
  });

  it('мышь меняет вкладку и адрес, кнопка «назад» возвращает прежнюю', async () => {
    seedOne();
    const user = userEvent.setup();
    const { router } = renderAt('/valkey/management/instance-1');

    await screen.findByRole('heading', { name: 'valkey-1474' }, WAIT);

    await user.click(screen.getByRole('tab', { name: 'Аудит' }));
    expect(await screen.findByRole('heading', { name: 'Аудит' }, WAIT)).toBeVisible();
    expect(router.state.location.pathname).toBe('/valkey/management/instance-1/audit-logs');

    await router.navigate(-1);
    await waitFor(
      () =>
        expect(screen.getByRole('tab', { name: 'Управление' })).toHaveAttribute(
          'aria-selected',
          'true'
        ),
      WAIT
    );
    expect(router.state.location.pathname).toBe('/valkey/management/instance-1');
  });

  it('клавиатура открывает вкладку по Enter', async () => {
    seedOne();
    const user = userEvent.setup();
    renderAt('/valkey/management/instance-1');

    await screen.findByRole('heading', { name: 'valkey-1474' }, WAIT);

    const monitoring = screen.getByRole('tab', { name: 'Мониторинг' });
    monitoring.focus();
    await user.keyboard('{Enter}');

    expect(await screen.findByRole('heading', { name: 'Мониторинг' }, WAIT)).toBeVisible();
  });
});

describe('хлебные крошки', () => {
  it('стоят в шапке каркаса, а не в содержимом страницы', async () => {
    seedOne();

    renderAt('/valkey/management');

    await screen.findByRole('table', {}, WAIT);
    expect(screen.getByRole('navigation', { name: 'Хлебные крошки' }).closest('main')).toBeNull();
  });

  it('в списке баз состоят из одного раздела', async () => {
    renderAt('/valkey/management');

    await screen.findByRole('heading', { name: 'Управляемые базы Valkey на DDR5' }, WAIT);
    expect(crumbText()).toBe('Базы данных');
  });

  it('на создании ведут от раздела к форме', async () => {
    renderAt('/valkey/management/new');

    await screen.findByRole('heading', { name: 'Новая Valkey база' }, WAIT);
    expect(crumbText()).toBe('Базы данных/Valkey/Создание БД');
  });

  it('в карточке базы называют базу и сохраняются на её вкладках', async () => {
    seedOne();
    const user = userEvent.setup();
    renderAt('/valkey/management/instance-1');

    await screen.findByRole('heading', { name: 'valkey-1474' }, WAIT);
    expect(crumbText()).toBe('Базы данных/Valkey/valkey-1474');

    await user.click(screen.getByRole('tab', { name: 'Мониторинг' }));

    await screen.findByRole('heading', { name: 'Мониторинг' }, WAIT);
    expect(crumbText()).toBe('Базы данных/Valkey/valkey-1474');
  });
});

describe('завершение сессии', () => {
  it('кнопка «Выйти» удаляет токен и открывает /auth', async () => {
    const user = userEvent.setup();
    const { router } = renderAt('/valkey/management');

    await screen.findByRole('heading', { name: 'Управляемые базы Valkey на DDR5' }, WAIT);
    await user.click(screen.getByRole('button', { name: 'user@example.com' }));
    fireEvent.click(screen.getByRole('menuitem', { name: 'Выйти', hidden: true }));

    await waitFor(() => expect(router.state.location.pathname).toBe('/auth'), WAIT);
    expect(localStorage.getItem(AUTH_TOKEN_KEY)).toBeNull();
  });

  it('заканчивает сессию после удаления токена в другой вкладке', async () => {
    const { router } = renderAt('/valkey/management');

    await screen.findByRole('heading', { name: 'Управляемые базы Valkey на DDR5' }, WAIT);
    window.dispatchEvent(
      new StorageEvent('storage', {
        key: AUTH_TOKEN_KEY,
        oldValue: localStorage.getItem(AUTH_TOKEN_KEY),
        newValue: null,
      })
    );

    await waitFor(() => expect(router.state.location.pathname).toBe('/auth'), WAIT);
    expect(localStorage.getItem(AUTH_TOKEN_KEY)).toBeNull();
  });

  it('заканчивает сессию после 401 и по таймеру exp', async () => {
    const first = renderAt('/valkey/management');
    await screen.findByRole('heading', { name: 'Управляемые базы Valkey на DDR5' }, WAIT);
    window.dispatchEvent(new Event(AUTH_INVALIDATED_EVENT));
    await waitFor(() => expect(first.router.state.location.pathname).toBe('/auth'), WAIT);
    first.unmount();

    vi.useFakeTimers({ shouldAdvanceTime: true });
    seedSession();
    const second = renderAt('/valkey/management');
    await screen.findByRole('heading', { name: 'Управляемые базы Valkey на DDR5' }, WAIT);
    await vi.advanceTimersByTimeAsync(3_600_000);
    await vi.waitFor(() => expect(second.router.state.location.pathname).toBe('/auth'));
    vi.useRealTimers();
  });

  it('не завершает десятилетнюю сессию из-за предела setTimeout', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    seedSession(TEST_USER_ID, 'user@example.com', 10 * 365 * 24 * 60 * 60);
    const { router } = renderAt('/valkey/management');

    await screen.findByRole('heading', { name: 'Управляемые базы Valkey на DDR5' }, WAIT);
    await vi.advanceTimersByTimeAsync(1000);

    expect(router.state.location.pathname).toBe('/valkey/management');
    expect(localStorage.getItem(AUTH_TOKEN_KEY)).not.toBeNull();
    vi.useRealTimers();
  });
});
