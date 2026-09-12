import { randomUUID } from "node:crypto";
import { expect, type Browser, type Locator, type Page } from "@playwright/test";
import { AccountRegistry, type TestAccount } from "./accounts.ts";
import { recordSecret } from "./secrets.ts";
import { UserActions } from "./user-actions.ts";
import type { ValkeyInstanceConnection } from "./valkey.ts";

export type ValkeyMode = "single" | "ha";

export interface ValkeySize {
  vcpu: number;
  ramGb: number;
}

export interface CreatedInstance {
  name: string;
  slug: string;
  password: string;
  connection: ValkeyInstanceConnection;
}

export interface LocalMaintenance {
  dow: number;
  time: string;
}

const operationTimeout = 5 * 60_000;
const modeLabels: Record<ValkeyMode, string> = {
  single: "Одна нода",
  ha: "Отказоустойчивый",
};
const weekdayLabels = [
  "Воскресенье",
  "Понедельник",
  "Вторник",
  "Среда",
  "Четверг",
  "Пятница",
  "Суббота",
];

function accountEmail(scenario: string) {
  const runId = (process.env.MV_RUN_ID ?? "manual").toLowerCase().replace(/[^a-z0-9-]/g, "-");
  return `e2e+${runId}-${scenario}-${randomUUID().slice(0, 8)}@example.test`;
}

function accountPassword() {
  const value = process.env.MANAGED_VALKEY_E2E_ACCOUNT_PASSWORD;
  if (!value) {
    throw new Error("MANAGED_VALKEY_E2E_ACCOUNT_PASSWORD не задан");
  }
  return value;
}

function parseNumber(value: string) {
  return Number.parseInt(value.replaceAll(/\s/g, ""), 10);
}

export function parseSize(value: string): ValkeySize {
  const match = value.match(/([\d\s\u00a0]+)\s*vCPU.*?([\d\s\u00a0]+)\s*ГБ/i);
  if (!match) {
    throw new Error(`Не удалось разобрать конфигурацию из строки «${value}»`);
  }
  return { vcpu: parseNumber(match[1]), ramGb: parseNumber(match[2]) };
}

function endpointFromAddress(address: string, password: string) {
  let parsed: URL;
  try {
    parsed = new URL(address.trim());
  } catch {
    throw new Error(`Не удалось разобрать адрес Valkey из строки «${address}»`);
  }
  if (
    !["redis:", "rediss:"].includes(parsed.protocol) ||
    !parsed.hostname ||
    !parsed.port ||
    parsed.pathname !== ""
  ) {
    throw new Error(`Не удалось разобрать адрес Valkey из строки «${address}»`);
  }
  return { host: parsed.hostname, port: Number.parseInt(parsed.port, 10), password };
}

function sameSize(left: ValkeySize, right: ValkeySize) {
  return left.vcpu === right.vcpu && left.ramGb === right.ramGb;
}

function sizeName(size: ValkeySize) {
  return new RegExp(`${size.vcpu}\\s*vCPU.*${size.ramGb}\\s*ГБ`, "i");
}

export class ConsoleDriver {
  constructor(
    readonly page: Page,
    readonly actions: UserActions,
    readonly accounts: AccountRegistry,
  ) {}

  async register(scenario: string) {
    const email = accountEmail(scenario);
    const password = accountPassword();
    recordSecret(password);

    await this.actions.goto("/auth");
    await expect(this.page.getByRole("heading", { name: "Вход в консоль" })).toBeVisible();
    await this.actions.fill(this.page.getByLabel("Почта"), email);
    await this.submitAuthRequest(
      "/v1/auth/check-email",
      this.page.getByRole("button", { name: "Продолжить" }),
    );
    await expect(this.page.getByRole("heading", { name: "Создайте аккаунт" })).toBeVisible();
    await this.actions.fill(this.page.getByLabel("Пароль", { exact: true }), password);
    await this.actions.fill(this.page.getByLabel("Повторите пароль"), password);

    const account = this.accounts.track(email, password);
    this.accounts.markRegistered(account);
    await this.submitAuthRequest(
      "/v1/auth/register",
      this.page.getByRole("button", { name: "Создать аккаунт" }),
      async () => {
        await this.actions.fill(this.page.getByLabel("Пароль", { exact: true }), password);
        await this.actions.fill(this.page.getByLabel("Повторите пароль"), password);
      },
    );
    await expect(this.page).toHaveURL(/\/valkey\/management$/);
    await expect(this.page.getByTitle(email).first()).toBeVisible();
    return account;
  }

