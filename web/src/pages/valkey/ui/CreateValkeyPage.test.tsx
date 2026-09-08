import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it } from 'vitest';
import { renderValkeySection, seedSession, TEST_USER_ID, wholeText } from '../../../../test/render';
import { createInstance, listInstances } from '../api/valkey-storage';
import type { ValkeyMode, ValkeyRamGb, ValkeyVcpu } from '../model/valkey';

const WAIT = { timeout: 10_000 };

function plan(vcpu: number, ramGb: number) {
  return screen.getByRole('radio', {
    name: new RegExp(`^${vcpu}\\s+vCPU,\\s+${ramGb}\\s+ГБ RAM$`),
  });
}

async function openForm() {
  const result = renderValkeySection('/valkey/management/new');
  await screen.findByRole('heading', { name: 'Новая Valkey база' }, WAIT);
  return result;
}

function seedInstance(name: string, mode: ValkeyMode, vcpu: ValkeyVcpu, ramGb: ValkeyRamGb) {
  return createInstance(TEST_USER_ID, { name, prefix: 'valkey', mode, vcpu, ramGb });
}

beforeEach(() => {
  seedSession();
});

describe('значения по умолчанию', () => {
  it('открывает форму рабочей: имя, режим и доступный размер', async () => {
    await openForm();

    const name = screen.getByRole('textbox', { name: /Имя/ }) as HTMLInputElement;
    expect(name.value).toMatch(/^valkey-\d{4}$/);

    expect(screen.getByRole('radio', { name: /Одна нода/ })).toBeChecked();
    expect(plan(1, 1)).toBeChecked();
    expect(screen.getByRole('button', { name: 'Создать базу' })).toBeEnabled();
  });

  it('подставляет префикс и показывает будущий адрес с доменом', async () => {
    await openForm();

    const prefix = screen.getByRole('textbox', { name: /Префикс/ }) as HTMLInputElement;
    expect(prefix.value).toBe('valkey');
    expect(
      await screen.findByText(/valkey-xxxxxx\.valkey\.h3llo-demo\.com/, {}, WAIT)
    ).toBeVisible();
  });

  it('проверяет префикс при потере фокуса', async () => {
    const user = userEvent.setup();
    await openForm();

    const prefix = screen.getByRole('textbox', { name: /Префикс/ });
    await user.clear(prefix);
    await user.type(prefix, 'ab');
    await user.tab();

    expect(await screen.findByText('Префикс не короче 3 символов', {}, WAIT)).toBeVisible();
  });

  it('проверяет имя при потере фокуса', async () => {
    const user = userEvent.setup();
    await openForm();

    const name = screen.getByRole('textbox', { name: /Имя/ });
    await user.clear(name);
    await user.type(name, 'Valkey_1474');
    await user.tab();

    expect(
      await screen.findByText(
        'Строчные латинские буквы, цифры и дефис; дефис не по краям',
        {},
        WAIT
      )
    ).toBeVisible();
  });
});

describe('DNS-имя', () => {
  it('собирает DNS-имя базы из выбранного префикса', async () => {
    const user = userEvent.setup();
    await openForm();

    const prefix = screen.getByRole('textbox', { name: /Префикс/ });
    await user.clear(prefix);
    await user.type(prefix, 'shop');

    expect(screen.getByText(/shop-xxxxxx/)).toBeVisible();

    await user.click(screen.getByRole('button', { name: 'Создать базу' }));

    await waitFor(async () => {
      const [created] = await listInstances(TEST_USER_ID);
      expect(created?.slug).toMatch(/^shop-[a-z0-9]{6}$/);
    }, WAIT);
  });
});

describe('тарифная сетка', () => {
  it('показывает готовые конфигурации из тарифной сетки', async () => {
    await openForm();

    expect(plan(1, 1)).toBeVisible();
    expect(plan(1, 2)).toBeVisible();
    expect(plan(1, 4)).toBeVisible();
    expect(plan(2, 4)).toBeVisible();
    expect(plan(2, 8)).toBeVisible();
    expect(plan(4, 16)).toBeVisible();
  });

  it('выбирает процессор и память одной карточкой', async () => {
    const user = userEvent.setup();
    await openForm();

    await user.click(plan(2, 8));

    expect(plan(2, 8)).toBeChecked();
    expect(screen.getByText(wholeText('4 680,00 ₽ в месяц'))).toBeVisible();
  });

  it('учитывает три ноды отказоустойчивого режима в стоимости', async () => {
    const user = userEvent.setup();
    await openForm();

    await user.click(screen.getByRole('radio', { name: /Отказоустойчивый/ }));

    expect(await screen.findByText(wholeText('3 780,00 ₽ в месяц'), {}, WAIT)).toBeVisible();
  });
});

