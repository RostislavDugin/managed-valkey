import { ApiError } from '@/shared/api';
import { checkQuota } from '../model/quota';
import {
  generateSlug,
  isValidSize,
  validateWhitelistCidrs,
  validateInstanceName,
  validateInstancePrefix,
  type ValkeyInstance,
  type ValkeyMode,
  type ValkeyRamGb,
  type ValkeySize,
  type ValkeyVcpu,
} from '../model/valkey';

export const VALKEY_INSTANCES_KEY = 'mv_valkey_instances';

const STORAGE_VERSION = 3;
const PREVIOUS_STORAGE_VERSIONS = [1, 2] as const;
const RESPONSE_DELAY_MS = 400;

interface StoredValkeyState {
  version: typeof STORAGE_VERSION;
  instances: ValkeyInstance[];
}

export interface CreateInstanceInput extends ValkeySize {
  name: string;
  prefix: string;
  mode: ValkeyMode;
  isWhitelistEnabled?: boolean;
  whitelistCidrs?: string[];
}

const delay = () => new Promise((resolve) => setTimeout(resolve, RESPONSE_DELAY_MS));

function storage() {
  if (typeof localStorage === 'undefined') {
    throw new ApiError('STORAGE_UNAVAILABLE', 'Хранилище браузера недоступно');
  }
  return localStorage;
}

function corrupted(): never {
  throw new ApiError(
    'STORAGE_CORRUPTED',
    'Сохранённые данные не удалось прочитать. Повторите попытку.'
  );
}

function isValidInstance(value: unknown): value is ValkeyInstance {
  if (!value || typeof value !== 'object') {
    return false;
  }

  const instance = value as Record<string, unknown>;
  return (
    typeof instance.id === 'string' &&
    typeof instance.ownerId === 'string' &&
    typeof instance.name === 'string' &&
    typeof instance.prefix === 'string' &&
    typeof instance.slug === 'string' &&
    (instance.mode === 'single' || instance.mode === 'ha') &&
    typeof instance.vcpu === 'number' &&
    typeof instance.ramGb === 'number' &&
    isValidSize(instance.vcpu, instance.ramGb) &&
    typeof instance.isWhitelistEnabled === 'boolean' &&
    Array.isArray(instance.whitelistCidrs) &&
    instance.whitelistCidrs.every((cidr) => typeof cidr === 'string') &&
    instance.status === 'running' &&
    typeof instance.createdAt === 'string' &&
    typeof instance.updatedAt === 'string'
  );
}

/**
 * В браузере может лежать запись без префикса и slug: префикс берётся из
 * имени, потому что до появления отдельного поля имя и было DNS-именем.
 */
function addSlug(value: unknown) {
  if (!value || typeof value !== 'object' || 'slug' in value) {
    return value;
  }

  const instance = value as Record<string, unknown>;
  const prefix = typeof instance.name === 'string' ? instance.name.slice(0, 20) : '';

  return { ...instance, prefix, slug: generateSlug(prefix) };
}

function addWhitelist(value: unknown) {
  if (!value || typeof value !== 'object') {
    return value;
  }

  return { isWhitelistEnabled: false, whitelistCidrs: [], ...value };
}

/**
 * Повреждённые данные дают ошибку загрузки, а не пустой список: очистка
 * хранилища выглядела бы для пользователя как удаление всех баз.
 */
function readState(): StoredValkeyState {
  const value = storage().getItem(VALKEY_INSTANCES_KEY);
  if (!value) {
    return { version: STORAGE_VERSION, instances: [] };
  }

  let parsed: unknown;
  try {
    parsed = JSON.parse(value);
  } catch {
    corrupted();
  }

  if (!parsed || typeof parsed !== 'object') {
    corrupted();
  }

  const state = parsed as Record<string, unknown>;
  const known =
    state.version === STORAGE_VERSION ||
    (PREVIOUS_STORAGE_VERSIONS as readonly unknown[]).includes(state.version);

  if (!known || !Array.isArray(state.instances)) {
    corrupted();
  }

  const instances = (state.instances as unknown[]).map((instance) => {
    const withSlug = state.version === 1 ? addSlug(instance) : instance;
    return state.version === STORAGE_VERSION ? withSlug : addWhitelist(withSlug);
  });

  if (!instances.every(isValidInstance)) {
    corrupted();
  }

  return { version: STORAGE_VERSION, instances: instances as ValkeyInstance[] };
}

function writeState(instances: ValkeyInstance[]) {
  const state: StoredValkeyState = { version: STORAGE_VERSION, instances };
  storage().setItem(VALKEY_INSTANCES_KEY, JSON.stringify(state));
}

/** Копия DTO: страницы не должны править то, что лежит в хранилище. */
function toDto(instance: ValkeyInstance): ValkeyInstance {
  return { ...instance, whitelistCidrs: [...instance.whitelistCidrs] };
}

function ownedBy(instances: ValkeyInstance[], ownerId: string) {
  return instances.filter((instance) => instance.ownerId === ownerId);
}

