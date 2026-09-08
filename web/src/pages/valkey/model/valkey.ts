/** Сетка размеров, ставки и форматы взяты из SYSTEM.md, раздел 8. */

export type ValkeyMode = 'single' | 'ha';
export type ValkeyStatus = 'running';
export type PricePeriod = 'month' | 'day' | 'hour';

export const VCPU_OPTIONS = [1, 2, 4, 8, 16] as const;
export const RAM_OPTIONS = [1, 2, 4, 8, 16, 32, 64, 128] as const;

export type ValkeyVcpu = (typeof VCPU_OPTIONS)[number];
export type ValkeyRamGb = (typeof RAM_OPTIONS)[number];

export interface ValkeySize {
  vcpu: ValkeyVcpu;
  ramGb: ValkeyRamGb;
}

export const VALKEY_PLANS: readonly ValkeySize[] = [
  { vcpu: 1, ramGb: 1 },
  { vcpu: 1, ramGb: 2 },
  { vcpu: 1, ramGb: 4 },
  { vcpu: 2, ramGb: 4 },
  { vcpu: 2, ramGb: 8 },
  { vcpu: 4, ramGb: 16 },
];

export interface ValkeyInstance extends ValkeySize {
  id: string;
  ownerId: string;
  name: string;
  prefix: string;
  slug: string;
  mode: ValkeyMode;
  isWhitelistEnabled: boolean;
  whitelistCidrs: string[];
  status: ValkeyStatus;
  createdAt: string;
  updatedAt: string;
}

/** Ставки из SYSTEM.md: 1,25 ₽ за vCPU в час и 0,50 ₽ за 1 ГБ RAM в час. */
const PRICE_VCPU_KOPECKS_PER_HOUR = 125;
const PRICE_RAM_GB_KOPECKS_PER_HOUR = 50;

const HOURS_IN_MONTH = 720;
const HOURS_IN_DAY = 24;

export const PRICE_PERIOD_HOURS: Record<PricePeriod, number> = {
  month: HOURS_IN_MONTH,
  day: HOURS_IN_DAY,
  hour: 1,
};

export const PRICE_PERIOD_LABELS: Record<PricePeriod, string> = {
  month: 'Месяц',
  day: 'День',
  hour: 'Час',
};

export const PRICE_PERIOD_SUFFIXES: Record<PricePeriod, string> = {
  month: 'в месяц',
  day: 'в день',
  hour: 'в час',
};

export const MODE_LABELS: Record<ValkeyMode, string> = {
  single: 'Одна нода',
  ha: 'Отказоустойчивый',
};

export const STATUS_LABELS: Record<ValkeyStatus, string> = {
  running: 'Работает',
};

/** `ha` это primary и две реплики. */
export function getNodeCount(mode: ValkeyMode) {
  return mode === 'ha' ? 3 : 1;
}

export function isValidSize(vcpu: number, ramGb: number) {
  return (
    (VCPU_OPTIONS as readonly number[]).includes(vcpu) &&
    (RAM_OPTIONS as readonly number[]).includes(ramGb) &&
    ramGb >= vcpu &&
    ramGb <= 16 * vcpu
  );
}

export function getRamOptions(vcpu: ValkeyVcpu): ValkeyRamGb[] {
  return RAM_OPTIONS.filter((ramGb) => isValidSize(vcpu, ramGb));
}

export function clampRamGb(vcpu: ValkeyVcpu, ramGb: number): ValkeyRamGb {
  const options = getRamOptions(vcpu);
  return options.reduce((closest, option) =>
    Math.abs(option - ramGb) < Math.abs(closest - ramGb) ? option : closest
  );
}

export function getNodeHourlyKopecks(size: ValkeySize) {
  return PRICE_VCPU_KOPECKS_PER_HOUR * size.vcpu + PRICE_RAM_GB_KOPECKS_PER_HOUR * size.ramGb;
}