  async login(account: TestAccount) {
    await this.actions.goto("/auth");
    await expect(this.page.getByRole("heading", { name: "Вход в консоль" })).toBeVisible();
    await this.actions.fill(this.page.getByLabel("Почта"), account.email);
    await this.submitAuthRequest(
      "/v1/auth/check-email",
      this.page.getByRole("button", { name: "Продолжить" }),
    );
    await expect(this.page.getByRole("heading", { name: "Введите пароль" })).toBeVisible();
    await this.actions.fill(this.page.getByLabel("Пароль", { exact: true }), account.password);
    await this.submitAuthRequest(
      "/v1/auth/login",
      this.page.getByRole("button", { name: "Войти" }),
      () => this.actions.fill(this.page.getByLabel("Пароль", { exact: true }), account.password),
    );
    await expect(this.page).toHaveURL(/\/valkey\/management$/);
  }

  async logout(account: TestAccount) {
    await this.actions.click(this.page.getByTitle(account.email).first());
    await this.actions.click(this.page.getByRole("menuitem", { name: "Выйти" }));
    await expect(this.page.getByRole("heading", { name: "Вход в консоль" })).toBeVisible();
  }

  async openCreatePage() {
    await this.actions.goto("/valkey/management/new");
    await expect(this.page.getByRole("heading", { name: "Новая Valkey база" })).toBeVisible();
  }

  async readSizeCatalog() {
    const radios = this.page.getByRole("radio").filter({ hasText: /vCPU.*ГБ/i });
    const result: ValkeySize[] = [];
    for (let index = 0; index < (await radios.count()); index += 1) {
      const label = await radios.nth(index).textContent();
      if (!label) {
        continue;
      }
      const size = parseSize(label);
      if (!result.some((candidate) => sameSize(candidate, size))) {
        result.push(size);
      }
    }
    if (result.length === 0) {
      throw new Error("Интерфейс не показал ни одной конфигурации Valkey");
    }
    return [...result].sort((left, right) => left.vcpu - right.vcpu || left.ramGb - right.ramGb);
  }

  async chooseMode(mode: ValkeyMode) {
    await this.actions.click(
      this.page
        .getByRole("radio")
        .filter({ has: this.page.getByText(modeLabels[mode], { exact: true }) }),
    );
  }

  async chooseSize(size: ValkeySize) {
    await this.actions.click(this.page.getByRole("radio").filter({ hasText: sizeName(size) }));
  }

  async submitCreation(
    account: TestAccount,
    input: {
      mode: ValkeyMode;
      size: ValkeySize;
      name: string;
      prefix: string;
      maintenance?: LocalMaintenance;
    },
  ) {
    await this.prepareCreation(input);
    await this.actions.click(this.page.getByRole("button", { name: "Создать базу", exact: true }));

    await expect(this.page.getByRole("heading", { name: input.name })).toBeVisible({
      timeout: operationTimeout,
    });
    await expect(this.page.getByRole("dialog", { name: "Сохраните пароль" })).toBeVisible();
    const password = await this.page.getByLabel("Пароль базы").inputValue();
    recordSecret(password);
    const connection = await this.readConnection(password);
    const slug = connection.primary.host.split(".", 1)[0];
    this.accounts.trackInstance(account, input.name, slug);
    return { name: input.name, slug, password, connection } satisfies CreatedInstance;
  }

