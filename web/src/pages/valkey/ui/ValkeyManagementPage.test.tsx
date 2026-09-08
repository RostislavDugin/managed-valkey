import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it } from 'vitest';
import { renderValkeySection, seedSession, TEST_USER_ID } from '../../../../test/render';
import { createInstance } from '../api/valkey-storage';
import type { ValkeyMode, ValkeyRamGb, ValkeyVcpu } from '../model/valkey';

const WAIT = { timeout: 10_000 };

function seedInstance(name: string, mode: ValkeyMode, vcpu: ValkeyVcpu, ramGb: ValkeyRamGb) {
  return createInstance(TEST_USER_ID, { name, prefix: 'valkey', mode, vcpu, ramGb });
}

beforeEach(() => {
  seedSession();
});

describe('загрузка списка', () => {
  it('резервирует место скелетоном и показывает таблицу после ответа', async () => {
    await seedInstance('valkey-1474', 'single', 1, 2);

    const { container } = renderValkeySection('/valkey/management');

    expect(container.querySelectorAll('.mantine-Skeleton-root').length).toBeGreaterThan(0);
    expect(await screen.findByRole('table', {}, WAIT)).toBeVisible();
    expect(container.querySelectorAll('.mantine-Skeleton-root')).toHaveLength(0);
  });

  it('показывает ошибку загрузки с повтором и восстанавливается после починки данных', async () => {
    localStorage.setItem('mv_valkey_instances', '{"version":3,"instances":[{}]}');
    const user = userEvent.setup();

    renderValkeySection('/valkey/management');

    expect(await screen.findByText('Не удалось загрузить базы', {}, WAIT)).toBeVisible();

    localStorage.removeItem('mv_valkey_instances');
    await user.click(screen.getByRole('button', { name: 'Повторить' }));

    expect(
      await screen.findByRole('heading', { name: 'Управляемые базы Valkey на DDR5' }, WAIT)
    ).toBeVisible();
  });

  it('показывает базы только текущего пользователя', async () => {
    await seedInstance('valkey-1474', 'single', 1, 2);
    await createInstance('01930000-0000-7000-8000-00000000ffff', {
      name: 'valkey-9999',
      prefix: 'valkey',
      mode: 'single',
      vcpu: 1,
      ramGb: 1,
    });

    renderValkeySection('/valkey/management');

    expect(await screen.findByRole('link', { name: 'valkey-1474' }, WAIT)).toBeVisible();
    expect(screen.queryByRole('link', { name: 'valkey-9999' })).toBeNull();
  });
});

describe('пустое состояние', () => {
  it('вместо таблицы и квоты показывает точку входа в создание', async () => {
    renderValkeySection('/valkey/management');

    expect(
      await screen.findByRole('heading', { name: 'Управляемые базы Valkey на DDR5' }, WAIT)
    ).toBeVisible();
    expect(screen.queryByRole('table')).toBeNull();
    expect(screen.queryByText('Использование')).toBeNull();
    expect(screen.getByRole('link', { name: 'Создать базу данных' })).toHaveAttribute(
      'href',
      '/valkey/management/new'
    );
  });
});

