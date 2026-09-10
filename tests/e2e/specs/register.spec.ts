import { randomUUID } from "node:crypto";
import { expect, test } from "@playwright/test";

function registrationEmail() {
  const runId = (process.env.MV_RUN_ID ?? "manual").toLowerCase().replace(/[^a-z0-9-]/g, "-");
  return `e2e+${runId}-register-${randomUUID().slice(0, 8)}@example.test`;
}

function accountPassword() {
  const password = process.env.MANAGED_VALKEY_E2E_ACCOUNT_PASSWORD;
  if (!password) {
    throw new Error("MANAGED_VALKEY_E2E_ACCOUNT_PASSWORD не задан");
  }
  return password;
}

test("новый пользователь регистрируется через форму и попадает в авторизованную консоль", { tag: "@prod" }, async ({ page }) => {
  const email = registrationEmail();
  const password = accountPassword();

  await page.goto("/auth");
  await expect(page.getByRole("heading", { name: "Вход в консоль" })).toBeVisible();

  await page.getByLabel("Почта").fill(email);
  await page.getByRole("button", { name: "Продолжить" }).click();

  await expect(page.getByRole("heading", { name: "Создайте аккаунт" })).toBeVisible();
  await page.getByLabel("Пароль", { exact: true }).fill(password);
  await page.getByLabel("Повторите пароль").fill(password);
  await page.getByRole("button", { name: "Создать аккаунт" }).click();

  await expect(page).toHaveURL(/\/valkey\/management$/);
  await expect(page.getByTitle(email)).toBeVisible();
});
