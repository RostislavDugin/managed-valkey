import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { renderValkeySection, seedSession, TEST_USER_ID, wholeText } from '../../../../test/render';
import { createInstance } from '../api/valkey-storage';

const WAIT = { timeout: 10_000 };

function useNarrowScreen() {
  vi.spyOn(window, 'matchMedia').mockImplementation(
    (query) =>
      ({
        matches: false,
        media: query,
        onchange: null,
        addListener: () => {},
        removeListener: () => {},
        addEventListener: () => {},
        removeEventListener: () => {},
        dispatchEvent: () => false,
      }) as unknown as MediaQueryList
  );
}

beforeEach(() => {
  seedSession();
});

describe('узкий экран', () => {
  it('открывает панель использования из выдвижного блока', async () => {
    await createInstance(TEST_USER_ID, {
      name: 'valkey-1474',
      prefix: 'valkey',
      mode: 'single',
      vcpu: 1,
      ramGb: 2,
    });
    useNarrowScreen();
    const user = userEvent.setup();

    renderValkeySection('/valkey/management');

    const trigger = await screen.findByRole('button', { name: 'Квота' }, WAIT);
    expect(screen.queryByText('Квота', { selector: 'h2' })).toBeNull();

    await user.click(trigger);

    const drawer = await screen.findByRole('dialog', {}, WAIT);
    expect(within(drawer).getByText('1 / 4')).toBeVisible();
    expect(within(drawer).getByText('2 ГБ / 16 ГБ')).toBeVisible();
  });

  it('открывает панель стоимости из выдвижного блока', async () => {
    useNarrowScreen();
    const user = userEvent.setup();

    renderValkeySection('/valkey/management/new');

    await screen.findByRole('heading', { name: 'Новая Valkey база' }, WAIT);
    await user.click(screen.getByRole('button', { name: 'Стоимость' }));

    const drawer = await screen.findByRole('dialog', {}, WAIT);
    expect(within(drawer).getByText(wholeText('1 260,00 ₽ в месяц'))).toBeVisible();
  });
});

describe('доступность', () => {
  it('у каждой кнопки и поля списка есть подпись', async () => {
    await createInstance(TEST_USER_ID, {
      name: 'valkey-1474',
      prefix: 'valkey',
      mode: 'single',
      vcpu: 1,
      ramGb: 2,
    });

    renderValkeySection('/valkey/management');

    await screen.findByRole('table', {}, WAIT);

    for (const control of [...screen.getAllByRole('button'), ...screen.getAllByRole('textbox')]) {
      expect(control).toHaveAccessibleName();
    }
  });

  it('у каждого контрола формы создания есть подпись', async () => {
    renderValkeySection('/valkey/management/new');

    await screen.findByRole('heading', { name: 'Новая Valkey база' }, WAIT);

    for (const control of [
      ...screen.getAllByRole('button'),
      ...screen.getAllByRole('textbox'),
      ...screen.queryAllByRole('slider'),
      ...screen.getAllByRole('radio'),
      ...screen.getAllByRole('switch'),
    ]) {
      expect(control).toHaveAccessibleName();
    }
  });
});