describe('таблица и поиск', () => {
  it('показывает столбцы и значения базы', async () => {
    await seedInstance('valkey-1474', 'ha', 1, 2);

    renderValkeySection('/valkey/management');

    const table = await screen.findByRole('table', {}, WAIT);
    const headers = Array.from(table.querySelectorAll('thead th')).map((cell) => cell.textContent);

    expect(headers).toEqual(['Имя', 'Статус', 'Режим', 'vCPU', 'RAM', 'Цена за месяц']);

    const row = within(table).getByRole('row', { name: /valkey-1474/ });
    expect(within(row).getByText('Работает')).toBeVisible();
    expect(within(row).getByText('Отказоустойчивый')).toBeVisible();
    expect(within(row).getByText(`1 vCPU`)).toBeVisible();
    expect(within(row).getByText(`2 ГБ`)).toBeVisible();
    expect(within(row).getByText(`4 860,00 ₽`)).toBeVisible();
  });

  it('сортирует полный список локально', async () => {
    await seedInstance('valkey-10', 'single', 1, 1);
    await seedInstance('valkey-2', 'ha', 1, 2);
    const user = userEvent.setup();

    renderValkeySection('/valkey/management');

    const table = await screen.findByRole('table', {}, WAIT);
    const names = () =>
      within(table)
        .getAllByRole('link')
        .map((link) => link.textContent);

    expect(names()).toEqual(['valkey-2', 'valkey-10']);

    await user.click(screen.getByRole('button', { name: 'Имя, по возрастанию' }));
    expect(names()).toEqual(['valkey-10', 'valkey-2']);

    await user.click(screen.getByRole('button', { name: 'RAM, без сортировки' }));
    expect(names()).toEqual(['valkey-10', 'valkey-2']);

    await user.click(screen.getByRole('button', { name: 'RAM, по возрастанию' }));
    expect(names()).toEqual(['valkey-2', 'valkey-10']);
  });

  it('строка ведёт в настройки базы', async () => {
    const created = await seedInstance('valkey-1474', 'single', 1, 2);
    const user = userEvent.setup();

    const { router } = renderValkeySection('/valkey/management');

    const table = await screen.findByRole('table', {}, WAIT);
    const row = within(table).getByRole('row', { name: /valkey-1474/ });
    await user.click(within(row).getByText('Работает'));

    await waitFor(
      () => expect(router.state.location.pathname).toBe(`/valkey/management/${created.id}`),
      WAIT
    );
  });

  it('фильтрует без учёта регистра и показывает «Ничего не найдено»', async () => {
    await seedInstance('valkey-1474', 'single', 1, 2);
    await seedInstance('cache-2222', 'ha', 1, 1);
    const user = userEvent.setup();

    renderValkeySection('/valkey/management');

    const search = await screen.findByRole('textbox', { name: 'Поиск по базам' }, WAIT);

    await user.type(search, 'VALKEY-14');
    expect(screen.getByRole('link', { name: 'valkey-1474' })).toBeVisible();
    expect(screen.queryByRole('link', { name: 'cache-2222' })).toBeNull();

    await user.clear(search);
    await user.type(search, 'отказоустойчивый');
    expect(screen.getByRole('link', { name: 'cache-2222' })).toBeVisible();

    await user.clear(search);
    await user.type(search, 'postgres');
    expect(screen.getByText('Ничего не найдено')).toBeVisible();
    expect(screen.queryByRole('table')).toBeNull();
    expect(screen.queryByRole('heading', { name: 'Управляемые базы Valkey' })).toBeNull();
  });
});

describe('панель использования', () => {
  it('уезжает в правую колонку каркаса', async () => {
    await seedInstance('valkey-1474', 'single', 1, 2);
    const user = userEvent.setup();

    renderValkeySection('/valkey/management');

    const aside = await screen.findByTestId('console-aside', {}, WAIT);
    expect(await within(aside).findByRole('heading', { name: 'Квота' }, WAIT)).toBeVisible();
    expect(within(aside).getByRole('link', { name: 'Увеличить через поддержку' })).toHaveAttribute(
      'href',
      'https://t.me/rostislav_dugin'
    );
    expect(within(aside).getByRole('button', { name: 'Что такое квота' })).toBeVisible();
    expect(within(aside).getByText('1 / 4')).toBeVisible();

    await user.hover(within(aside).getByRole('button', { name: 'Что такое квота' }));
    expect(
      await screen.findByText(/Квота ограничивает количество ресурсов/, {}, WAIT)
    ).toBeVisible();
  });

  it('считает занятое по одной ноде', async () => {
    await seedInstance('valkey-1474', 'single', 2, 8);

    renderValkeySection('/valkey/management');

    expect(await screen.findByRole('heading', { name: 'Квота' }, WAIT)).toBeVisible();
    expect(screen.getByText('2 / 4')).toBeVisible();
    expect(screen.getByText(`8 ГБ / 16 ГБ`)).toBeVisible();
  });

  it('считает занятое по трём нодам и по сочетанию режимов', async () => {
    await seedInstance('valkey-1474', 'ha', 1, 2);

    renderValkeySection('/valkey/management');

    expect(await screen.findByText('3 / 4', {}, WAIT)).toBeVisible();
    expect(screen.getByText(`6 ГБ / 16 ГБ`)).toBeVisible();
  });
});
