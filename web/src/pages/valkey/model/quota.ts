import { getNodeCount, type ValkeyInstance, type ValkeyMode, type ValkeySize } from './valkey';

export interface QuotaAmount {
  vcpu: number;
  ramGb: number;
}

export interface ValkeyQuota {
  limit: QuotaAmount;
  usage: QuotaAmount;
}

export interface QuotaCheck {
  fits: boolean;
  used: QuotaAmount;
  available: QuotaAmount;
  required: QuotaAmount;
  requested: QuotaAmount;
  missing: QuotaAmount;
}

export function getInstanceReserve(instance: ValkeyInstance): QuotaAmount {
  const nodes = getNodeCount(instance.mode);
  return {
    vcpu: nodes * Math.max(instance.vcpu, instance.appliedVcpu),
    ramGb: nodes * Math.max(instance.ramGb, instance.appliedRamGb),
  };
}

export function checkQuota(
  quota: ValkeyQuota,
  candidate: { size: ValkeySize; mode: ValkeyMode },
  current?: ValkeyInstance
): QuotaCheck {
  const previous = current ? getInstanceReserve(current) : { vcpu: 0, ramGb: 0 };
  const nodes = getNodeCount(candidate.mode);
  const required = {
    vcpu: nodes * Math.max(candidate.size.vcpu, current?.appliedVcpu ?? 0),
    ramGb: nodes * Math.max(candidate.size.ramGb, current?.appliedRamGb ?? 0),
  };
  const requested = {
    vcpu: quota.usage.vcpu - previous.vcpu + required.vcpu,
    ramGb: quota.usage.ramGb - previous.ramGb + required.ramGb,
  };
  const missing = {
    vcpu: Math.max(0, requested.vcpu - quota.limit.vcpu),
    ramGb: Math.max(0, requested.ramGb - quota.limit.ramGb),
  };
  const fitsVcpu = requested.vcpu <= quota.limit.vcpu || requested.vcpu <= quota.usage.vcpu;
  const fitsRam = requested.ramGb <= quota.limit.ramGb || requested.ramGb <= quota.usage.ramGb;

  return {
    fits: fitsVcpu && fitsRam,
    used: quota.usage,
    available: {
      vcpu: Math.max(0, quota.limit.vcpu - quota.usage.vcpu),
      ramGb: Math.max(0, quota.limit.ramGb - quota.usage.ramGb),
    },
    required,
    requested,
    missing,
  };
}

export function findAvailableSize(
  sizes: readonly ValkeySize[],
  quota: ValkeyQuota,
  mode: ValkeyMode,
  current?: ValkeyInstance
) {
  return sizes.find((size) => checkQuota(quota, { size, mode }, current).fits) ?? null;
}

export function findLargestAvailableSize(
  sizes: readonly ValkeySize[],
  quota: ValkeyQuota,
  mode: ValkeyMode,
  current?: ValkeyInstance
) {
  return sizes.findLast((size) => checkQuota(quota, { size, mode }, current).fits) ?? null;
}