export function getHourlyKopecks(size: ValkeySize, mode: ValkeyMode) {
  return getNodeHourlyKopecks(size) * getNodeCount(mode);
}

export function getPeriodKopecks(size: ValkeySize, mode: ValkeyMode, period: PricePeriod) {
  return getHourlyKopecks(size, mode) * PRICE_PERIOD_HOURS[period];
}

export interface PriceBreakdown {
  nodes: number;
  vcpuKopecks: number;
  ramKopecks: number;
  totalKopecks: number;
}

export function getPriceBreakdown(
  size: ValkeySize,
  mode: ValkeyMode,
  period: PricePeriod
): PriceBreakdown {
  const multiplier = PRICE_PERIOD_HOURS[period] * getNodeCount(mode);
  const vcpuKopecks = PRICE_VCPU_KOPECKS_PER_HOUR * size.vcpu * multiplier;
  const ramKopecks = PRICE_RAM_GB_KOPECKS_PER_HOUR * size.ramGb * multiplier;

  return {
    nodes: getNodeCount(mode),
    vcpuKopecks,
    ramKopecks,
    totalKopecks: vcpuKopecks + ramKopecks,
  };
}

export function getTotalResources(size: ValkeySize, mode: ValkeyMode) {
  const nodes = getNodeCount(mode);
  return { vcpu: size.vcpu * nodes, ramGb: size.ramGb * nodes };
}

const NBSP = '\u00A0';

function groupDigits(value: string) {
  return value.replace(/\B(?=(\d{3})+(?!\d))/g, NBSP);
}

/** Форматы чисел и денег заданы в web/DESIGN.md, раздел «Текст». */
export function formatPrice(kopecks: number) {
  const rubles = Math.trunc(kopecks / 100);
  const remainder = Math.abs(kopecks % 100);
  return `${groupDigits(String(rubles))},${String(remainder).padStart(2, '0')}${NBSP}₽`;
}

export function formatPriceWithPeriod(kopecks: number, period: PricePeriod) {
  return `${formatPrice(kopecks)} ${PRICE_PERIOD_SUFFIXES[period]}`;
}

export function formatRam(ramGb: number) {
  return `${groupDigits(String(ramGb))}${NBSP}ГБ`;
}

export function formatVcpu(vcpu: number) {
  return `${groupDigits(String(vcpu))}${NBSP}vCPU`;
}

const DATE_TIME_FORMAT = new Intl.DateTimeFormat('ru-RU', {
  dateStyle: 'medium',
  timeStyle: 'short',
});

export function formatDateTime(isoDate: string) {
  const date = new Date(isoDate);
  return Number.isNaN(date.getTime()) ? isoDate : DATE_TIME_FORMAT.format(date);
}

export function formatSize(size: ValkeySize) {
  return `${formatVcpu(size.vcpu)} / ${formatRam(size.ramGb)}`;
}

export const INSTANCE_NAME_PATTERN = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/;

const INSTANCE_NAME_MAX_LENGTH = 40;

export function validateInstanceName(name: string) {
  if (!name) {
    return 'Введите имя базы';
  }

  if (name.length > INSTANCE_NAME_MAX_LENGTH) {
    return `Имя не длиннее ${INSTANCE_NAME_MAX_LENGTH} символов`;
  }

  if (!INSTANCE_NAME_PATTERN.test(name)) {
    return 'Строчные латинские буквы, цифры и дефис; дефис не по краям';
  }

  return null;
}

/** Префикс уходит в DNS-имя, поэтому правила из SYSTEM.md, раздел 5. */
export const INSTANCE_PREFIX_PATTERN = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/;

export const INSTANCE_PREFIX_MIN_LENGTH = 3;
export const INSTANCE_PREFIX_MAX_LENGTH = 20;
export const DEFAULT_INSTANCE_PREFIX = 'valkey';

