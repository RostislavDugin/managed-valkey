import { describe, expect, it } from 'vitest';
import {
  checkQuota,
  findAvailableSize,
  findLargestAvailableSize,
  getQuotaUsage,
  USER_QUOTA,
} from './quota';
import type { ValkeyInstance, ValkeyMode, ValkeyRamGb, ValkeyVcpu } from './valkey';

function instance(
  id: string,
  mode: ValkeyMode,
  vcpu: ValkeyVcpu,
  ramGb: ValkeyRamGb
): ValkeyInstance {
  return {
    id,
    ownerId: 'owner',
    name: id,
    prefix: 'valkey',
    slug: `valkey-${id}`,
    mode,
    vcpu,
    ramGb,
    isWhitelistEnabled: false,
    whitelistCidrs: [],
    passwordHint: 'demo*****',
    passwordVersion: 1,
    appliedPasswordVersion: 1,
    status: 'running',
    createdAt: '2026-09-08T00:00:00.000Z',
    updatedAt: '2026-09-08T00:00:00.000Z',
  };
}

describe('занятая квота', () => {
  it('пустой список ничего не занимает', () => {
    expect(getQuotaUsage([])).toEqual({ vcpu: 0, ramGb: 0 });
  });

  it('суммирует ресурсы по числу нод', () => {
    const instances = [instance('a', 'single', 2, 8), instance('b', 'ha', 1, 2)];

    expect(getQuotaUsage(instances)).toEqual({ vcpu: 5, ramGb: 14 });
  });

  it('исключает изменяемую базу из суммы', () => {
    const instances = [instance('a', 'single', 2, 8), instance('b', 'ha', 1, 2)];

    expect(getQuotaUsage(instances, 'a')).toEqual({ vcpu: 3, ramGb: 6 });
  });
});

describe('доступность кандидата', () => {
  it('пропускает размер ровно по границе квоты', () => {
    const check = checkQuota([], { size: { vcpu: 4, ramGb: 16 }, mode: 'single' });

    expect(check.fits).toBe(true);
    expect(check.required).toEqual({ vcpu: 4, ramGb: 16 });
    expect(check.available).toEqual({ vcpu: USER_QUOTA.vcpu, ramGb: USER_QUOTA.ramGb });
    expect(check.missing).toEqual({ vcpu: 0, ramGb: 0 });
  });

  it('называет нехватку по каждому ресурсу', () => {
    const check = checkQuota([instance('a', 'single', 2, 8)], {
      size: { vcpu: 4, ramGb: 16 },
      mode: 'single',
    });

    expect(check.fits).toBe(false);
    expect(check.used).toEqual({ vcpu: 2, ramGb: 8 });
    expect(check.missing).toEqual({ vcpu: 2, ramGb: 8 });
  });

  it('учитывает три ноды режима ha', () => {
    expect(checkQuota([], { size: { vcpu: 1, ramGb: 4 }, mode: 'ha' }).fits).toBe(true);
    expect(checkQuota([], { size: { vcpu: 1, ramGb: 8 }, mode: 'ha' }).fits).toBe(false);
    expect(checkQuota([], { size: { vcpu: 2, ramGb: 4 }, mode: 'ha' }).fits).toBe(false);
  });

  it('при resize не считает текущий размер базы занятым', () => {
    const instances = [instance('a', 'single', 4, 16)];

    expect(checkQuota(instances, { size: { vcpu: 2, ramGb: 8 }, mode: 'single' }, 'a').fits).toBe(
      true
    );
    expect(checkQuota(instances, { size: { vcpu: 2, ramGb: 8 }, mode: 'single' }).fits).toBe(false);
  });
});

describe('поиск доступного размера', () => {
  it('на пустой квоте предлагает минимальный размер', () => {
    expect(findAvailableSize([], 'single')).toEqual({ vcpu: 1, ramGb: 1 });
    expect(findAvailableSize([], 'ha')).toEqual({ vcpu: 1, ramGb: 1 });
  });

  it('возвращает null, когда квота исчерпана', () => {
    const instances = [instance('a', 'single', 4, 16)];

    expect(findAvailableSize(instances, 'single')).toBeNull();
    expect(findAvailableSize(instances, 'single', 'a')).toEqual({ vcpu: 1, ramGb: 1 });
  });

  it('находит максимальную конфигурацию для каждого режима', () => {
    expect(findLargestAvailableSize([], 'single')).toEqual({ vcpu: 4, ramGb: 16 });
    expect(findLargestAvailableSize([], 'ha')).toEqual({ vcpu: 1, ramGb: 4 });
  });

  it('считает максимальную конфигурацию по свободному остатку', () => {
    const instances = [instance('a', 'single', 2, 8)];

    expect(findLargestAvailableSize(instances, 'single')).toEqual({ vcpu: 2, ramGb: 8 });
    expect(findLargestAvailableSize(instances, 'ha')).toBeNull();
  });
});
