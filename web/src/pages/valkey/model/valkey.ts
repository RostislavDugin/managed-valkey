export type ValkeyMode = 'single' | 'ha';
export type ValkeyStatus =
  | 'provisioning'
  | 'running'
  | 'updating'
  | 'degraded'
  | 'unavailable'
  | 'error'
  | 'deleting'
  | 'deleted';
export type ValkeyNetworkVerificationStatus = 'pending' | 'verified' | 'unknown';
export type PricePeriod = 'month' | 'day' | 'hour';
export type ValkeyVcpu = number;
export type ValkeyRamGb = number;

export interface ValkeySize {
  vcpu: ValkeyVcpu;
  ramGb: ValkeyRamGb;
}

export interface ValkeyPricing {
  vcpuCoinsPerHour: number;
  ramGbCoinsPerHour: number;
  hoursPerMonth: number;
}

export interface ValkeyConnection {
  domain: string;
  port: number;
}

export interface ValkeyCatalog {
  items: ValkeySize[];
  pricing: ValkeyPricing;
  connection: ValkeyConnection;
}

export interface ValkeyMaintenance {
  dow: number;
  hourUtc: number;
  durationMin: number;
}

export interface ValkeyInstance extends ValkeySize {
  id: string;
  name: string;
  slug: string;
  mode: ValkeyMode;
  appliedVcpu: number;
  appliedRamGb: number;
  host: string;
  hostRo: string;
  port: number;
  isWhitelistEnabled: boolean;
  whitelistCidrs: string[];
  maintenance: ValkeyMaintenance | null;
  passwordHint: string;
  passwordVersion: number;
  appliedPasswordVersion: number;
  status: ValkeyStatus;
  phaseReason: string | null;
  desiredGeneration: number;
  observedGeneration: number;
  observedAt: string | null;
  isStale: boolean;
  isUpdating: boolean;
  isRecoveryRequired: boolean;
  networkVerificationStatus: ValkeyNetworkVerificationStatus;
  networkVerifiedAt: string | null;
  createdAt: string;
  updatedAt: string;
  configurationRequestedAt: string;
  deletionRequestedAt: string | null;
}

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
  provisioning: 'Создаётся',
  running: 'Работает',
  updating: 'Обновляется',
  degraded: 'Работает с ограничениями',
  unavailable: 'Недоступна',
  error: 'Ошибка',
  deleting: 'Удаляется',
  deleted: 'Удалена',
};

export function getProcessCount(mode: ValkeyMode) {
  return mode === 'ha' ? 3 : 1;
}

export function isCatalogSize(catalog: ValkeyCatalog, size: ValkeySize) {
  return catalog.items.some((item) => item.vcpu === size.vcpu && item.ramGb === size.ramGb);
}

function periodHours(period: PricePeriod, pricing: ValkeyPricing) {
  if (period === 'month') {
    return pricing.hoursPerMonth;
  }
  return period === 'day' ? 24 : 1;
}

export function getPeriodCoins(
  size: ValkeySize,
  mode: ValkeyMode,
  period: PricePeriod,
  pricing: ValkeyPricing
) {
  const perProcess = pricing.vcpuCoinsPerHour * size.vcpu + pricing.ramGbCoinsPerHour * size.ramGb;
  return perProcess * getProcessCount(mode) * periodHours(period, pricing);
}

export interface PriceBreakdown {
  processes: number;
  vcpuCoins: number;
  ramCoins: number;
  totalCoins: number;
}

export function getPriceBreakdown(
  size: ValkeySize,
  mode: ValkeyMode,
  period: PricePeriod,
  pricing: ValkeyPricing
): PriceBreakdown {
  const multiplier = periodHours(period, pricing) * getProcessCount(mode);
  const vcpuCoins = pricing.vcpuCoinsPerHour * size.vcpu * multiplier;
  const ramCoins = pricing.ramGbCoinsPerHour * size.ramGb * multiplier;

  return {
    processes: getProcessCount(mode),
    vcpuCoins,
    ramCoins,
    totalCoins: vcpuCoins + ramCoins,
  };
}

export function getTotalResources(size: ValkeySize, mode: ValkeyMode) {
  const processes = getProcessCount(mode);
  return { vcpu: size.vcpu * processes, ramGb: size.ramGb * processes };
}

const NBSP = '\u00A0';

function groupDigits(value: string) {
  return value.replace(/\B(?=(\d{3})+(?!\d))/g, NBSP);
}

export function formatPrice(coins: number) {
  const rubles = Math.trunc(coins / 100);
  const remainder = Math.abs(coins % 100);
  return `${groupDigits(String(rubles))},${String(remainder).padStart(2, '0')}${NBSP}₽`;
}

export function formatPriceWithPeriod(coins: number, period: PricePeriod) {
  return `${formatPrice(coins)} ${PRICE_PERIOD_SUFFIXES[period]}`;
}

export function formatRam(ramGb: number) {
  return `${groupDigits(String(ramGb))}${NBSP}ГБ`;
}

export function formatVcpu(vcpu: number) {
  return `${groupDigits(String(vcpu))}${NBSP}vCPU`;
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
    if (
      extra !== undefined ||
      !isValidIpv4(address) ||
      !/^\d{1,2}$/.test(prefix) ||
      Number(prefix) > 32
    ) {
      return `Проверьте адрес ${cidr}`;
    }
  }
  return null;
}

export const SLUG_SUFFIX_LENGTH = 6;

export function getSlugPreview(prefix: string) {
  return `${prefix}-${'x'.repeat(SLUG_SUFFIX_LENGTH)}`;
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
