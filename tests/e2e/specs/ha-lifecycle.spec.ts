import { expect, test } from '../src/fixtures.ts';
import {
  assertValkeyReadOnly,
  minimumSize,
  nextSize,
  scenarioIdentity,
  waitForConnectionClose,
  waitForRejectedPassword,
} from '../src/scenario.ts';
import { connectValkey, disconnectValkey } from '../src/valkey.ts';

const personalQuota = { vcpu: 4, ramGb: 12 };

test(
  'пользователь проходит полный жизненный цикл HA-инстанса',
  { tag: '@prod' },
  async ({ console, page }) => {
    const account = await console.register('ha');
    await console.openCreatePage();
    const catalog = await console.readSizeCatalog();
    const available = catalog.filter(
      (size) => size.vcpu * 3 <= personalQuota.vcpu && size.ramGb * 3 <= personalQuota.ramGb
    );
    const initialSize = minimumSize(available);
    const largerSize = nextSize(available, initialSize);
    const identity = scenarioIdentity('ha');

    const created = await console.submitCreation(account, {
      mode: 'ha',
      size: initialSize,
      ...identity,
    });
    await expect(page.getByText('Создаётся', { exact: true }).first()).toBeVisible();
    await console.assertConfigurationDisabled();
    await console.closePasswordWindow();
    await console.waitForRunningSize(initialSize);
    const connection = await console.readConnection(created.password, true);
    if (!connection.readOnly) {
      throw new Error('Интерфейс HA не показал адрес только для чтения');
    }
    expect(connection.readOnly.host).not.toBe(connection.primary.host);
    expect(await console.readTotalResources()).toEqual({
      vcpu: initialSize.vcpu * 3,
      ramGb: initialSize.ramGb * 3,
    });
    await assertValkeyReadOnly(connection.primary, connection.readOnly, 'ha-created', initialSize);

    await console.resize(largerSize);
    expect(await console.readTotalResources()).toEqual({
      vcpu: largerSize.vcpu * 3,
      ramGb: largerSize.ramGb * 3,
    });
    await assertValkeyReadOnly(connection.primary, connection.readOnly, 'ha-grown', largerSize);

    await console.resize(initialSize);
    expect(await console.readTotalResources()).toEqual({
      vcpu: initialSize.vcpu * 3,
      ramGb: initialSize.ramGb * 3,
    });
    await assertValkeyReadOnly(connection.primary, connection.readOnly, 'ha-shrunk', initialSize);

    const oldPrimary = await connectValkey(connection.primary);
    let oldReadOnly: Awaited<ReturnType<typeof connectValkey>> | undefined;
    let nextPassword: string;
    try {
      oldReadOnly = await connectValkey(connection.readOnly);
      nextPassword = await console.rotatePassword(created.password);
      await Promise.all([
        waitForConnectionClose(oldPrimary),
        waitForConnectionClose(oldReadOnly),
        waitForRejectedPassword(connection.primary),
        waitForRejectedPassword(connection.readOnly),
      ]);
    } finally {
      await Promise.all([
        disconnectValkey(oldPrimary),
        ...(oldReadOnly ? [disconnectValkey(oldReadOnly)] : []),
      ]);
    }

    const currentPrimary = { ...connection.primary, password: nextPassword };
    const currentReadOnly = { ...connection.readOnly, password: nextPassword };
    await assertValkeyReadOnly(currentPrimary, currentReadOnly, 'ha-rotated', initialSize);
    await console.deleteInstance(account, created);
    await console.waitForEmptyManagement();
    await Promise.all([
      waitForRejectedPassword(currentPrimary),
      waitForRejectedPassword(currentReadOnly),
    ]);
  }
);
