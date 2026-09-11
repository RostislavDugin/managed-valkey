import assert from 'node:assert/strict';
import test from 'node:test';
import { UserActionGate, type ActionClock } from '../src/user-actions.ts';

test('headed-планировщик начинает второе UI-действие не раньше чем через три секунды', async () => {
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
  const gate = new UserActionGate(3_000, clock);

  await gate.run(async () => {
    starts.push(now);
  });
  now += 450;
  await gate.run(async () => {
    starts.push(now);
  });

  assert.deepEqual(starts, [0, 3_000]);
  assert.deepEqual(sleeps, [2_550]);
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
