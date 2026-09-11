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
  private readonly scrollPauseMs: number;

  constructor(page: Page, intervalMs: number, scrollPauseMs = 0, clock?: ActionClock) {
    if (!Number.isFinite(scrollPauseMs) || scrollPauseMs < 0) {
      throw new Error('Пауза после прокрутки должна быть неотрицательным числом');
    }
    this.page = page;
    this.gate = new UserActionGate(intervalMs, clock);
    this.scrollPauseMs = scrollPauseMs;
  }

  goto(url: string): Promise<Response | null> {
    return this.gate.run(() => this.page.goto(url));
  }

  click(locator: Locator): Promise<void> {
    return this.runOnLocator(locator, () => locator.click());
  }

  fill(locator: Locator, value: string): Promise<void> {
    return this.runOnLocator(locator, () => locator.fill(value));
  }

  check(locator: Locator): Promise<void> {
    return this.runOnLocator(locator, () => locator.check());
  }

  selectOption(locator: Locator, value: string): Promise<string[]> {
    return this.runOnLocator(locator, () => locator.selectOption(value));
  }

  private runOnLocator<T>(locator: Locator, action: () => Promise<T>): Promise<T> {
    return this.gate.run(async () => {
      await this.scrollTo(locator);
      return action();
    });
  }

  private async scrollTo(locator: Locator): Promise<void> {
    if (this.scrollPauseMs === 0) {
      await locator.scrollIntoViewIfNeeded();
      return;
    }
    await locator.evaluate((element) => {
      element.scrollIntoView({ behavior: 'smooth', block: 'center', inline: 'nearest' });
    });
    await this.page.waitForTimeout(this.scrollPauseMs);
  }
}

function millisecondsFromEnvironment(name: string) {
  const value = process.env[name] ?? '0';
  if (!/^\d+$/.test(value)) {
    throw new Error(`${name} должен содержать целое число миллисекунд`);
  }
  return Number.parseInt(value, 10);
}

export function actionDelayFromEnvironment() {
  return millisecondsFromEnvironment('MANAGED_VALKEY_E2E_ACTION_DELAY_MS');
}

export function scrollPauseFromEnvironment() {
  return millisecondsFromEnvironment('MANAGED_VALKEY_E2E_SCROLL_PAUSE_MS');
}
