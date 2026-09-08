import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { getValkeyCredentials, rotateValkeyPassword } from './valkey-credentials';
import { createInstance, readAuditSnapshot, VALKEY_INSTANCES_KEY } from './valkey-storage';

const OWNER = '0199b0b6-0000-7000-8000-000000000001';
const AUTHOR = { userId: OWNER, email: 'owner@example.com' };
const PASSWORD = 'abcdEFGHijklMNOPqrstUVWXyz01_234';
const NEXT_PASSWORD = 'wxyzEFGHijklMNOPqrstUVWXyz01_234';

async function advance<T>(promise: Promise<T>, milliseconds = 400) {
  await vi.advanceTimersByTimeAsync(milliseconds);
  return promise;
}

async function createDatabase() {
  return advance(
    createInstance(AUTHOR, {
      name: 'valkey-1474',
      prefix: 'valkey',
      mode: 'single',
      vcpu: 1,
      ramGb: 1,
      password: PASSWORD,
    })
  );
}

beforeEach(() => {
  localStorage.clear();
  vi.useFakeTimers();
  vi.setSystemTime(new Date('2026-09-08T12:00:00.000Z'));
});

afterEach(() => {
  vi.useRealTimers();
});

describe('временный клиент учётных данных', () => {
  it('переходит из готового состояния в применение и обратно', async () => {
    const instance = await createDatabase();

    await expect(advance(getValkeyCredentials(OWNER, instance.id))).resolves.toEqual({
      username: 'app',
      passwordHint: 'abcd*****',
      passwordVersion: 1,
      appliedPasswordVersion: 1,
    });

    const applying = await advance(
      rotateValkeyPassword(AUTHOR, instance.id, {
        password: NEXT_PASSWORD,
        expectedPasswordVersion: 1,
      })
    );

    expect(applying).toEqual({
      username: 'app',
      passwordHint: 'wxyz*****',
      passwordVersion: 2,
      appliedPasswordVersion: 1,
    });
    await expect(advance(getValkeyCredentials(OWNER, instance.id))).resolves.toMatchObject({
      passwordVersion: 2,
      appliedPasswordVersion: 1,
    });

    await vi.advanceTimersByTimeAsync(800);

    await expect(advance(getValkeyCredentials(OWNER, instance.id))).resolves.toMatchObject({
      passwordVersion: 2,
      appliedPasswordVersion: 2,
    });
  });

  it('проверяет версию, формат и незавершённую операцию до записи', async () => {
    const instance = await createDatabase();

    const conflict = rotateValkeyPassword(AUTHOR, instance.id, {
      password: NEXT_PASSWORD,
      expectedPasswordVersion: 0,
    }).catch((error: unknown) => error);
    await vi.advanceTimersByTimeAsync(400);
    expect(await conflict).toMatchObject({ code: 'CONFLICT' });

    const invalid = rotateValkeyPassword(AUTHOR, instance.id, {
      password: 'short',
      expectedPasswordVersion: 1,
    }).catch((error: unknown) => error);
    await vi.advanceTimersByTimeAsync(400);
    expect(await invalid).toMatchObject({ code: 'VALIDATION_FAILED' });

    await advance(
      rotateValkeyPassword(AUTHOR, instance.id, {
        password: NEXT_PASSWORD,
        expectedPasswordVersion: 1,
      })
    );

    const inProgress = rotateValkeyPassword(AUTHOR, instance.id, {
      password: PASSWORD,
      expectedPasswordVersion: 2,
    }).catch((error: unknown) => error);
    await vi.advanceTimersByTimeAsync(400);
    expect(await inProgress).toMatchObject({ code: 'OPERATION_IN_PROGRESS' });

    expect(readAuditSnapshot(OWNER, instance.id).map((item) => item.action)).toEqual([
      'instance.create',
      'instance.password.rotate',
    ]);
  });

  it('не сохраняет полный новый пароль в базе или аудите', async () => {
    const instance = await createDatabase();

    await advance(
      rotateValkeyPassword(AUTHOR, instance.id, {
        password: NEXT_PASSWORD,
        expectedPasswordVersion: 1,
      })
    );

    const serialized = localStorage.getItem(VALKEY_INSTANCES_KEY) ?? '';
    expect(serialized).not.toContain(PASSWORD);
    expect(serialized).not.toContain(NEXT_PASSWORD);
    expect(readAuditSnapshot(OWNER, instance.id)[1]).toEqual(
      expect.objectContaining({
        action: 'instance.password.rotate',
        userEmail: AUTHOR.email,
      })
    );
  });
});
