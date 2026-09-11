import type { Locator, Page, Response } from '@playwright/test';

export interface ActionClock {
  now: () => number;
  sleep: (milliseconds: number) => Promise<void>;
}

const systemClock: ActionClock = {
  now: Date.now,
  sleep: (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds)),
};

export class UserActionGate {
  private lastFinishedAt: number | null = null;
  private readonly intervalMs: number;
  private readonly clock: ActionClock;

  constructor(intervalMs: number, clock: ActionClock = systemClock) {
    if (!Number.isFinite(intervalMs) || intervalMs < 0) {
      throw new Error('Интервал пользовательских действий должен быть неотрицательным числом');
    }
    this.intervalMs = intervalMs;
    this.clock = clock;
  }

  async run<T>(action: () => Promise<T>): Promise<T> {
    if (this.lastFinishedAt !== null) {
      const remaining = this.intervalMs - (this.clock.now() - this.lastFinishedAt);
      if (remaining > 0) {
        await this.clock.sleep(remaining);
      }
    }

    try {
      return await action();
    } finally {
      this.lastFinishedAt = this.clock.now();
    }
  }
}

export class UserActions {
  private readonly gate: UserActionGate;
  private readonly page: Page;

  constructor(page: Page, intervalMs: number, clock?: ActionClock) {
    this.page = page;
    this.gate = new UserActionGate(intervalMs, clock);
  }

  goto(url: string): Promise<Response | null> {
    return this.gate.run(() => this.page.goto(url));
  }

  click(locator: Locator): Promise<void> {
    return this.gate.run(() => locator.click());
  }

  fill(locator: Locator, value: string): Promise<void> {
    return this.gate.run(() => locator.fill(value));
  }

  check(locator: Locator): Promise<void> {
    return this.gate.run(() => locator.check());
  }

  selectOption(locator: Locator, value: string): Promise<string[]> {
    return this.gate.run(() => locator.selectOption(value));
  }
}

export function actionDelayFromEnvironment() {
  const value = process.env.MANAGED_VALKEY_E2E_ACTION_DELAY_MS ?? '0';
  if (!/^\d+$/.test(value)) {
    throw new Error('MANAGED_VALKEY_E2E_ACTION_DELAY_MS должен содержать целое число миллисекунд');
  }
  return Number.parseInt(value, 10);
}
