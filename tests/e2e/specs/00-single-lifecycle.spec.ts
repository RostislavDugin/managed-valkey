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
    const initialMaintenance = { dow: 2, time: "04:00" };

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
    const connectionScheme = new URL(page.url()).protocol === "https:" ? "rediss" : "redis";
    await expect(page.getByText("Адрес для записи и чтения", { exact: true })).toBeVisible();
    await expect(page.getByText("Адрес только для чтения", { exact: true })).toBeVisible();
    await expect(page.getByText("Белый список", { exact: true })).toBeVisible();
    await expect(page.getByText("Создано", { exact: true })).toBeVisible();
    await expect(page.getByText("Создана", { exact: true })).toHaveCount(0);
    await expect
      .poll(() => console.propertyText("Адрес для записи и чтения"))
      .toMatch(new RegExp(`^${connectionScheme}://`));
    await expect
      .poll(() => console.propertyText("Адрес только для чтения"))
      .toMatch(new RegExp(`^${connectionScheme}://`));
    await expect
      .poll(() => console.propertyText("Окно обслуживания"))
      .toContain("Вторник, 04:00 по местному времени");
    await assertValkeySharedPrimary(
      created.connection.primary,
      created.connection.readOnly,
      "single-created",
      initialSize,
    );

    await console.updateMaintenance({ dow: 4, time: "05:00" });
    await expect
      .poll(() => console.propertyText("Окно обслуживания"))
      .toContain("Четверг, 05:00 по местному времени");
    await console.actions.goto(page.url());
    await expect
      .poll(() => console.propertyText("Окно обслуживания"))
      .toContain("Четверг, 05:00 по местному времени");

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
