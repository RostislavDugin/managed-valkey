import { expect, test } from '../src/fixtures.ts';
import { scenarioIdentity } from '../src/scenario.ts';

const personalQuota = { vcpu: 4, ramGb: 12 };

test(
  'личная квота запрещает недопустимое создание и изменение размера',
  { tag: '@local' },
  async ({ console, page }) => {
    const account = await console.register('personal-quota');
    await console.openCreatePage();
    const catalog = await console.readSizeCatalog();
    const minimum = catalog.find((size) => size.vcpu === 1 && size.ramGb === 2);
    const forbiddenResize = catalog.find((size) => size.vcpu === 2 && size.ramGb === 8);
    if (!minimum || !forbiddenResize) {
      throw new Error('Каталог не содержит конфигурации 1 vCPU / 2 GB и 2 vCPU / 8 GB');
    }
    const forbiddenHA = catalog.find(
      (size) => size.vcpu * 3 > personalQuota.vcpu || size.ramGb * 3 > personalQuota.ramGb
    );
    if (!forbiddenHA) {
      throw new Error('Каталог не содержит HA-конфигурацию для проверки личной квоты');
    }

    let forbiddenCreateRequests = 0;
    const countCreateRequest = (request: { method: () => string; url: () => string }) => {
      if (request.method() === 'POST' && /\/v1\/managed\/valkey\/instances$/.test(request.url())) {
        forbiddenCreateRequests += 1;
      }
    };
    page.on('request', countCreateRequest);
    await console.chooseMode('ha');
    await console.chooseSize(forbiddenHA);
    await expect(page.getByRole('alert').filter({ hasText: 'Недостаточно квоты' })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Создать базу', exact: true })).toBeDisabled();
    expect(forbiddenCreateRequests).toBe(0);
    page.off('request', countCreateRequest);

    const single = await console.submitCreation(account, {
      mode: 'single',
      size: minimum,
      ...scenarioIdentity('quota-single'),
    });
    await console.closePasswordWindow();
    await console.waitForRunningSize(minimum);

    await console.openCreatePage();
    const ha = await console.submitCreation(account, {
      mode: 'ha',
      size: minimum,
      ...scenarioIdentity('quota-ha'),
    });
    await console.closePasswordWindow();
    await console.waitForRunningSize(minimum);

    await console.openInstance(single.name);
    const resizeDialog = await console.openResize();
    let forbiddenResizeRequests = 0;
    const countResizeRequest = (request: { method: () => string; url: () => string }) => {
      if (request.method() === 'POST' && /\/resize$/.test(request.url())) {
        forbiddenResizeRequests += 1;
      }
    };
    page.on('request', countResizeRequest);
    await console.chooseSize(forbiddenResize);
    await expect(
      resizeDialog.getByRole('alert').filter({ hasText: 'Не хватает квоты' })
    ).toBeVisible();
    await expect(resizeDialog.getByRole('button', { name: 'Изменить тариф' })).toBeDisabled();
    expect(forbiddenResizeRequests).toBe(0);
    page.off('request', countResizeRequest);
    await console.closeResize();

    await console.openInstance(ha.name);
    await console.deleteInstance(account, ha);
    await console.waitForQuotaUsage(minimum);
    await console.openInstance(single.name);
    await console.resize(forbiddenResize);
    await console.deleteInstance(account, single);
    await console.waitForEmptyManagement();
  }
);
