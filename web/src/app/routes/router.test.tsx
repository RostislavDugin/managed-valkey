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
  it('при открытии корневого пути перенаправляет пользователя к списку баз Valkey', async () => {
    renderAt('/');

    expect(
      await screen.findByRole('heading', { name: 'Управляемые базы Valkey на DDR5' }, WAIT)
    ).toBeVisible();
  });

  it('при открытии /valkey перенаправляет пользователя к списку баз Valkey', async () => {
    renderAt('/valkey');

    expect(
      await screen.findByRole('heading', { name: 'Управляемые базы Valkey на DDR5' }, WAIT)
    ).toBeVisible();
  });

  it('при прямом переходе на путь создания показывает форму новой базы Valkey', async () => {
    renderAt('/valkey/management/new');

    expect(await screen.findByRole('heading', { name: 'Новая Valkey база' }, WAIT)).toBeVisible();
  });

  it('при прямом переходе к существующей базе показывает её карточку и активную вкладку управления', async () => {
    seedOne();

    renderAt('/valkey/management/instance-1');

    expect(await screen.findByRole('heading', { name: 'valkey-1474' }, WAIT)).toBeVisible();
    expect(screen.getByRole('tab', { name: 'Управление' })).toHaveAttribute(
      'aria-selected',
      'true'
    );
  });

  it('при прямом переходе к мониторингу показывает пустое состояние, выбирает вкладку и запрашивает метрики', async () => {
    seedOne();

    renderAt('/valkey/management/instance-1/monitoring');

    expect(await screen.findByRole('heading', { name: 'Мониторинг' }, WAIT)).toBeVisible();
    expect(await screen.findByRole('heading', { name: 'Метрик пока нет' }, WAIT)).toBeVisible();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.queryByText('Демонстрационные данные')).not.toBeInTheDocument();
    expect(screen.getByRole('tab', { name: 'Мониторинг' })).toHaveAttribute(
      'aria-selected',
      'true'
    );
    expect(
      vi
        .mocked(window.fetch)
        .mock.calls.some(([input]) => String(input).endsWith('/metrics?range=5m'))
    ).toBe(true);
  });

  it('при прямом переходе к аудиту показывает пустое состояние, выбирает вкладку и запрашивает события', async () => {
    seedOne();

    renderAt('/valkey/management/instance-1/audit-logs');

    expect(await screen.findByRole('heading', { name: 'Аудит' }, WAIT)).toBeVisible();
    expect(await screen.findByText('Действий с базой пока не было', {}, WAIT)).toBeVisible();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.getByRole('tab', { name: 'Аудит' })).toHaveAttribute('aria-selected', 'true');
    expect(
      vi.mocked(window.fetch).mock.calls.some(([input]) => String(input).includes('/audit'))
    ).toBe(true);
  });
});

describe('вкладки принадлежат выбранной базе', () => {
  it('в списке баз не показывает вкладки, относящиеся к выбранной базе', async () => {
    seedOne();
    renderAt('/valkey/management');

    await screen.findByRole('table', {}, WAIT);
    expect(screen.queryAllByRole('tab')).toHaveLength(0);
  });

  it('на странице создания не показывает вкладки ещё не созданной базы', async () => {
    renderAt('/valkey/management/new');

    await screen.findByRole('heading', { name: 'Новая Valkey база' }, WAIT);
    expect(screen.queryAllByRole('tab')).toHaveLength(0);
  });

  it('после выбора вкладки мышью меняет адрес, а возврат в истории открывает прежнюю вкладку', async () => {
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

  it('при нажатии Enter на вкладке открывает соответствующую страницу базы', async () => {
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
  it('размещает хлебные крошки в шапке приложения вне содержимого страницы', async () => {
    seedOne();

    renderAt('/valkey/management');

    await screen.findByRole('table', {}, WAIT);
    expect(screen.getByRole('navigation', { name: 'Хлебные крошки' }).closest('main')).toBeNull();
  });

  it('на странице списка показывает в хлебных крошках только раздел баз данных', async () => {
    renderAt('/valkey/management');

    await screen.findByRole('heading', { name: 'Управляемые базы Valkey на DDR5' }, WAIT);
    expect(crumbText()).toBe('Базы данных');
  });

  it('на странице создания показывают путь от раздела баз данных к форме Valkey', async () => {
    renderAt('/valkey/management/new');

    await screen.findByRole('heading', { name: 'Новая Valkey база' }, WAIT);
    expect(crumbText()).toBe('Базы данных/Valkey/Создание БД');
  });

  it('в карточке и на её вкладках сохраняют имя выбранной базы в хлебных крошках', async () => {
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
  it('после нажатия «Выйти» удаляет токен и открывает страницу /auth', async () => {
    const user = userEvent.setup();
    const { router } = renderAt('/valkey/management');

    await screen.findByRole('heading', { name: 'Управляемые базы Valkey на DDR5' }, WAIT);
    await user.click(screen.getByRole('button', { name: 'user@example.com' }));
    fireEvent.click(screen.getByRole('menuitem', { name: 'Выйти', hidden: true }));

    await waitFor(() => expect(router.state.location.pathname).toBe('/auth'), WAIT);
    expect(localStorage.getItem(AUTH_TOKEN_KEY)).toBeNull();
  });

  it('после удаления токена в другой вкладке завершает сессию и открывает страницу входа', async () => {
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

  it('после ответа 401 или истечения JWT завершает сессию и открывает страницу входа', async () => {
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

  it('при сроке JWT в десять лет не завершает сессию из-за ограничения setTimeout', async () => {
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
