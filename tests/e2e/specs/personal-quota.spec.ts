import { expect, test } from '../src/fixtures.ts';
import { minimumSize, scenarioIdentity } from '../src/scenario.ts';

const personalQuota = { vcpu: 4, ramGb: 16 };

test(
  'личная квота запрещает недопустимое создание и изменение размера',
  { tag: '@local' },
  async ({ console, page }) => {
    const account = await console.register('personal-quota');
    await console.openCreatePage();
    const catalog = await console.readSizeCatalog();
    const minimum = minimumSize(catalog);
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

    const first = await console.submitCreation(account, {
      mode: 'single',
      size: minimum,
      ...scenarioIdentity('quota-a'),
    });
    await console.closePasswordWindow();
    await console.waitForRunningSize(minimum);

    await console.openCreatePage();
    const second = await console.submitCreation(account, {
      mode: 'single',
      size: minimum,
      ...scenarioIdentity('quota-b'),
    });
    await console.closePasswordWindow();
    await console.waitForRunningSize(minimum);

    const forbiddenResize = catalog.find(
      (size) =>
        size.vcpu <= personalQuota.vcpu &&
        size.ramGb <= personalQuota.ramGb &&
        (size.vcpu + minimum.vcpu > personalQuota.vcpu ||
          size.ramGb + minimum.ramGb > personalQuota.ramGb)
    );
    if (!forbiddenResize) {
      throw new Error('Каталог не содержит конфигурацию для проверки квоты изменения размера');
    }

    await console.openInstance(first.name);
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

    await console.openInstance(second.name);
    await console.deleteInstance(account, second);
    await console.waitForQuotaUsage(minimum);
    await console.openInstance(first.name);
    await console.resize(forbiddenResize);
    await console.deleteInstance(account, first);
    await console.waitForEmptyManagement();
  }
);
