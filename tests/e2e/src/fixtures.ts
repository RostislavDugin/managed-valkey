import { expect, test as base } from '@playwright/test';
import { AccountRegistry } from './accounts.ts';
import { cleanupAccounts, ConsoleDriver } from './console.ts';
import { FaultRegistry, KubernetesController } from './kubernetes.ts';
import {
  actionDelayFromEnvironment,
  scrollPauseFromEnvironment,
  UserActions,
} from './user-actions.ts';

interface E2EFixtures {
  accounts: AccountRegistry;
  actions: UserActions;
  console: ConsoleDriver;
  faults: FaultRegistry;
  kubernetes: KubernetesController;
  _faultCleanup: void;
  _safeFailureScreenshot: void;
}

export const test = base.extend<E2EFixtures>({
  accounts: async ({ browser, baseURL }, use) => {
    if (!baseURL) {
      throw new Error('Playwright baseURL не задан');
    }
    const registry = new AccountRegistry();
    await use(registry);
    await cleanupAccounts(
      browser,
      baseURL,
      actionDelayFromEnvironment(),
      scrollPauseFromEnvironment(),
      registry
    );
  },
  actions: async ({ page }, use) => {
    await use(
      new UserActions(page, actionDelayFromEnvironment(), scrollPauseFromEnvironment())
    );
  },
  console: async ({ page, actions, accounts }, use) => {
    await use(new ConsoleDriver(page, actions, accounts));
  },
  faults: async ({}, use) => {
    await use(new FaultRegistry());
  },
  kubernetes: async ({ faults }, use) => {
    await use(new KubernetesController(faults));
  },
  _faultCleanup: [
    async ({ faults, accounts: _accounts }, use) => {
      await use();
      await faults.restoreAll();
    },
    { auto: true },
  ],
  _safeFailureScreenshot: [
    async ({ page, accounts: _accounts }, use, testInfo) => {
      await use();
      if (testInfo.status === testInfo.expectedStatus || page.isClosed()) {
        return;
      }
      const masks = [
        page.locator('input[type="password"]'),
        page.getByLabel('Пароль базы'),
        page.getByLabel('Новый пароль'),
      ];
      await page.screenshot({
        path: testInfo.outputPath('safe-failure.png'),
        fullPage: true,
        mask: masks,
      });
    },
    { auto: true },
  ],
});

export { expect };
