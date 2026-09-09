import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';
import { renderValkeySection, seedSession } from '../../../../test/render';
import { installStatefulValkeyApi, TEST_INSTANCE_ID } from '../../../../test/valkey-api-fixture';

const WAIT = { timeout: 10_000 };

function expectNamedControls() {
  for (const control of screen.getAllByRole('button')) {
    expect(control).toHaveAccessibleName();
  }
}

describe('доступность управления Valkey', () => {
  it('даёт имена всем кнопкам формы и связывает ошибку с полем', async () => {
    const session = seedSession();
    installStatefulValkeyApi([]);
    const user = userEvent.setup();

    renderValkeySection('/valkey/management/new', session);
    const name = await screen.findByRole('textbox', { name: 'Имя' }, WAIT);
    expectNamedControls();

    await user.clear(name);
    await user.tab();

    expect(await screen.findByText('Введите имя базы', {}, WAIT)).toBeVisible();
    expect(name).toHaveAttribute('aria-invalid', 'true');
    expect(name).toHaveAccessibleDescription('Введите имя базы');
  });

  it('оставляет вкладки и действия карточки доступными с клавиатуры', async () => {
    const session = seedSession();
    installStatefulValkeyApi();
    const user = userEvent.setup();

    renderValkeySection(`/valkey/management/${TEST_INSTANCE_ID}`, session);
    await screen.findByRole('heading', { name: 'cache' }, WAIT);
    await screen.findByRole('button', { name: 'Сменить пароль' }, WAIT);
    expectNamedControls();

    const monitoring = screen.getByRole('tab', { name: 'Мониторинг' });
    monitoring.focus();
    await user.keyboard('{Enter}');

    expect(await screen.findByRole('heading', { name: 'Мониторинг' }, WAIT)).toBeVisible();
    expect(screen.getByText('Метрик пока нет')).toBeVisible();
  });
});
