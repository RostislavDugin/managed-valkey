import { beforeEach, describe, expect, it } from 'vitest';
import { ApiError } from '@/shared/api';
import type { ValkeyInstance } from '../model/valkey';
import type { AuditLogEntry } from '../model/valkey-observability';
import {
  createInstance as createStoredInstance,
  type CreateInstanceInput,
  deleteInstance,
  getInstance,
  listInstances,
  readAuditSnapshot,
  renameInstance,
  resizeInstance,
  VALKEY_INSTANCES_KEY,
} from './valkey-storage';

const OWNER = '0199b0b6-0000-7000-8000-000000000001';
const OTHER_OWNER = '0199b0b6-0000-7000-8000-000000000002';
const ACTOR = { userId: OWNER, email: 'owner@example.com' };
const OTHER_ACTOR = { userId: OTHER_OWNER, email: 'other@example.com' };
const PASSWORD = 'abcdEFGHijklMNOPqrstUVWXyz01_234';

function createInstance(
  author: typeof ACTOR,
  input: Omit<CreateInstanceInput, 'password'> & { password?: string }
) {
  return createStoredInstance(author, { password: PASSWORD, ...input });
}

function writeRaw(value: unknown) {
  localStorage.setItem(VALKEY_INSTANCES_KEY, JSON.stringify(value));
}

function readRaw() {
  return JSON.parse(localStorage.getItem(VALKEY_INSTANCES_KEY) as string) as {
    version: number;
    instances: ValkeyInstance[];
    auditLogs: AuditLogEntry[];
  };
}

async function expectApiError(operation: Promise<unknown>, code: string) {
  await expect(operation).rejects.toMatchObject({ code });
  await expect(operation.catch((error: unknown) => error)).resolves.toBeInstanceOf(ApiError);
}

beforeEach(() => {
  localStorage.clear();
});

describe('хранение', () => {
  it('пустое хранилище даёт пустой список и не создаёт документ', async () => {
    await expect(listInstances(OWNER)).resolves.toEqual([]);
    expect(localStorage.getItem(VALKEY_INSTANCES_KEY)).toBeNull();
  });

  it('созданная база читается снова и хранится под текущей версией', async () => {
    const created = await createInstance(ACTOR, {
      name: 'valkey-1474',
      prefix: 'valkey',
      mode: 'single',
      vcpu: 1,
      ramGb: 2,
    });

    expect(readRaw().version).toBe(5);
    await expect(listInstances(OWNER)).resolves.toEqual([created]);
    await expect(getInstance(OWNER, created.id)).resolves.toEqual(created);
  });

  it('другой пользователь не видит чужие базы', async () => {
    const created = await createInstance(ACTOR, {
      name: 'valkey-1474',
      prefix: 'valkey',
      mode: 'single',
      vcpu: 1,
      ramGb: 2,
    });

    await expect(listInstances(OTHER_OWNER)).resolves.toEqual([]);
    await expectApiError(getInstance(OTHER_OWNER, created.id), 'NOT_FOUND');
  });

  it('повреждённая запись даёт ошибку загрузки, а не пустой список', async () => {
    writeRaw({ version: 1, instances: [{ id: 'a', ownerId: OWNER, name: 'valkey-1474' }] });

    await expectApiError(listInstances(OWNER), 'STORAGE_CORRUPTED');
  });

  it('размер вне сетки считается повреждённой записью', async () => {
    writeRaw({
      version: 1,
      instances: [
        {
          id: 'a',
          ownerId: OWNER,
          name: 'valkey-1474',
          prefix: 'valkey',
          mode: 'single',
          vcpu: 3,
          ramGb: 5,
          status: 'running',
          createdAt: '2026-09-08T00:00:00.000Z',
          updatedAt: '2026-09-08T00:00:00.000Z',
        },
      ],
    });

    await expectApiError(listInstances(OWNER), 'STORAGE_CORRUPTED');
  });

  it('неизвестная версия даёт ошибку загрузки', async () => {
    writeRaw({ version: 6, instances: [] });

    await expectApiError(listInstances(OWNER), 'STORAGE_CORRUPTED');
  });

  it('записи без префикса получают его из имени и своё DNS-имя', async () => {
    writeRaw({
      version: 1,
      instances: [
        {
          id: 'instance-1',
          ownerId: OWNER,
          name: 'valkey-1474',
          mode: 'single',
          vcpu: 1,
          ramGb: 2,
          status: 'running',
          createdAt: '2026-09-08T00:00:00.000Z',
          updatedAt: '2026-09-08T00:00:00.000Z',
        },
      ],
    });

    const [migrated] = await listInstances(OWNER);

    expect(migrated.prefix).toBe('valkey-1474');
    expect(migrated.slug).toMatch(/^valkey-1474-[a-z0-9]{6}$/);
    expect(migrated).toMatchObject({ isWhitelistEnabled: false, whitelistCidrs: [] });
  });

  it.each([2, 3, 4])('читает версию %s и добавляет метаданные пароля', async (version) => {
    writeRaw({
      version,
      auditLogs: version === 4 ? [] : undefined,
      instances: [
        {
          id: 'instance-1',
          ownerId: OWNER,
          name: 'valkey-1474',
          prefix: 'valkey',
          slug: 'valkey-abc123',
          mode: 'single',
          vcpu: 1,
          ramGb: 2,
          isWhitelistEnabled: false,
          whitelistCidrs: [],
          status: 'running',
          createdAt: '2026-09-08T00:00:00.000Z',
          updatedAt: '2026-09-08T00:00:00.000Z',
        },
      ],
    });

    await expect(listInstances(OWNER)).resolves.toHaveLength(1);
    expect(readAuditSnapshot(OWNER, 'instance-1')).toEqual([]);
    await expect(listInstances(OWNER)).resolves.toEqual([
      expect.objectContaining({
        passwordHint: 'demo*****',
        passwordVersion: 1,
        appliedPasswordVersion: 1,
      }),
    ]);
  });
});

