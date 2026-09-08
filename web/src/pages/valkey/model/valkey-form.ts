import { ApiError } from '@/shared/api';
import { checkQuota, findAvailableSize, type QuotaCheck } from './quota';
import {
  clampRamGb,
  DEFAULT_INSTANCE_PREFIX,
  formatRam,
  formatVcpu,
  generateInstanceName,
  getRamOptions,
  type ValkeyInstance,
  type ValkeyMode,
  type ValkeyRamGb,
  type ValkeySize,
  type ValkeyVcpu,
} from './valkey';

/** Поддержка повышает квоту вручную: интерфейса для этого нет. */
export const SUPPORT_URL = 'https://t.me/rostislav_dugin';

export interface CreateFormValues extends ValkeySize {
  name: string;
  prefix: string;
  mode: ValkeyMode;
  isWhitelistEnabled: boolean;
  whitelist: string;
}

/**
 * Без свободного размера форма открывается на минимальном: цену посмотреть
 * можно, отправить нельзя.
 */
export function getCreateFormDefaults(instances: ValkeyInstance[]): CreateFormValues {
  const size = findAvailableSize(instances, 'single') ?? { vcpu: 1, ramGb: 1 };

  return {
    name: generateInstanceName(instances.map((instance) => instance.name)),
    prefix: DEFAULT_INSTANCE_PREFIX,
    mode: 'single',
    vcpu: size.vcpu,
    ramGb: size.ramGb,
    isWhitelistEnabled: false,
    whitelist: '',
  };
}

export function alignRamGb(vcpu: ValkeyVcpu, ramGb: ValkeyRamGb): ValkeyRamGb {
  return getRamOptions(vcpu).includes(ramGb) ? ramGb : clampRamGb(vcpu, ramGb);
}

export function describeMissingQuota(check: QuotaCheck) {
  const parts: string[] = [];

  if (check.missing.vcpu > 0) {
    parts.push(formatVcpu(check.missing.vcpu));
  }

  if (check.missing.ramGb > 0) {
    parts.push(formatRam(check.missing.ramGb));
  }

  if (parts.length === 0) {
    return null;
  }

  return `Не хватает ${parts.join(' и ')}: занято ${formatVcpu(check.used.vcpu)} и ${formatRam(
    check.used.ramGb
  )} из квоты.`;
}

export function checkCandidateQuota(
  instances: ValkeyInstance[],
  candidate: { size: ValkeySize; mode: ValkeyMode },
  excludeInstanceId?: string
) {
  return checkQuota(instances, candidate, excludeInstanceId);
}

/** Последствия ресайза перечислены в SYSTEM.md, раздел 1. */
export function getResizeWarnings(
  mode: ValkeyMode,
  current: ValkeySize,
  next: ValkeySize
): string[] {
  const warnings: string[] = [];

  if (mode === 'single') {
    warnings.push('База будет недоступна во время ресайза, кеш очистится полностью.');
  } else if (next.ramGb < current.ramGb) {
    warnings.push('Уменьшение RAM в отказоустойчивом режиме очищает кеш полностью.');
  } else {
    warnings.push('При смене primary можно потерять последние записи.');
  }

  warnings.push('Ресайз нельзя отменить, новый тариф действует с момента принятия запроса.');
  return warnings;
}

export function getRequestErrorMessage(error: unknown) {
  return error instanceof ApiError
    ? error.message
    : 'Не удалось выполнить запрос. Попробуйте ещё раз.';
}
