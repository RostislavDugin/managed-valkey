import {
  getTotalResources,
  isValidSize,
  RAM_OPTIONS,
  VALKEY_PLANS,
  VCPU_OPTIONS,
  type ValkeyInstance,
  type ValkeyMode,
  type ValkeySize,
} from './valkey';

/** Квота пользователя по умолчанию из SYSTEM.md. Интерфейса для её правки нет. */
export const USER_QUOTA = { vcpu: 4, ramGb: 16 } as const;

export interface QuotaAmount {
  vcpu: number;
  ramGb: number;
}

export interface QuotaCheck {
  fits: boolean;
  used: QuotaAmount;
  available: QuotaAmount;
  required: QuotaAmount;
  missing: QuotaAmount;
}

/**
 * При изменении тарифа текущий размер базы исключается: иначе он считался бы
 * дважды и не дал бы уменьшить базу.
 */
export function getQuotaUsage(
  instances: ValkeyInstance[],
  excludeInstanceId?: string
): QuotaAmount {
  return instances
    .filter((instance) => instance.id !== excludeInstanceId)
    .reduce(
      (usage, instance) => {
        const total = getTotalResources(instance, instance.mode);
        return { vcpu: usage.vcpu + total.vcpu, ramGb: usage.ramGb + total.ramGb };
      },
      { vcpu: 0, ramGb: 0 }
    );
}

export function checkQuota(
  instances: ValkeyInstance[],
  candidate: { size: ValkeySize; mode: ValkeyMode },
  excludeInstanceId?: string
): QuotaCheck {
  const used = getQuotaUsage(instances, excludeInstanceId);
  const available = {
    vcpu: USER_QUOTA.vcpu - used.vcpu,
    ramGb: USER_QUOTA.ramGb - used.ramGb,
  };
  const required = getTotalResources(candidate.size, candidate.mode);
  const missing = {
    vcpu: Math.max(0, required.vcpu - available.vcpu),
    ramGb: Math.max(0, required.ramGb - available.ramGb),
  };

  return { fits: missing.vcpu === 0 && missing.ramGb === 0, used, available, required, missing };
}

export function findAvailableSize(
  instances: ValkeyInstance[],
  mode: ValkeyMode,
  excludeInstanceId?: string
): ValkeySize | null {
  for (const vcpu of VCPU_OPTIONS) {
    for (const ramGb of RAM_OPTIONS) {
      if (!isValidSize(vcpu, ramGb)) {
        continue;
      }

      const size = { vcpu, ramGb };
      if (checkQuota(instances, { size, mode }, excludeInstanceId).fits) {
        return size;
      }
    }
  }

  return null;
}

export function findLargestAvailableSize(
  instances: ValkeyInstance[],
  mode: ValkeyMode,
  excludeInstanceId?: string
): ValkeySize | null {
  for (const size of [...VALKEY_PLANS].reverse()) {
    if (checkQuota(instances, { size, mode }, excludeInstanceId).fits) {
      return size;
    }
  }

  return null;
}
