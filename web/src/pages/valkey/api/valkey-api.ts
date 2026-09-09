import { apiRequest } from '@/shared/api';
import type { ValkeyQuota } from '../model/quota';
import type {
  ValkeyCatalog,
  ValkeyInstance,
  ValkeyMaintenance,
  ValkeyMode,
  ValkeyNetworkVerificationStatus,
  ValkeySize,
} from '../model/valkey';
import type { ValkeyCredentials } from '../model/valkey-credentials';

interface ValkeyCatalogDto {
  items: Array<{ vcpu: number; ram_gb: number }>;
  pricing: {
    vcpu_coins_per_hour: number;
    ram_gb_coins_per_hour: number;
    hours_per_month: number;
  };
  connection: { domain: string; port: number };
}

interface CurrentUserDto {
  quota: { max_vcpu: number; max_ram_gb: number };
  usage: { used_vcpu: number; used_ram_gb: number };
}

interface ValkeyInstanceDto {
  id: string;
  name: string;
  slug: string;
  mode: ValkeyMode;
  vcpu: number;
  ram_gb: number;
  applied_vcpu: number;
  applied_ram_gb: number;
  host: string;
  host_ro: string | null;
  port: number;
  is_whitelist_enabled: boolean;
  whitelist_cidrs: string[];
  maintenance: { dow: number; hour_utc: number; duration_min: number } | null;
  password_hint: string;
  password_version: number;
  applied_password_version: number;
  status: ValkeyInstance['status'];
  phase_reason: string | null;
  desired_generation: number;
  observed_generation: number;
  observed_at: string | null;
  is_stale: boolean;
  is_updating: boolean;
  is_recovery_required: boolean;
  network_verification_status: ValkeyNetworkVerificationStatus;
  network_verified_at: string | null;
  created_at: string;
  updated_at: string;
  configuration_requested_at: string;
  deletion_requested_at: string | null;
}

interface ValkeyCredentialsDto {
  host: string;
  host_ro: string | null;
  port: number;
  username: 'app';
  password_hint: string;
  password_version: number;
  applied_password_version: number;
}

function mapInstance(dto: ValkeyInstanceDto): ValkeyInstance {
  return {
    id: dto.id,
    name: dto.name,
    slug: dto.slug,
    mode: dto.mode,
    vcpu: dto.vcpu,
    ramGb: dto.ram_gb,
    appliedVcpu: dto.applied_vcpu,
    appliedRamGb: dto.applied_ram_gb,
    host: dto.host,
    hostRo: dto.host_ro,
    port: dto.port,
    isWhitelistEnabled: dto.is_whitelist_enabled,
    whitelistCidrs: dto.whitelist_cidrs,
    maintenance: dto.maintenance
      ? {
          dow: dto.maintenance.dow,
          hourUtc: dto.maintenance.hour_utc,
          durationMin: dto.maintenance.duration_min,
        }
      : null,
    passwordHint: dto.password_hint,
    passwordVersion: dto.password_version,
    appliedPasswordVersion: dto.applied_password_version,
    status: dto.status,
    phaseReason: dto.phase_reason,
    desiredGeneration: dto.desired_generation,
    observedGeneration: dto.observed_generation,
    observedAt: dto.observed_at,
    isStale: dto.is_stale,
    isUpdating: dto.is_updating,
    isRecoveryRequired: dto.is_recovery_required,
    networkVerificationStatus: dto.network_verification_status,
    networkVerifiedAt: dto.network_verified_at,
    createdAt: dto.created_at,
    updatedAt: dto.updated_at,
    configurationRequestedAt: dto.configuration_requested_at,
    deletionRequestedAt: dto.deletion_requested_at,
  };
}

function mapCredentials(dto: ValkeyCredentialsDto): ValkeyCredentials {
  return {
    host: dto.host,
    hostRo: dto.host_ro,
    port: dto.port,
    username: dto.username,
    passwordHint: dto.password_hint,
    passwordVersion: dto.password_version,
    appliedPasswordVersion: dto.applied_password_version,
  };
}

export async function getValkeyCatalog(signal?: AbortSignal): Promise<ValkeyCatalog> {
  const dto = await apiRequest<ValkeyCatalogDto>('/managed/valkey/sizes', { signal });
  return {
    items: dto.items.map((item) => ({ vcpu: item.vcpu, ramGb: item.ram_gb })),
    pricing: {
      vcpuCoinsPerHour: dto.pricing.vcpu_coins_per_hour,
      ramGbCoinsPerHour: dto.pricing.ram_gb_coins_per_hour,
      hoursPerMonth: dto.pricing.hours_per_month,
    },
    connection: dto.connection,
  };
}

