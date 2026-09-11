import { expect, test } from '../src/fixtures.ts';

test(
  'новый пользователь регистрируется через форму и попадает в авторизованную консоль',
  { tag: '@prod' },
  async ({ console, page }) => {
    const account = await console.register('register');

    await expect(page).toHaveURL(/\/valkey\/management$/);
    await expect(page.getByTitle(account.email).first()).toBeVisible();
  }
);
