import { describe, expect, it } from 'vitest';
import {
  clampRamGb,
  formatPrice,
  formatPriceWithPeriod,
  formatRam,
  formatSize,
  formatVcpu,
  getNodeCount,
  getPeriodKopecks,
  getRamOptions,
  getTotalResources,
  isValidSize,
  parseWhitelistCidrs,
  RAM_OPTIONS,
  VCPU_OPTIONS,
  generateSlug,
  getSlugPreview,
  validateInstancePrefix,
  validateWhitelistCidrs,
} from './valkey';

const NBSP = '\u00A0';

describe('сетка размеров', () => {
  it('оставляет для каждого vCPU ровно те ступени RAM, что заданы в SYSTEM.md', () => {
    expect(getRamOptions(1)).toEqual([1, 2, 4, 8, 16]);
    expect(getRamOptions(2)).toEqual([2, 4, 8, 16, 32]);
    expect(getRamOptions(4)).toEqual([4, 8, 16, 32, 64]);
    expect(getRamOptions(8)).toEqual([8, 16, 32, 64, 128]);
    expect(getRamOptions(16)).toEqual([16, 32, 64, 128]);
  });

  it('принимает сочетание, когда RAM не меньше vCPU и не больше 16 * vCPU', () => {
    for (const vcpu of VCPU_OPTIONS) {
      for (const ramGb of RAM_OPTIONS) {
        expect(isValidSize(vcpu, ramGb)).toBe(ramGb >= vcpu && ramGb <= 16 * vcpu);
      }
    }
  });

  it('отбрасывает значения вне списков сетки', () => {
    expect(isValidSize(3, 8)).toBe(false);
    expect(isValidSize(2, 3)).toBe(false);
    expect(isValidSize(0, 1)).toBe(false);
    expect(isValidSize(16, 256)).toBe(false);
  });

  it('приводит RAM к ближайшей допустимой ступени нового vCPU', () => {
    expect(clampRamGb(8, 1)).toBe(8);
    expect(clampRamGb(1, 64)).toBe(16);
    expect(clampRamGb(4, 8)).toBe(8);
  });
});

describe('белый список', () => {
  it('нормализует отдельные IPv4-адреса в CIDR и удаляет повторы', () => {
    expect(parseWhitelistCidrs('203.0.113.10\n198.51.100.0/24, 203.0.113.10')).toEqual([
      '203.0.113.10/32',
      '198.51.100.0/24',
    ]);
  });

  it('проверяет IPv4-адрес и длину префикса', () => {
    expect(validateWhitelistCidrs('203.0.113.10\n198.51.100.0/24')).toBeNull();
    expect(validateWhitelistCidrs('')).toBe('Добавьте хотя бы один IPv4-адрес или диапазон CIDR');
    expect(validateWhitelistCidrs('203.0.113.999')).toBe('Проверьте адрес 203.0.113.999/32');
    expect(validateWhitelistCidrs('203.0.113.0/33')).toBe('Проверьте адрес 203.0.113.0/33');
  });
});

describe('цена', () => {
  it('считает месяц для single по примерам SYSTEM.md', () => {
    expect(getPeriodKopecks({ vcpu: 1, ramGb: 1 }, 'single', 'month')).toBe(126_000);
    expect(getPeriodKopecks({ vcpu: 1, ramGb: 2 }, 'single', 'month')).toBe(162_000);
    expect(getPeriodKopecks({ vcpu: 1, ramGb: 4 }, 'single', 'month')).toBe(234_000);
    expect(getPeriodKopecks({ vcpu: 2, ramGb: 4 }, 'single', 'month')).toBe(324_000);
    expect(getPeriodKopecks({ vcpu: 2, ramGb: 8 }, 'single', 'month')).toBe(468_000);
    expect(getPeriodKopecks({ vcpu: 4, ramGb: 16 }, 'single', 'month')).toBe(936_000);
  });

  it('умножает цену ha на три ноды', () => {
    expect(getPeriodKopecks({ vcpu: 1, ramGb: 1 }, 'ha', 'month')).toBe(378_000);
    expect(getPeriodKopecks({ vcpu: 4, ramGb: 16 }, 'ha', 'month')).toBe(2_808_000);
    expect(getNodeCount('ha')).toBe(3);
    expect(getNodeCount('single')).toBe(1);
  });

  it('считает день и час от той же ставки', () => {
    expect(getPeriodKopecks({ vcpu: 1, ramGb: 2 }, 'single', 'hour')).toBe(225);
    expect(getPeriodKopecks({ vcpu: 1, ramGb: 2 }, 'single', 'day')).toBe(5_400);
    expect(getPeriodKopecks({ vcpu: 16, ramGb: 128 }, 'ha', 'hour')).toBe(25_200);
  });

  it('умножает ресурсы на число нод', () => {
    expect(getTotalResources({ vcpu: 2, ramGb: 8 }, 'single')).toEqual({ vcpu: 2, ramGb: 8 });
    expect(getTotalResources({ vcpu: 2, ramGb: 8 }, 'ha')).toEqual({ vcpu: 6, ramGb: 24 });
  });
});

describe('подписи', () => {
  it('пишет цену с разрядами и копейками', () => {
    expect(formatPrice(192_600)).toBe(`1${NBSP}926,00${NBSP}₽`);
    expect(formatPrice(22_500)).toBe(`225,00${NBSP}₽`);
    expect(formatPrice(225)).toBe(`2,25${NBSP}₽`);
    expect(formatPriceWithPeriod(936_000, 'month')).toBe(`9${NBSP}360,00${NBSP}₽ в месяц`);
  });

  it('пишет ресурсы с неразрывным пробелом', () => {
    expect(formatRam(2)).toBe(`2${NBSP}ГБ`);
    expect(formatVcpu(1)).toBe(`1${NBSP}vCPU`);
    expect(formatSize({ vcpu: 2, ramGb: 8 })).toBe(`2${NBSP}vCPU / 8${NBSP}ГБ`);
  });
});

describe('префикс и DNS-имя', () => {
  it('принимает префикс от 3 до 20 символов из букв, цифр и дефиса', () => {
    expect(validateInstancePrefix('shop')).toBeNull();
    expect(validateInstancePrefix('shop-cache-1')).toBeNull();
    expect(validateInstancePrefix('a1b')).toBeNull();
  });

  it('называет причину отказа', () => {
    expect(validateInstancePrefix('')).toBe('Введите префикс');
    expect(validateInstancePrefix('ab')).toBe('Префикс не короче 3 символов');
    expect(validateInstancePrefix('a'.repeat(21))).toBe('Префикс не длиннее 20 символов');
    expect(validateInstancePrefix('-shop')).not.toBeNull();
    expect(validateInstancePrefix('shop-')).not.toBeNull();
    expect(validateInstancePrefix('Shop')).not.toBeNull();
    expect(validateInstancePrefix('shop_1')).not.toBeNull();
  });

  it('добавляет к префиксу шесть случайных символов', () => {
    const slug = generateSlug('shop');

    expect(slug).toMatch(/^shop-[a-z0-9]{6}$/);
    expect(generateSlug('shop')).not.toBe(slug);
  });

  it('обходит занятое DNS-имя', () => {
    const taken = generateSlug('shop');

    expect(generateSlug('shop', [taken])).not.toBe(taken);
  });

  it('показывает будущее DNS-имя без случайной части', () => {
    expect(getSlugPreview('shop')).toBe('shop-xxxxxx');
  });
});
