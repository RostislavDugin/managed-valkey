import { ApiError } from '@/shared/api';
import { checkCapacity, type CapacityCheck, type QuotaCheck, type ValkeyCapacity } from './quota';
import {
  DEFAULT_INSTANCE_PREFIX,
  formatRam,
  formatVcpu,
  generateInstanceName,
  type ValkeyInstance,
  type ValkeyMode,
  type ValkeySize,
} from './valkey';

export const SUPPORT_URL = 'https://t.me/rostislav_dugin';

export interface CreateFormValues extends ValkeySize {
  name: string;
  prefix: string;
  mode: ValkeyMode;
  isWhitelistEnabled: boolean;
  whitelist: string;
  confirmDenyAll: boolean;
  maintenanceEnabled: boolean;
  maintenanceDow: number;
  maintenanceHourUtc: number;
  maintenanceDurationMin: number;
}

export function getCreateFormDefaults(
  instances: ValkeyInstance[],
  sizes: readonly ValkeySize[],
  capacity: ValkeyCapacity
): CreateFormValues {
  const size = sizes.find(
    (candidate) => checkCapacity(capacity, { size: candidate, mode: 'single' }).fits
  ) ??
    sizes[0] ?? { vcpu: 1, ramGb: 1 };

  return {
    name: generateInstanceName(instances.map((instance) => instance.name)),
    prefix: DEFAULT_INSTANCE_PREFIX,
    mode: 'single',
    vcpu: size.vcpu,
    ramGb: size.ramGb,
    isWhitelistEnabled: false,
    whitelist: '',
    confirmDenyAll: false,
    maintenanceEnabled: false,
    maintenanceDow: 0,
    maintenanceHourUtc: 0,
    maintenanceDurationMin: 30,
  };
}

export function validateMaintenanceDow(value: number, enabled: boolean) {
  return enabled && (!Number.isInteger(value) || value < 0 || value > 6)
    ? 'Укажите число от 0 до 6'
    : null;
}

export function validateMaintenanceHourUtc(value: number, enabled: boolean) {
  return enabled && (!Number.isInteger(value) || value < 0 || value > 23)
    ? 'Укажите час от 0 до 23'
    : null;
}

export function validateMaintenanceDurationMin(value: number, enabled: boolean) {
  return enabled && (!Number.isInteger(value) || value < 1 || value > 1440)
    ? 'Укажите длительность от 1 до 1440 минут'
    : null;
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

export function checkCandidateCapacity(
  capacity: ValkeyCapacity,
  candidate: { size: ValkeySize; mode: ValkeyMode },
  current?: ValkeyInstance
): CapacityCheck {
  return checkCapacity(capacity, candidate, current);
}

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
  if (!(error instanceof ApiError)) {
    return 'Не удалось выполнить запрос. Попробуйте ещё раз.';
  }
  if (error.code === 'NOT_ENOUGH_RESOURCES') {
    return error.message;
  }
  if (error.code === 'OPERATION_IN_PROGRESS') {
    const desired = error.details?.desired_generation;
    const observed = error.details?.observed_generation;
    return typeof desired === 'number' && typeof observed === 'number'
      ? `Предыдущее изменение ещё применяется: подтверждено поколение ${observed} из ${desired}.`
      : 'Предыдущее изменение ещё применяется.';
  }
  if (error.code === 'INSTANCE_NOT_READY') {
    if (error.details?.reason === 'deleting') {
      return 'База уже удаляется.';
    }
    if (error.details?.reason === 'stale_observation') {
      return 'Нет свежего подтверждения состояния базы.';
    }
    if (error.details?.reason === 'invalid_phase' && typeof error.details.status === 'string') {
      return `Статус «${error.details.status}» не позволяет выполнить операцию.`;
    }
    return 'Текущее состояние базы не позволяет выполнить операцию.';
  }
  return error.message;
}

export function getFieldErrors(error: ApiError) {
  const fields = error.details?.fields;
  return fields && typeof fields === 'object' ? (fields as Record<string, string>) : {};
}

export function shouldReuseSubmission(error: unknown) {
  return !(error instanceof ApiError) || error.code === 'INVALID_RESPONSE';
}