function findOwned(instances: ValkeyInstance[], ownerId: string, instanceId: string) {
  const instance = instances.find(
    (candidate) => candidate.id === instanceId && candidate.ownerId === ownerId
  );

  if (!instance) {
    throw new ApiError('NOT_FOUND', 'База не найдена');
  }

  return instance;
}

function assertNameFree(
  instances: ValkeyInstance[],
  ownerId: string,
  name: string,
  exceptInstanceId?: string
) {
  const message = validateInstanceName(name);
  if (message) {
    throw new ApiError('VALIDATION_FAILED', message);
  }

  const taken = ownedBy(instances, ownerId).some(
    (instance) => instance.name === name && instance.id !== exceptInstanceId
  );

  if (taken) {
    throw new ApiError('CONFLICT', 'База с таким именем уже существует');
  }
}

function assertPrefix(prefix: string) {
  const message = validateInstancePrefix(prefix);
  if (message) {
    throw new ApiError('VALIDATION_FAILED', message);
  }
}

function assertQuota(
  instances: ValkeyInstance[],
  ownerId: string,
  candidate: { size: ValkeySize; mode: ValkeyMode },
  exceptInstanceId?: string
) {
  const check = checkQuota(ownedBy(instances, ownerId), candidate, exceptInstanceId);
  if (!check.fits) {
    throw new ApiError('QUOTA_EXCEEDED', 'Размер не помещается в квоту', {
      details: { missing: check.missing, available: check.available },
    });
  }
}

function assertSize(size: ValkeySize) {
  if (!isValidSize(size.vcpu, size.ramGb)) {
    throw new ApiError('VALIDATION_FAILED', 'Такого размера нет в тарифной сетке');
  }
}

function assertWhitelist(isEnabled: boolean, cidrs: string[]) {
  if (!isEnabled) {
    return;
  }

  const message = validateWhitelistCidrs(cidrs.join('\n'));
  if (message) {
    throw new ApiError('VALIDATION_FAILED', message);
  }
}

export async function listInstances(ownerId: string): Promise<ValkeyInstance[]> {
  await delay();
  return ownedBy(readState().instances, ownerId).map(toDto);
}

export async function getInstance(ownerId: string, instanceId: string): Promise<ValkeyInstance> {
  await delay();
  return toDto(findOwned(readState().instances, ownerId, instanceId));
}

export async function createInstance(
  ownerId: string,
  input: CreateInstanceInput
): Promise<ValkeyInstance> {
  await delay();
  const { instances } = readState();
  const size: ValkeySize = { vcpu: input.vcpu, ramGb: input.ramGb };

  assertSize(size);
  assertNameFree(instances, ownerId, input.name);
  assertPrefix(input.prefix);
  assertWhitelist(input.isWhitelistEnabled ?? false, input.whitelistCidrs ?? []);
  assertQuota(instances, ownerId, { size, mode: input.mode });

  const now = new Date().toISOString();
  const instance: ValkeyInstance = {
    id: crypto.randomUUID(),
    ownerId,
    name: input.name,
    prefix: input.prefix,
    slug: generateSlug(
      input.prefix,
      instances.map((candidate) => candidate.slug)
    ),
    mode: input.mode,
    isWhitelistEnabled: input.isWhitelistEnabled ?? false,
    whitelistCidrs: input.whitelistCidrs ?? [],
    vcpu: input.vcpu,
    ramGb: input.ramGb,
    status: 'running',
    createdAt: now,
    updatedAt: now,
  };

  writeState([...instances, instance]);
  return toDto(instance);
}

export async function renameInstance(
  ownerId: string,
  instanceId: string,
  input: { name: string }
): Promise<ValkeyInstance> {
  await delay();
  const { instances } = readState();
  const instance = findOwned(instances, ownerId, instanceId);

  assertNameFree(instances, ownerId, input.name, instanceId);

  const updated: ValkeyInstance = {
    ...instance,
    name: input.name,
    updatedAt: new Date().toISOString(),
  };

  writeState(instances.map((candidate) => (candidate.id === instanceId ? updated : candidate)));
  return toDto(updated);
}

export async function resizeInstance(
  ownerId: string,
  instanceId: string,
  input: { vcpu: ValkeyVcpu; ramGb: ValkeyRamGb }
): Promise<ValkeyInstance> {
  await delay();
  const { instances } = readState();
  const instance = findOwned(instances, ownerId, instanceId);
  const size: ValkeySize = { vcpu: input.vcpu, ramGb: input.ramGb };

  assertSize(size);
  assertQuota(instances, ownerId, { size, mode: instance.mode }, instanceId);

  const updated: ValkeyInstance = {
    ...instance,
    vcpu: input.vcpu,
    ramGb: input.ramGb,
    updatedAt: new Date().toISOString(),
  };

  writeState(instances.map((candidate) => (candidate.id === instanceId ? updated : candidate)));
  return toDto(updated);
}

export async function deleteInstance(ownerId: string, instanceId: string): Promise<void> {
  await delay();
  const { instances } = readState();
  const remaining = instances.filter(
    (candidate) => candidate.id !== instanceId || candidate.ownerId !== ownerId
  );

  if (remaining.length !== instances.length) {
    writeState(remaining);
  }
}