describe('панель стоимости', () => {
  it('пересчитывает цену при смене режима, размера и периода', async () => {
    const user = userEvent.setup();
    await openForm();

    expect(screen.getByText(wholeText('1 260,00 ₽ в месяц'))).toBeVisible();

    await user.click(screen.getByRole('radio', { name: /Отказоустойчивый/ }));
    expect(await screen.findByText(wholeText('3 780,00 ₽ в месяц'), {}, WAIT)).toBeVisible();

    await user.click(screen.getByRole('radio', { name: /Одна нода/ }));
    await user.click(plan(1, 2));
    expect(await screen.findByText(wholeText('1 620,00 ₽ в месяц'), {}, WAIT)).toBeVisible();

    await user.click(screen.getByRole('radio', { name: 'День' }));
    expect(await screen.findByText(wholeText('54,00 ₽ в день'), {}, WAIT)).toBeVisible();

    await user.click(screen.getByRole('radio', { name: 'Час' }));
    expect(await screen.findByText(wholeText('2,25 ₽ в час'), {}, WAIT)).toBeVisible();
  });
});

describe('квота', () => {
  it('показывает цену размера сверх остатка, но не даёт отправить форму', async () => {
    await seedInstance('valkey-0001', 'single', 2, 8);
    const user = userEvent.setup();
    await openForm();

    await user.click(plan(4, 16));

    expect(await screen.findByText(wholeText('9 360,00 ₽ в месяц'), {}, WAIT)).toBeVisible();
    expect(screen.getByText('Недостаточно квоты')).toBeVisible();
    expect(screen.getByText(wholeText('Ваша квота: 4 vCPU и 16 ГБ RAM.'))).toBeVisible();
    expect(screen.getByText(wholeText('Свободно сейчас: 2 vCPU и 8 ГБ RAM.'))).toBeVisible();
    expect(
      screen.getByText(wholeText('Для выбранной конфигурации не хватает: 2 vCPU и 8 ГБ RAM.'))
    ).toBeVisible();
    expect(screen.getByText(wholeText('Одна нода: 2 vCPU и 8 ГБ RAM.'))).toBeVisible();
    expect(
      screen.getByText(
        'Отказоустойчивый режим: свободной квоты не хватает даже на минимальную конфигурацию.'
      )
    ).toBeVisible();
    expect(
      screen.getByRole('link', { name: 'Напишите в поддержку для увеличения квоты' })
    ).toHaveAttribute('href', 'https://t.me/rostislav_dugin');
    expect(screen.getByRole('button', { name: 'Создать базу' })).toBeDisabled();
  });

  it('при исчерпанной квоте отправка недоступна', async () => {
    await seedInstance('valkey-0001', 'single', 4, 16);
    await openForm();

    expect(screen.getByRole('button', { name: 'Создать базу' })).toBeDisabled();
    expect(
      screen.getByRole('link', { name: 'Напишите в поддержку для увеличения квоты' })
    ).toBeVisible();
  });
});

describe('белый список адресов', () => {
  it('проверяет адреса и сохраняет их в формате CIDR', async () => {
    const user = userEvent.setup();
    await openForm();

    await user.click(screen.getByRole('switch', { name: 'Ограничить доступ по IP-адресам' }));
    const addresses = screen.getByRole('textbox', { name: /Разрешённые адреса/ });
    await user.type(addresses, '203.0.113.10\n198.51.100.0/24');

    await user.click(screen.getByRole('button', { name: 'Создать базу' }));

    await waitFor(async () => {
      const [created] = await listInstances(TEST_USER_ID);
      expect(created).toMatchObject({
        isWhitelistEnabled: true,
        whitelistCidrs: ['203.0.113.10/32', '198.51.100.0/24'],
      });
    }, WAIT);
  });
});

describe('отправка формы', () => {
  it('создаёт базу, открывает её карточку и сохраняет данные', async () => {
    const user = userEvent.setup();
    const { router } = await openForm();

    const name = screen.getByRole('textbox', { name: /Имя/ });
    await user.clear(name);
    await user.type(name, 'valkey-1474');
    await user.click(screen.getByRole('button', { name: 'Создать базу' }));

    await waitFor(
      () => expect(router.state.location.pathname).toMatch(/^\/valkey\/management\/[\w-]+$/),
      WAIT
    );
    expect(await screen.findByRole('heading', { name: 'valkey-1474' }, WAIT)).toBeVisible();

    await expect(listInstances(TEST_USER_ID)).resolves.toHaveLength(1);
  });

  it('не создаёт вторую базу при повторном нажатии', async () => {
    const user = userEvent.setup();
    await openForm();

    const submit = screen.getByRole('button', { name: 'Создать базу' });
    await user.click(submit);
    await user.click(submit);

    await waitFor(async () => {
      await expect(listInstances(TEST_USER_ID)).resolves.toHaveLength(1);
    }, WAIT);
  });

  it('ставит ошибку занятого имени у поля', async () => {
    await seedInstance('valkey-1474', 'single', 1, 1);
    const user = userEvent.setup();
    await openForm();

    const name = screen.getByRole('textbox', { name: /Имя/ });
    await user.clear(name);
    await user.type(name, 'valkey-1474');
    await user.click(screen.getByRole('button', { name: 'Создать базу' }));

    expect(await screen.findByText('База с таким именем уже существует', {}, WAIT)).toBeVisible();
    await expect(listInstances(TEST_USER_ID)).resolves.toHaveLength(1);
  });
});