describe('операции', () => {
  it('не сохраняет полный пароль созданной базы', async () => {
    const created = await createInstance(ACTOR, {
      name: 'valkey-1474',
      prefix: 'valkey',
      mode: 'single',
      vcpu: 1,
      ramGb: 1,
    });

    expect(created).toMatchObject({
      passwordHint: 'abcd*****',
      passwordVersion: 1,
      appliedPasswordVersion: 1,
    });
    expect(JSON.stringify(readRaw())).not.toContain(PASSWORD);
  });

  it('отклоняет пароль неверного формата', async () => {
    await expectApiError(
      createInstance(ACTOR, {
        name: 'valkey-1474',
        prefix: 'valkey',
        mode: 'single',
        vcpu: 1,
        ramGb: 1,
        password: 'short',
      }),
      'VALIDATION_FAILED'
    );

    expect(localStorage.getItem(VALKEY_INSTANCES_KEY)).toBeNull();
  });

  it('отклоняет второе имя, занятое у того же пользователя', async () => {
    await createInstance(ACTOR, {
      name: 'valkey-1474',
      prefix: 'valkey',
      mode: 'single',
      vcpu: 1,
      ramGb: 1,
    });

    await expectApiError(
      createInstance(ACTOR, {
        name: 'valkey-1474',
        prefix: 'valkey',
        mode: 'single',
        vcpu: 1,
        ramGb: 1,
      }),
      'CONFLICT'
    );
    expect(readRaw().instances).toHaveLength(1);
  });

  it('отклоняет имя вне формата', async () => {
    await expectApiError(
      createInstance(ACTOR, {
        name: 'Valkey_1474',
        prefix: 'valkey',
        mode: 'single',
        vcpu: 1,
        ramGb: 1,
      }),
      'VALIDATION_FAILED'
    );
  });

  it('отклоняет префикс вне формата', async () => {
    await expectApiError(
      createInstance(ACTOR, {
        name: 'valkey-1474',
        prefix: 'ab',
        mode: 'single',
        vcpu: 1,
        ramGb: 1,
      }),
      'VALIDATION_FAILED'
    );
    await expectApiError(
      createInstance(ACTOR, {
        name: 'valkey-1474',
        prefix: '-cache',
        mode: 'single',
        vcpu: 1,
        ramGb: 1,
      }),
      'VALIDATION_FAILED'
    );
  });

  it('собирает DNS-имя из префикса и не повторяет его между базами', async () => {
    const first = await createInstance(ACTOR, {
      name: 'valkey-1474',
      prefix: 'shop',
      mode: 'single',
      vcpu: 1,
      ramGb: 1,
    });
    const second = await createInstance(OTHER_ACTOR, {
      name: 'valkey-1474',
      prefix: 'shop',
      mode: 'single',
      vcpu: 1,
      ramGb: 1,
    });

    expect(first.slug).toMatch(/^shop-[a-z0-9]{6}$/);
    expect(second.slug).toMatch(/^shop-[a-z0-9]{6}$/);
    expect(first.slug).not.toBe(second.slug);
  });

  it('переименование не трогает DNS-имя', async () => {
    const created = await createInstance(ACTOR, {
      name: 'valkey-1474',
      prefix: 'shop',
      mode: 'single',
      vcpu: 1,
      ramGb: 1,
    });

    const renamed = await renameInstance(ACTOR, created.id, { name: 'valkey-3333' });

    expect(renamed.slug).toBe(created.slug);
    expect(renamed.prefix).toBe('shop');
  });

  it('отклоняет размер сверх квоты', async () => {
    await expectApiError(
      createInstance(ACTOR, {
        name: 'valkey-1474',
        prefix: 'valkey',
        mode: 'ha',
        vcpu: 2,
        ramGb: 4,
      }),
      'QUOTA_EXCEEDED'
    );
    expect(localStorage.getItem(VALKEY_INSTANCES_KEY)).toBeNull();
  });

  it('переименование сохраняет новое имя и отвергает занятое', async () => {
    const first = await createInstance(ACTOR, {
      name: 'valkey-1474',
      prefix: 'valkey',
      mode: 'single',
      vcpu: 1,
      ramGb: 1,
    });
    await createInstance(ACTOR, {
      name: 'valkey-2222',
      prefix: 'valkey',
      mode: 'single',
      vcpu: 1,
      ramGb: 1,
    });

    const renamed = await renameInstance(ACTOR, first.id, { name: 'valkey-3333' });
    expect(renamed.name).toBe('valkey-3333');
    expect(readRaw().instances[0].name).toBe('valkey-3333');

    await expectApiError(renameInstance(ACTOR, first.id, { name: 'valkey-2222' }), 'CONFLICT');
  });

  it('resize сохраняет новый размер и не считает текущий размер занятым', async () => {
    const created = await createInstance(ACTOR, {
      name: 'valkey-1474',
      prefix: 'valkey',
      mode: 'single',
      vcpu: 4,
      ramGb: 16,
    });

    const resized = await resizeInstance(ACTOR, created.id, { vcpu: 2, ramGb: 8 });
    expect(resized).toMatchObject({ vcpu: 2, ramGb: 8 });
    expect(readRaw().instances[0]).toMatchObject({ vcpu: 2, ramGb: 8 });

    await expectApiError(
      resizeInstance(ACTOR, created.id, { vcpu: 8, ramGb: 32 }),
      'QUOTA_EXCEEDED'
    );
  });

  it('удаление убирает базу и освобождает квоту', async () => {
    const created = await createInstance(ACTOR, {
      name: 'valkey-1474',
      prefix: 'valkey',
      mode: 'single',
      vcpu: 4,
      ramGb: 16,
    });

    await Promise.all([deleteInstance(ACTOR, created.id), deleteInstance(ACTOR, created.id)]);

    await expect(listInstances(OWNER)).resolves.toEqual([]);
    await expect(
      createInstance(ACTOR, {
        name: 'valkey-2222',
        prefix: 'valkey',
        mode: 'single',
        vcpu: 4,
        ramGb: 16,
      })
    ).resolves.toMatchObject({ name: 'valkey-2222' });
  });

  it('неизвестный идентификатор даёт NOT_FOUND при чтении и изменении', async () => {
    await expectApiError(getInstance(OWNER, 'missing'), 'NOT_FOUND');
    await expectApiError(renameInstance(ACTOR, 'missing', { name: 'valkey-1474' }), 'NOT_FOUND');
    await expectApiError(resizeInstance(ACTOR, 'missing', { vcpu: 1, ramGb: 1 }), 'NOT_FOUND');
    await expect(deleteInstance(ACTOR, 'missing')).resolves.toBeUndefined();
  });
});

