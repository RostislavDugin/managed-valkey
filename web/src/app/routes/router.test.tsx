import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it } from 'vitest';
import { renderRoutes, seedInstances, seedSession } from '../../../test/render';
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

  it('открывает заглушку мониторинга базы по прямой ссылке', async () => {
    seedOne();

    renderAt('/valkey/management/instance-1/monitoring');

    expect(await screen.findByRole('heading', { name: 'Мониторинг' }, WAIT)).toBeVisible();
    expect(screen.getByText('Раздел появится позже')).toBeVisible();
    expect(screen.getByRole('tab', { name: 'Мониторинг' })).toHaveAttribute(
      'aria-selected',
      'true'
    );
  });

  it('открывает заглушку журнала аудита базы по прямой ссылке', async () => {
    seedOne();

    renderAt('/valkey/management/instance-1/audit-logs');

    expect(await screen.findByRole('heading', { name: 'Аудит логи' }, WAIT)).toBeVisible();
    expect(screen.getByText('Раздел появится позже')).toBeVisible();
    expect(screen.getByRole('tab', { name: 'Аудит логи' })).toHaveAttribute(
      'aria-selected',
      'true'
    );
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

    await user.click(screen.getByRole('tab', { name: 'Аудит логи' }));
    expect(await screen.findByRole('heading', { name: 'Аудит логи' }, WAIT)).toBeVisible();
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