  async prepareCreation(input: {
    mode: ValkeyMode;
    size: ValkeySize;
    name: string;
    prefix: string;
    maintenance?: LocalMaintenance;
  }) {
    await this.chooseMode(input.mode);
    await this.chooseSize(input.size);
    await this.actions.fill(
      this.page.getByRole("textbox", { name: "Имя", exact: true }),
      input.name,
    );
    await this.actions.fill(
      this.page.getByRole("textbox", { name: "Префикс", exact: true }),
      input.prefix,
    );
    if (input.maintenance) {
      await this.actions.click(
        this.page.getByRole("combobox", { name: "День недели", exact: true }),
      );
      await this.actions.click(
        this.page.getByRole("option", { name: weekdayLabels[input.maintenance.dow], exact: true }),
      );
      await this.actions.click(
        this.page.getByRole("combobox", { name: "Время начала, местное", exact: true }),
      );
      await this.actions.click(
        this.page.getByRole("option", { name: input.maintenance.time, exact: true }),
      );
    }
  }

  async closePasswordWindow() {
    await this.actions.click(this.page.getByRole("button", { name: "Закрыть", exact: true }));
    await expect(this.page.getByRole("dialog", { name: "Сохраните пароль" })).toBeHidden();
  }

  async readConnection(password: string, _requireReadOnly = true) {
    const primaryAddress = await this.propertyText("Адрес для записи и чтения");
    const readOnlyAddress = await this.propertyText("Адрес только для чтения");
    return {
      primary: endpointFromAddress(primaryAddress, password),
      readOnly: endpointFromAddress(readOnlyAddress, password),
    } satisfies ValkeyInstanceConnection;
  }

  async updateMaintenance(maintenance: LocalMaintenance) {
    await this.actions.click(
      this.page.getByRole("button", { name: "Изменить окно обслуживания", exact: true }),
    );
    const dialog = this.page.getByRole("dialog", { name: "Окно обслуживания" });
    await expect(dialog).toBeVisible();
    await this.actions.click(
      dialog.getByRole("combobox", { name: "День недели", exact: true }),
    );
    await this.actions.click(
      this.page.getByRole("option", { name: weekdayLabels[maintenance.dow], exact: true }),
    );
    await this.actions.click(
      dialog.getByRole("combobox", { name: "Время начала, местное", exact: true }),
    );
    await this.actions.click(
      this.page.getByRole("option", { name: maintenance.time, exact: true }),
    );
    const responsePromise = this.page.waitForResponse(
      (response) =>
        response.request().method() === "PATCH" &&
        /\/v1\/managed\/valkey\/instances\/[^/]+$/.test(response.url()),
    );
    await this.actions.click(dialog.getByRole("button", { name: "Сохранить", exact: true }));
    const response = await responsePromise;
    expect(response.ok()).toBe(true);
    await expect(dialog).toBeHidden();
  }

  async readCurrentSize() {
    return parseSize(await this.propertyText("Текущая конфигурация"));
  }

  async assertConfigurationDisabled() {
    await expect(
      this.page.getByRole("button", { name: "Изменить тариф", exact: true }),
    ).toBeDisabled();
    await expect(
      this.page.getByRole("button", { name: "Изменить пароль", exact: true }),
    ).toBeDisabled();
    await expect(
      this.page.getByRole("button", { name: "Изменить белый список", exact: true }),
    ).toBeDisabled();
    await expect(
      this.page.getByRole("button", { name: "Изменить окно обслуживания", exact: true }),
    ).toBeEnabled();
  }

  async waitForRunning() {
    await expect(this.page.getByText("Работает", { exact: true }).first()).toBeVisible({
      timeout: operationTimeout,
    });
    await expect(
      this.page.getByRole("button", { name: "Изменить тариф", exact: true }),
    ).toBeEnabled({ timeout: operationTimeout });
  }

  async waitForRunningSize(size: ValkeySize) {
    await this.waitForRunning();
    await expect.poll(() => this.readCurrentSize(), { timeout: operationTimeout }).toEqual(size);
  }