describe('аудит операций', () => {
  it('сохраняет почту автора на момент успешной операции', async () => {
    const author = { userId: OWNER, email: 'before@example.com' };
    const created = await createInstance(author, {
      name: 'valkey-1474',
      prefix: 'valkey',
      mode: 'single',
      vcpu: 1,
      ramGb: 1,
    });

    author.email = 'after@example.com';

    expect(readAuditSnapshot(OWNER, created.id)[0]).toMatchObject({
      action: 'instance.create',
      userEmail: 'before@example.com',
    });
  });

  it('пишет по одному событию создания, переименования, тарифа и удаления', async () => {
    const created = await createInstance(ACTOR, {
      name: 'valkey-1474',
      prefix: 'valkey',
      mode: 'single',
      vcpu: 1,
      ramGb: 1,
    });

    await renameInstance(ACTOR, created.id, { name: 'valkey-2222' });
    await resizeInstance(ACTOR, created.id, { vcpu: 1, ramGb: 2 });
    await deleteInstance(ACTOR, created.id);

    expect(readRaw().auditLogs.map((item) => item.action)).toEqual([
      'instance.create',
      'instance.update',
      'instance.resize',
      'instance.delete',
    ]);
  });

  it('не пишет событие отклонённой операции', async () => {
    const created = await createInstance(ACTOR, {
      name: 'valkey-1474',
      prefix: 'valkey',
      mode: 'single',
      vcpu: 1,
      ramGb: 1,
    });
    const before = readRaw().auditLogs;

    await expectApiError(renameInstance(ACTOR, created.id, { name: '' }), 'VALIDATION_FAILED');

    expect(readRaw().auditLogs).toEqual(before);
  });
});
