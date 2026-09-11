import { expect, test } from '../src/fixtures.ts';
import { assertValkeyReadWrite, minimumSize, scenarioIdentity } from '../src/scenario.ts';

type PositiveTopologyVariable =
  | 'MANAGED_K8S_NODE_COUNT'
  | 'MANAGED_K8S_NODE_CAPACITY_VCPU'
  | 'MANAGED_K8S_NODE_CAPACITY_RAM_GB';

type NonnegativeTopologyVariable =
  | 'MANAGED_K8S_NODE_RESERVED_CPU_MILLI'
  | 'MANAGED_K8S_NODE_RESERVED_RAM_MIB';

function positiveTopologyValue(name: PositiveTopologyVariable) {
  const raw = process.env[name];
  if (!raw || !/^\d+$/.test(raw) || Number.parseInt(raw, 10) <= 0) {
    throw new Error(`${name} должен содержать положительное целое число`);
  }
  return Number.parseInt(raw, 10);
}

function nonnegativeTopologyValue(name: NonnegativeTopologyVariable) {
  const raw = process.env[name];
  if (!raw || !/^\d+$/.test(raw)) {
    throw new Error(`${name} должен содержать неотрицательное целое число`);
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
    const nodeCount = positiveTopologyValue('MANAGED_K8S_NODE_COUNT');
    const availableCpuMilli =
      positiveTopologyValue('MANAGED_K8S_NODE_CAPACITY_VCPU') * 1000 -
      nonnegativeTopologyValue('MANAGED_K8S_NODE_RESERVED_CPU_MILLI');
    const availableRamMiB =
      positiveTopologyValue('MANAGED_K8S_NODE_CAPACITY_RAM_GB') * 1024 -
      nonnegativeTopologyValue('MANAGED_K8S_NODE_RESERVED_RAM_MIB');
    const capacity = Math.min(
      Math.floor((availableCpuMilli * nodeCount) / (minimum.vcpu * 1000)),
      Math.floor((availableRamMiB * nodeCount) / (minimum.ramGb * 1024))
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