  async resize(size: ValkeySize) {
    await this.actions.click(
      this.page.getByRole("button", { name: "Изменить тариф", exact: true }),
    );
    await expect(this.page.getByRole("dialog", { name: "Изменить тариф" })).toBeVisible();
    await this.chooseSize(size);
    const responsePromise = this.page.waitForResponse(
      (response) =>
        response.request().method() === "POST" &&
        /\/v1\/managed\/valkey\/instances\/[^/]+\/resize$/.test(response.url()),
    );
    await this.actions.click(
      this.page.getByRole("dialog", { name: "Изменить тариф" }).getByRole("button", {
        name: "Изменить тариф",
      }),
    );
    const response = await responsePromise;
    expect(response.status()).toBe(202);
    await expect(this.page.getByRole("dialog", { name: "Изменить тариф" })).toBeHidden();
    await this.waitForRunningSize(size);
  }

  async rotatePassword(password: string) {
    await this.actions.click(
      this.page.getByRole("button", { name: "Изменить пароль", exact: true }),
    );
    const dialog = this.page.getByRole("dialog", { name: "Изменить пароль" });
    await expect(dialog).toBeVisible();
    await this.actions.click(dialog.getByLabel("Подтвердить изменение пароля"));
    const revealed = this.page.getByRole("dialog", { name: "Сохраните пароль" });
    await expect(revealed).toBeVisible({ timeout: operationTimeout });
    const nextPassword = await this.page.getByLabel("Новый пароль").inputValue();
    if (nextPassword === password) {
      throw new Error("Ротация вернула прежний пароль");
    }
    recordSecret(nextPassword);
    await this.closePasswordWindow();
    await expect(
      this.page.getByRole("button", { name: "Изменить пароль", exact: true }),
    ).toBeEnabled({ timeout: operationTimeout });
    return nextPassword;
  }

  async openResize() {
    await this.actions.click(
      this.page.getByRole("button", { name: "Изменить тариф", exact: true }),
    );
    const dialog = this.page.getByRole("dialog", { name: "Изменить тариф" });
    await expect(dialog).toBeVisible();
    return dialog;
  }

  async closeResize() {
    const dialog = this.page.getByRole("dialog", { name: "Изменить тариф" });
    await this.actions.click(dialog.getByRole("button", { name: "Отмена" }));
    await expect(dialog).toBeHidden();
  }

  async openInstance(name: string) {
    await this.actions.goto("/valkey/management");
    await this.actions.click(this.page.getByRole("link", { name, exact: true }));
    await expect(this.page.getByRole("heading", { name })).toBeVisible();
  }

  async deleteInstance(account: TestAccount, instance: { name: string; slug: string }) {
    await this.actions.click(
      this.page.getByRole("button", { name: "Действия с базой", exact: true }),
    );
    await this.actions.click(this.page.getByRole("menuitem", { name: "Удалить", exact: true }));
    const dialog = this.page.getByRole("dialog", {
      name: new RegExp(`Удалить базу ${instance.name}`),
    });
    await expect(dialog).toBeVisible();
    await this.actions.fill(dialog.getByLabel(/Введите .* для подтверждения/), instance.slug);
    const responsePromise = this.page.waitForResponse(
      (response) =>
        response.request().method() === "DELETE" &&
        /\/v1\/managed\/valkey\/instances\/[^/]+$/.test(response.url()),
    );
    await this.actions.click(dialog.getByRole("button", { name: "Удалить базу" }));
    const response = await responsePromise;
    expect(response.status()).toBe(202);
    await expect(dialog).toBeHidden({ timeout: operationTimeout });
    await this.actions.goto("/valkey/management");
    await expect(
      this.page.getByRole("heading", {
        name: /^(Базы данных|Управляемые базы Valkey на DDR5)$/,
      }),
    ).toBeVisible({ timeout: operationTimeout });
    await expect(this.page.getByRole("link", { name: instance.name, exact: true })).toHaveCount(0, {
      timeout: operationTimeout,
    });
    this.accounts.forgetInstance(account, instance.slug);
  }

  async waitForEmptyManagement() {
    const quotaResponse = this.page.waitForResponse(
      (response) => response.request().method() === "GET" && /\/v1\/me$/.test(response.url()),
    );
    await this.actions.goto("/valkey/management");
    const quota = await quotaResponse;
    expect(quota.ok()).toBe(true);
    await expect(quota.json()).resolves.toMatchObject({
      usage: { used_vcpu: 0, used_ram_gb: 0 },
    });
    await expect(
      this.page.getByRole("heading", { name: "Управляемые базы Valkey на DDR5" }),
    ).toBeVisible({ timeout: operationTimeout });
    await expect(this.page.getByRole("row")).toHaveCount(0);
  }