export function validateInstancePrefix(prefix: string) {
  if (!prefix) {
    return 'Введите префикс';
  }

  if (prefix.length < INSTANCE_PREFIX_MIN_LENGTH) {
    return `Префикс не короче ${INSTANCE_PREFIX_MIN_LENGTH} символов`;
  }

  if (prefix.length > INSTANCE_PREFIX_MAX_LENGTH) {
    return `Префикс не длиннее ${INSTANCE_PREFIX_MAX_LENGTH} символов`;
  }

  if (!INSTANCE_PREFIX_PATTERN.test(prefix)) {
    return 'Строчные латинские буквы, цифры и дефис; дефис не по краям';
  }

  return null;
}

function isValidIpv4(value: string) {
  const octets = value.split('.');
  return (
    octets.length === 4 && octets.every((octet) => /^\d{1,3}$/.test(octet) && Number(octet) <= 255)
  );
}

export function parseWhitelistCidrs(value: string) {
  return [
    ...new Set(
      value
        .split(/[\n,]+/)
        .map((item) => item.trim())
        .filter(Boolean)
        .map((item) => (item.includes('/') ? item : `${item}/32`))
    ),
  ];
}

export function validateWhitelistCidrs(value: string) {
  const cidrs = parseWhitelistCidrs(value);

  if (cidrs.length === 0) {
    return 'Добавьте хотя бы один IPv4-адрес или диапазон CIDR';
  }

  for (const cidr of cidrs) {
    const [address, prefix, extra] = cidr.split('/');
    if (extra !== undefined || !isValidIpv4(address) || !/^\d{1,2}$/.test(prefix)) {
      return `Проверьте адрес ${cidr}`;
    }

    if (Number(prefix) > 32) {
      return `Проверьте адрес ${cidr}`;
    }
  }

  return null;
}

export const SLUG_SUFFIX_LENGTH = 6;

const SLUG_ALPHABET = 'abcdefghijklmnopqrstuvwxyz0123456789';

/** Отбрасываем хвост диапазона: остаток от 256 сместил бы часть символов. */
const SLUG_RANDOM_LIMIT = 256 - (256 % SLUG_ALPHABET.length);

function randomSlugSuffix() {
  let suffix = '';

  while (suffix.length < SLUG_SUFFIX_LENGTH) {
    for (const byte of crypto.getRandomValues(new Uint8Array(SLUG_SUFFIX_LENGTH))) {
      if (byte < SLUG_RANDOM_LIMIT && suffix.length < SLUG_SUFFIX_LENGTH) {
        suffix += SLUG_ALPHABET[byte % SLUG_ALPHABET.length];
      }
    }
  }

  return suffix;
}

export function getSlugPreview(prefix: string) {
  return `${prefix}-${'x'.repeat(SLUG_SUFFIX_LENGTH)}`;
}

export function formatInstanceHost(slug: string, domain: string) {
  return `${slug}.${domain}`;
}

export function formatInstanceAddress(slug: string, domain: string, port: number) {
  return `${formatInstanceHost(slug, domain)}:${port}`;
}

/** Slug уникален по всей установке, а не по одному владельцу: он адрес в DNS. */
export function generateSlug(prefix: string, takenSlugs: readonly string[] = []) {
  const taken = new Set(takenSlugs);

  for (let attempt = 0; attempt < 100; attempt += 1) {
    const slug = `${prefix}-${randomSlugSuffix()}`;

    if (!taken.has(slug)) {
      return slug;
    }
  }

  throw new Error('Не удалось подобрать свободный slug');
}

export function generateInstanceName(takenNames: readonly string[] = []) {
  const taken = new Set(takenNames);

  for (let attempt = 0; attempt < 100; attempt += 1) {
    const digits = crypto.getRandomValues(new Uint32Array(1))[0] % 10_000;
    const name = `valkey-${String(digits).padStart(4, '0')}`;

    if (!taken.has(name)) {
      return name;
    }
  }

  return `valkey-${Date.now().toString().slice(-4)}`;
}
