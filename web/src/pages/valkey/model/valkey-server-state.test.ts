import { describe, expect, it } from 'vitest';
import {
  checkCapacity,
  checkQuota,
  getInstanceReserve,
  type ValkeyCapacity,
  type ValkeyQuota,
} from './quota';
import { getPeriodCoins, type ValkeyInstance, type ValkeyPricing } from './valkey';

const quota: ValkeyQuota = {
  limit: { vcpu: 4, ramGb: 12 },
  usage: { vcpu: 4, ramGb: 16 },
};

const capacity: ValkeyCapacity = {
  user: { limit: { vcpu: 4, ramGb: 12 }, usage: { vcpu: 1, ramGb: 2 } },
  cluster: { limit: { vcpu: 12, ramGb: 48 }, usage: { vcpu: 8, ramGb: 32 } },
  instances: { limit: 32, usage: 31 },
};

const instance = {
  mode: 'single',
  vcpu: 2,
  ramGb: 4,
  appliedVcpu: 4,
  appliedRamGb: 8,
} as ValkeyInstance;

describe('серверные цены и квота', () => {
  it('при расчёте месячной цены использует ставки и число часов из серверного каталога', () => {
    const pricing: ValkeyPricing = {
      vcpuCoinsPerHour: 200,
      ramGbCoinsPerHour: 80,
      hoursPerMonth: 744,
    };
    expect(getPeriodCoins({ vcpu: 2, ramGb: 4 }, 'single', 'month', pricing)).toBe(
      (2 * 200 + 4 * 80) * 744
    );
    expect(getPeriodCoins({ vcpu: 1, ramGb: 4 }, 'ha', 'hour', pricing)).toBe(1560);
  });

  it('после снижения квоты учитывает применённый резерв и разрешает уменьшить конфигурацию базы', () => {
    expect(getInstanceReserve(instance)).toEqual({ vcpu: 4, ramGb: 8 });
    expect(
      checkQuota(quota, { size: { vcpu: 1, ramGb: 4 }, mode: 'single' }, instance)
    ).toMatchObject({ fits: true, requested: { vcpu: 4, ramGb: 16 } });
    expect(
      checkQuota(quota, { size: { vcpu: 8, ramGb: 16 }, mode: 'single' }, instance)
    ).toMatchObject({ fits: false, missing: { vcpu: 4, ramGb: 12 } });
  });

  it('для базы в режиме HA рассчитывает резерв процессора и памяти на три ноды', () => {
    expect(
      checkQuota(
        { limit: { vcpu: 3, ramGb: 12 }, usage: { vcpu: 0, ramGb: 0 } },
        { size: { vcpu: 1, ramGb: 4 }, mode: 'ha' }
      )
    ).toMatchObject({ fits: true, required: { vcpu: 3, ramGb: 12 } });
  });

  it('для кандидата сообщает первое нарушенное ограничение и не применяет предел баз к изменению тарифа', () => {
    expect(checkCapacity(capacity, { size: { vcpu: 1, ramGb: 2 }, mode: 'single' })).toMatchObject({
      fits: true,
      reason: 'available',
    });
    expect(checkCapacity(capacity, { size: { vcpu: 2, ramGb: 8 }, mode: 'ha' })).toMatchObject({
      fits: false,
      reason: 'user_quota',
    });
    expect(
      checkCapacity(
        {
          ...capacity,
          user: { limit: { vcpu: 20, ramGb: 80 }, usage: { vcpu: 1, ramGb: 2 } },
        },
        { size: { vcpu: 2, ramGb: 8 }, mode: 'ha' }
      )
    ).toMatchObject({ fits: false, reason: 'cluster_resources' });
    expect(
      checkCapacity(
        { ...capacity, instances: { limit: 32, usage: 32 } },
        { size: { vcpu: 1, ramGb: 2 }, mode: 'single' }
      )
    ).toMatchObject({ fits: false, reason: 'instance_limit' });
    expect(
      checkCapacity(
        { ...capacity, instances: { limit: 32, usage: 32 } },
        { size: { vcpu: 1, ramGb: 2 }, mode: 'single' },
        { ...instance, vcpu: 1, ramGb: 2, appliedVcpu: 1, appliedRamGb: 2 }
      )
    ).toMatchObject({ fits: true, reason: 'available' });
  });
});
