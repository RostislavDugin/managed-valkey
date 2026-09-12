import { ConsoleDriver } from '../src/console.ts';
import { expect, test } from '../src/fixtures.ts';
import { assertValkeyReadWrite, minimumSize, scenarioIdentity } from '../src/scenario.ts';
import { actionDelayFromEnvironment, UserActions } from '../src/user-actions.ts';

interface CapacityResponse {
  cluster: {
    limit: { vcpu: number; ram_gb: number };
    used: { vcpu: number; ram_gb: number };
  };
}

const personalQuota = { vcpu: 4, ramGb: 12 };

test(
  'общая ёмкость кластера отклоняет новый инстанс без влияния на работающие',
  { tag: '@local' },
  async ({ accounts, baseURL, browser, console, page }) => {
    if (!baseURL) {
      throw new Error('Playwright baseURL не задан');
    }

    let activeAccount = await console.register('cluster-capacity-0');
    const capacityResponse = page.waitForResponse(
      (response) =>
        response.request().method() === 'GET' &&
        /\/v1\/managed\/valkey\/capacity$/.test(response.url()) &&
        response.ok()
    );
    await console.openCreatePage();
    const catalog = await console.readSizeCatalog();
    const minimum = minimumSize(catalog);
    const aggregate = (await (await capacityResponse).json()) as CapacityResponse;
    const availableVcpu = aggregate.cluster.limit.vcpu - aggregate.cluster.used.vcpu;
    const availableRamGb = aggregate.cluster.limit.ram_gb - aggregate.cluster.used.ram_gb;
    const capacity = Math.min(
      Math.floor(availableVcpu / minimum.vcpu),
      Math.floor(availableRamGb / minimum.ramGb)
    );
    if (capacity < 2 || capacity >= 32) {
      throw new Error(
        `Суммарный остаток допускает ${capacity} минимальных инстансов; для сценария нужно от 2 до 31`
      );
    }

    const survivors: Array<{
      endpoint: Awaited<ReturnType<typeof console.submitCreation>>['connection']['primary'];
      size: typeof minimum;
    }> = [];
    for (let index = 0; index < capacity - 1; index += 1) {
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
      survivors.push({ endpoint: created.connection.primary, size: minimum });
    }
    await console.logout(activeAccount);

    const candidateAccount = await console.register('cluster-capacity-candidate');
    await console.openCreatePage();
    await console.prepareCreation({
      mode: 'single',
      size: minimum,
      ...scenarioIdentity('capacity-candidate'),
    });
    await expect(page.getByRole('button', { name: 'Создать базу', exact: true })).toBeEnabled();
    let candidateCreateRequests = 0;
    const countCandidateRequests = (request: { method: () => string; url: () => string }) => {
      if (request.method() === 'POST' && /\/v1\/managed\/valkey\/instances$/.test(request.url())) {
        candidateCreateRequests += 1;
      }
    };
    page.on('request', countCandidateRequests);

    const competitorContext = await browser.newContext({ baseURL });
    const competitorPage = await competitorContext.newPage();
    const competitorConsole = new ConsoleDriver(
      competitorPage,
      new UserActions(competitorPage, actionDelayFromEnvironment()),
      accounts
    );
    const competitorAccount = await competitorConsole.register('cluster-capacity-competitor');
    await competitorConsole.openCreatePage();
    const competitor = await competitorConsole.submitCreation(competitorAccount, {
      mode: 'single',
      size: minimum,
      ...scenarioIdentity('capacity-rival'),
    });
    await competitorConsole.closePasswordWindow();
    await competitorConsole.waitForRunningSize(minimum);
    await competitorContext.close();

    await expect(
      page.getByRole('alert').filter({ hasText: 'Недостаточно ресурсов Managed Kubernetes' })
    ).toBeVisible({ timeout: 10_000 });
    await expect(page.getByRole('button', { name: 'Создать базу', exact: true })).toBeDisabled();
    expect(candidateCreateRequests).toBe(0);

    const forbiddenHA = catalog.find(
      (size) => size.vcpu * 3 > personalQuota.vcpu || size.ramGb * 3 > personalQuota.ramGb
    );
    if (!forbiddenHA) {
      throw new Error('Каталог не содержит HA-конфигурацию для проверки личной квоты');
    }
    await console.chooseMode('ha');
    await console.chooseSize(forbiddenHA);
    await expect(page.getByRole('alert').filter({ hasText: 'Недостаточно квоты' })).toBeVisible();
    await expect(
      page.getByRole('alert').filter({ hasText: 'Недостаточно ресурсов Managed Kubernetes' })
    ).toHaveCount(0);
    expect(candidateCreateRequests).toBe(0);
    page.off('request', countCandidateRequests);
    await console.waitForEmptyManagement();
    expect(candidateAccount.instances.size).toBe(0);

    for (const [index, instance] of survivors.entries()) {
      await assertValkeyReadWrite(instance.endpoint, `capacity-survivor-${index}`, instance.size);
    }
    await assertValkeyReadWrite(
      competitor.connection.primary,
      'capacity-competitor-survivor',
      minimum
    );
  }
);
