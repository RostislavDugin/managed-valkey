import { expect, test } from "../src/fixtures.ts";
import {
  assertValkeyReadWrite,
  assertValkeySharedPrimary,
  minimumSize,
  nextSize,
  scenarioIdentity,
  waitForConnectionClose,
  waitForRejectedPassword,
} from "../src/scenario.ts";
import { connectValkey, disconnectValkey } from "../src/valkey.ts";

test(
  "пользователь проходит полный жизненный цикл single-инстанса",
  { tag: "@prod" },
  async ({ console, page }) => {
    const account = await console.register("single");
    await console.openCreatePage();
    const catalog = await console.readSizeCatalog();
    const initialSize = minimumSize(catalog);
    const largerSize = nextSize(catalog, initialSize);
    const identity = scenarioIdentity("single");
    const initialMaintenance = { dow: 2, hourUtc: 3, durationMin: 60 };

    const created = await console.submitCreation(account, {
      mode: "single",
      size: initialSize,
      maintenance: initialMaintenance,
      ...identity,
    });
    await expect(page.getByText("Создаётся", { exact: true }).first()).toBeVisible();
    await console.assertConfigurationDisabled();
    await console.closePasswordWindow();
    await console.waitForRunningSize(initialSize);
    await expect(page.getByText("Primary", { exact: true })).toBeVisible();
    await expect(page.getByText("Для чтения", { exact: true })).toBeVisible();
    await expect(page.getByText("Белый список", { exact: true })).toBeVisible();
    await expect(page.getByText("Создано", { exact: true })).toBeVisible();
    await expect(page.getByText("Создана", { exact: true })).toHaveCount(0);
    await expect.poll(() => console.propertyText("Primary")).toMatch(/^redis:\/\//);
    await expect.poll(() => console.propertyText("Для чтения")).toMatch(/^redis:\/\//);
    await expect
      .poll(() => console.propertyText("Окно обслуживания"))
      .toContain("День 2, 3:00 UTC, 60 мин.");
    await assertValkeySharedPrimary(
      created.connection.primary,
      created.connection.readOnly,
      "single-created",
      initialSize,
    );

    await console.updateMaintenance({ dow: 4, hourUtc: 5, durationMin: 90 });
    await expect
      .poll(() => console.propertyText("Окно обслуживания"))
      .toContain("День 4, 5:00 UTC, 90 мин.");
    await console.actions.goto(page.url());
    await expect
      .poll(() => console.propertyText("Окно обслуживания"))
      .toContain("День 4, 5:00 UTC, 90 мин.");

    await console.resize(largerSize);
    await assertValkeyReadWrite(created.connection.primary, "single-grown", largerSize);

    await console.resize(initialSize);
    await assertValkeyReadWrite(created.connection.primary, "single-shrunk", initialSize);

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
    const currentReadEndpoint = { ...created.connection.readOnly, password: nextPassword };
    await assertValkeyReadWrite(currentEndpoint, "single-rotated", initialSize);
    await console.deleteInstance(account, created);
    await console.waitForEmptyManagement();
    await Promise.all([
      waitForRejectedPassword(currentEndpoint),
      waitForRejectedPassword(currentReadEndpoint),
    ]);
  },
);
