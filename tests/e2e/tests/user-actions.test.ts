import assert from 'node:assert/strict';
import test from 'node:test';
import type { Locator, Page } from '@playwright/test';
import { UserActionGate, UserActions, type ActionClock } from '../src/user-actions.ts';

test('headed-планировщик начинает второе UI-действие не раньше чем через одну секунду', async () => {
  let now = 0;
  const sleeps: number[] = [];
  const starts: number[] = [];
  const clock: ActionClock = {
    now: () => now,
    sleep: async (milliseconds) => {
      sleeps.push(milliseconds);
      now += milliseconds;
    },
  };
  const gate = new UserActionGate(1_000, clock);

  await gate.run(async () => {
    starts.push(now);
  });
  now += 450;
  await gate.run(async () => {
    starts.push(now);
  });

  assert.deepEqual(starts, [0, 1_000]);
  assert.deepEqual(sleeps, [550]);
});

test('headless-планировщик не ждёт между UI-действиями', async () => {
  let now = 0;
  const sleeps: number[] = [];
  const gate = new UserActionGate(0, {
    now: () => now,
    sleep: async (milliseconds) => {
      sleeps.push(milliseconds);
      now += milliseconds;
    },
  });

  await gate.run(async () => undefined);
  await gate.run(async () => undefined);

  assert.deepEqual(sleeps, []);
});

test('backend-проверка после UI-действия начинается без демонстрационной задержки', async () => {
  let now = 100;
  const events: Array<{ name: string; at: number }> = [];
  const gate = new UserActionGate(3_000, {
    now: () => now,
    sleep: async (milliseconds) => {
      now += milliseconds;
    },
  });

  await gate.run(async () => {
    events.push({ name: 'action', at: now });
  });
  events.push({ name: 'check', at: now });

  assert.deepEqual(events, [
    { name: 'action', at: 100 },
    { name: 'check', at: 100 },
  ]);
});

test('пользовательские действия прокручивают страницу до элемента перед взаимодействием', async () => {
  const events: string[] = [];
  const locator = {
    scrollIntoViewIfNeeded: async () => {
      events.push('scroll');
    },
    click: async () => {
      events.push('click');
    },
    fill: async () => {
      events.push('fill');
    },
    check: async () => {
      events.push('check');
    },
    selectOption: async () => {
      events.push('selectOption');
      return ['selected'];
    },
  } as unknown as Locator;
  const actions = new UserActions({} as Page, 0);

  await actions.click(locator);
  await actions.fill(locator, 'value');
  await actions.check(locator);
  await actions.selectOption(locator, 'selected');

  assert.deepEqual(events, [
    'scroll',
    'click',
    'scroll',
    'fill',
    'scroll',
    'check',
    'scroll',
    'selectOption',
  ]);
});

test('headed-действие плавно прокручивает элемент в центр и ждёт 500 миллисекунд перед нажатием', async () => {
  const events: string[] = [];
  const page = {
    waitForTimeout: async (milliseconds: number) => {
      events.push(`wait:${milliseconds}`);
    },
  } as unknown as Page;
  const locator = {
    evaluate: async (
      callback: (element: { scrollIntoView: (options: ScrollIntoViewOptions) => void }) => void
    ) => {
      callback({
        scrollIntoView: (options) => {
          events.push(
            `scroll:${options.behavior}:${options.block}:${options.inline}`
          );
        },
      });
    },
    click: async () => {
      events.push('click');
    },
  } as unknown as Locator;
  const actions = new UserActions(page, 1_000, 500);

  await actions.click(locator);

  assert.deepEqual(events, ['scroll:smooth:center:nearest', 'wait:500', 'click']);
});
