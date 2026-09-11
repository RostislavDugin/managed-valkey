import { expect, test } from '../src/fixtures.ts';
import { assertValkeyReadWrite, minimumSize, scenarioIdentity } from '../src/scenario.ts';

function positiveBudget(name: 'MANAGED_K8S_NODE_VCPU' | 'MANAGED_K8S_NODE_RAM_GB') {
  const raw = process.env[name];
  if (!raw || !/^\d+$/.test(raw) || Number.parseInt(raw, 10) <= 0) {
    throw new Error(`${name} должен содержать положительное целое число`);
  }
  return Number.parseInt(raw, 10);
}

test(
  'общая ёмкость кластера отклоняет новый инстанс без влияния на работающие',
  { tag: '@local' },
  async ({ console, page }) => {
    let activeAccount = await console.register('cluster-capacity-0');
    await console.openCreatePage();
    const minimum = minimumSize(await console.readSizeCatalog());
    const capacity = Math.min(
      Math.floor(positiveBudget('MANAGED_K8S_NODE_VCPU') / minimum.vcpu),
      Math.floor(positiveBudget('MANAGED_K8S_NODE_RAM_GB') / minimum.ramGb)
    );
    if (capacity < 1 || capacity >= 32) {
      throw new Error(
        `Общий бюджет даёт ${capacity} минимальных инстансов; для сценария нужно от 1 до 31`
      );
    }

    const accepted: Array<{
      endpoint: Awaited<ReturnType<typeof console.submitCreation>>['connection']['primary'];
      size: typeof minimum;
    }> = [];
    for (let index = 0; index < capacity; index += 1) {
      if (index > 0) {
        await console.logout(activeAccount);
        activeAccount = await console.register(`cluster-capacity-${index}`);
        await console.openCreatePage();
      }
      const created = await console.submitCreation(activeAccount, {
        mode: 'single',
        size: minimum,
        ...scenarioIdentity(`capacity-${index}`),
      });
      await console.closePasswordWindow();
      await console.waitForRunningSize(minimum);
      accepted.push({ endpoint: created.connection.primary, size: minimum });
    }

    await console.logout(activeAccount);
    const rejectedAccount = await console.register('cluster-capacity-rejected');
    await console.openCreatePage();
    const rejectedIdentity = scenarioIdentity('capacity-rejected');
    await console.prepareCreation({ mode: 'single', size: minimum, ...rejectedIdentity });
    const responsePromise = page.waitForResponse(
      (response) =>
        response.request().method() === 'POST' &&
        /\/v1\/managed\/valkey\/instances$/.test(response.url())
    );
    await console.actions.click(page.getByRole('button', { name: 'Создать базу', exact: true }));
    const response = await responsePromise;
    expect(response.status()).toBe(422);
    await expect(response.json()).resolves.toMatchObject({
      error: {
        code: 'NOT_ENOUGH_RESOURCES',
        details: { reason: 'cluster_quota' },
      },
    });
    await expect(page.getByText('В кластере сейчас не хватает свободных ресурсов.')).toBeVisible();
    await console.waitForEmptyManagement();
    expect(rejectedAccount.instances.size).toBe(0);

    for (const [index, instance] of accepted.entries()) {
      await assertValkeyReadWrite(instance.endpoint, `capacity-survivor-${index}`, instance.size);
    }
  }
);
