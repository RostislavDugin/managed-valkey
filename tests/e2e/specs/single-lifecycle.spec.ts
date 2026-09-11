import { expect, test } from '../src/fixtures.ts';
import {
  assertValkeyReadWrite,
  minimumSize,
  nextSize,
  scenarioIdentity,
  waitForConnectionClose,
  waitForRejectedPassword,
} from '../src/scenario.ts';
import { connectValkey, disconnectValkey } from '../src/valkey.ts';

test(
  'пользователь проходит полный жизненный цикл single-инстанса',
  { tag: '@prod' },
  async ({ console, page }) => {
    const account = await console.register('single');
    await console.openCreatePage();
    const catalog = await console.readSizeCatalog();
    const initialSize = minimumSize(catalog);
    const largerSize = nextSize(catalog, initialSize);
    const identity = scenarioIdentity('single');

    const created = await console.submitCreation(account, {
      mode: 'single',
      size: initialSize,
      ...identity,
    });
    await expect(page.getByText('Создаётся', { exact: true }).first()).toBeVisible();
    await console.assertConfigurationDisabled();
    await console.closePasswordWindow();
    await console.waitForRunningSize(initialSize);
    await expect(page.getByText('Адрес только для чтения', { exact: true })).toHaveCount(0);
    await assertValkeyReadWrite(created.connection.primary, 'single-created', initialSize);

    await console.resize(largerSize);
    await assertValkeyReadWrite(created.connection.primary, 'single-grown', largerSize);

    await console.resize(initialSize);
    await assertValkeyReadWrite(created.connection.primary, 'single-shrunk', initialSize);

    const oldConnection = await connectValkey(created.connection.primary);
    let nextPassword: string;
    try {
      nextPassword = await console.rotatePassword(created.password);
      await waitForConnectionClose(oldConnection);
      await waitForRejectedPassword(created.connection.primary);
    } finally {
      await disconnectValkey(oldConnection);
    }

    const currentEndpoint = { ...created.connection.primary, password: nextPassword };
    await assertValkeyReadWrite(currentEndpoint, 'single-rotated', initialSize);
    await console.deleteInstance(account, created);
    await console.waitForEmptyManagement();
    await waitForRejectedPassword(currentEndpoint);
  }
);