  async waitForQuotaUsage(usage: ValkeySize) {
    await expect(
      this.page.getByRole("progressbar", {
        name: new RegExp(`vCPU: занято ${usage.vcpu}\\s*/`),
      }),
    ).toBeVisible({ timeout: operationTimeout });
    await expect(
      this.page.getByRole("progressbar", {
        name: new RegExp(`RAM: занято ${usage.ramGb}\\s*ГБ\\s*/`),
      }),
    ).toBeVisible({ timeout: operationTimeout });
  }

  async propertyText(label: string) {
    const row = this.page.getByRole("group", { name: label, exact: true });
    await expect(row).toBeVisible({ timeout: operationTimeout });
    const value = (await row.textContent())?.slice(label.length).trim();
    if (!value) {
      throw new Error(`Интерфейс не показал значение «${label}»`);
    }
    return value;
  }

  private async submitAuthRequest(
    path: string,
    button: Locator,
    prepareRetry?: () => Promise<void>,
  ) {
    const deadline = Date.now() + operationTimeout;
    while (true) {
      const responsePromise = this.page.waitForResponse(
        (response) => response.request().method() === "POST" && response.url().endsWith(path),
      );
      await this.actions.click(button);
      const response = await responsePromise;
      if (response.status() !== 429) {
        return;
      }

      const payload = (await response.json()) as {
        error?: { details?: { retry_after?: unknown } };
      };
      const retryAfter = payload.error?.details?.retry_after;
      if (typeof retryAfter !== "number" || retryAfter < 1) {
        throw new Error(`Ответ ${path} не содержит допустимый retry_after`);
      }
      if (Date.now() + retryAfter * 1_000 > deadline) {
        throw new Error(`Лимит запросов ${path} не освободился до конечного срока`);
      }
      await this.page.waitForTimeout(retryAfter * 1_000 + 100);
      await prepareRetry?.();
    }
  }
}

export async function cleanupAccounts(
  browser: Browser,
  baseURL: string,
  intervalMs: number,
  scrollPauseMs: number,
  registry: AccountRegistry,
) {
  const failures: string[] = [];
  for (const account of registry.all().filter((candidate) => candidate.registered)) {
    const context = await browser.newContext({ baseURL });
    const page = await context.newPage();
    const console = new ConsoleDriver(
      page,
      new UserActions(page, intervalMs, scrollPauseMs),
      registry,
    );
    const observed = new Map(account.instances);
    try {
      await console.login(account);
      await console.actions.goto("/valkey/management");
      await expect(
        page.getByRole("heading", {
          name: /^(Базы данных|Управляемые базы Valkey на DDR5)$/,
        }),
      ).toBeVisible({ timeout: operationTimeout });
      while ((await page.getByRole("row").count()) > 1) {
        const row = page
          .getByRole("row")
          .filter({ has: page.getByRole("link") })
          .first();
        const link = row.getByRole("link").first();
        const name = (await link.textContent())?.trim() || "неизвестное имя";
        await console.actions.click(link);
        const address = await console.propertyText("Адрес для записи и чтения");
        const slug = endpointFromAddress(address, "").host.split(".", 1)[0];
        observed.set(slug, { name, slug });
        await console.deleteInstance(account, { name, slug });
      }
      await console.waitForEmptyManagement();
    } catch (error) {
      const resources = [...observed.values()]
        .map((instance) => `${instance.name} (${instance.slug})`)
        .join(", ");
      const reason = error instanceof Error ? error.message : String(error);
      failures.push(
        `${account.email}: ${resources || "видимые инстансы не определены"}; причина: ${reason}`,
      );
    } finally {
      await context.close();
    }
  }
  if (failures.length > 0) {
    throw new Error(`UI-очистка не завершилась: ${failures.join("; ")}`);
  }
}