export async function getValkeyQuota(signal?: AbortSignal): Promise<ValkeyQuota> {
  const dto = await apiRequest<CurrentUserDto>('/me', { signal });
  return {
    limit: { vcpu: dto.quota.max_vcpu, ramGb: dto.quota.max_ram_gb },
    usage: { vcpu: dto.usage.used_vcpu, ramGb: dto.usage.used_ram_gb },
  };
}

export async function listInstances(signal?: AbortSignal): Promise<ValkeyInstance[]> {
  const dto = await apiRequest<{ items: ValkeyInstanceDto[] }>('/managed/valkey/instances', {
    signal,
  });
  return dto.items.map(mapInstance);
}

export async function getInstance(instanceId: string, signal?: AbortSignal) {
  const dto = await apiRequest<ValkeyInstanceDto>(`/managed/valkey/instances/${instanceId}`, {
    signal,
  });
  return mapInstance(dto);
}

export interface CreateInstanceInput extends ValkeySize {
  name: string;
  prefix: string;
  mode: ValkeyMode;
  password: string;
  isWhitelistEnabled: boolean;
  whitelistCidrs: string[];
}

export async function createInstance(
  input: CreateInstanceInput,
  idempotencyKey: string,
  signal?: AbortSignal
) {
  const dto = await apiRequest<ValkeyInstanceDto>('/managed/valkey/instances', {
    method: 'POST',
    headers: { 'Idempotency-Key': idempotencyKey },
    body: JSON.stringify({
      name: input.name,
      prefix: input.prefix,
      mode: input.mode,
      vcpu: input.vcpu,
      ram_gb: input.ramGb,
      password: input.password,
      is_whitelist_enabled: input.isWhitelistEnabled,
      whitelist_cidrs: input.whitelistCidrs,
    }),
    signal,
  });
  return mapInstance(dto);
}

export async function patchInstance(
  instanceId: string,
  input: { name?: string; maintenance?: ValkeyMaintenance | null },
  signal?: AbortSignal
) {
  const body: Record<string, unknown> = {};
  if (input.name !== undefined) {
    body.name = input.name;
  }
  if (input.maintenance !== undefined) {
    body.maintenance = input.maintenance
      ? {
          dow: input.maintenance.dow,
          hour_utc: input.maintenance.hourUtc,
          duration_min: input.maintenance.durationMin,
        }
      : null;
  }
  const dto = await apiRequest<ValkeyInstanceDto>(`/managed/valkey/instances/${instanceId}`, {
    method: 'PATCH',
    body: JSON.stringify(body),
    retry: true,
    signal,
  });
  return mapInstance(dto);
}

export async function resizeInstance(
  instanceId: string,
  size: ValkeySize,
  idempotencyKey: string,
  signal?: AbortSignal
) {
  const dto = await apiRequest<ValkeyInstanceDto>(
    `/managed/valkey/instances/${instanceId}/resize`,
    {
      method: 'POST',
      headers: { 'Idempotency-Key': idempotencyKey },
      body: JSON.stringify({ vcpu: size.vcpu, ram_gb: size.ramGb }),
      signal,
    }
  );
  return mapInstance(dto);
}

export async function updateWhitelist(
  instanceId: string,
  input: { isWhitelistEnabled: boolean; whitelistCidrs: string[] },
  signal?: AbortSignal
) {
  const dto = await apiRequest<ValkeyInstanceDto>(
    `/managed/valkey/instances/${instanceId}/whitelist`,
    {
      method: 'PUT',
      body: JSON.stringify({
        is_whitelist_enabled: input.isWhitelistEnabled,
        whitelist_cidrs: input.whitelistCidrs,
      }),
      retry: true,
      signal,
    }
  );
  return mapInstance(dto);
}

export async function getValkeyCredentials(instanceId: string, signal?: AbortSignal) {
  const dto = await apiRequest<ValkeyCredentialsDto>(
    `/managed/valkey/instances/${instanceId}/credentials`,
    { signal }
  );
  return mapCredentials(dto);
}

export async function rotateValkeyPassword(
  instanceId: string,
  input: { password: string; expectedPasswordVersion: number },
  idempotencyKey: string,
  signal?: AbortSignal
) {
  const dto = await apiRequest<ValkeyCredentialsDto>(
    `/managed/valkey/instances/${instanceId}/credentials/rotate`,
    {
      method: 'POST',
      headers: { 'Idempotency-Key': idempotencyKey },
      body: JSON.stringify({
        password: input.password,
        expected_password_version: input.expectedPasswordVersion,
      }),
      signal,
    }
  );
  return mapCredentials(dto);
}

export function deleteInstance(instanceId: string, signal?: AbortSignal) {
  return apiRequest<void>(`/managed/valkey/instances/${instanceId}`, {
    method: 'DELETE',
    retry: true,
    signal,
  });
}
