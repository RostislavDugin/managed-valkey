import { describe, expect, it } from 'vitest';
import { checkQuota, getInstanceReserve, type ValkeyQuota } from './quota';
import { getPeriodCoins, type ValkeyInstance, type ValkeyPricing } from './valkey';

const quota: ValkeyQuota = {
  limit: { vcpu: 4, ramGb: 16 },
  usage: { vcpu: 4, ramGb: 16 },
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
    ).toMatchObject({ fits: false, missing: { vcpu: 4, ramGb: 8 } });
  });

  it('для базы в режиме HA рассчитывает резерв процессора и памяти на три ноды', () => {
    expect(
      checkQuota(
        { limit: { vcpu: 3, ramGb: 12 }, usage: { vcpu: 0, ramGb: 0 } },
        { size: { vcpu: 1, ramGb: 4 }, mode: 'ha' }
      )
    ).toMatchObject({ fits: true, required: { vcpu: 3, ramGb: 12 } });
  });
});
