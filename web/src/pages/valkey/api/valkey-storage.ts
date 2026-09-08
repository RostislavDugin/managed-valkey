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
import {
  getValkeyPasswordHint,
  validateValkeyPassword,
  type ValkeyCredentials,
} from '../model/valkey-credentials';
export type AuditAction =
  | 'instance.create'
  | 'instance.update'
  | 'instance.resize'
  | 'instance.password.rotate'
  | 'instance.delete';

export interface AuditLogEntry {
  id: string;
  instanceId: string;
  action: AuditAction;
  userEmail: string;
  createdAt: string;
}

export const VALKEY_INSTANCES_KEY = 'mv_valkey_instances';

const STORAGE_VERSION = 5;
const PREVIOUS_STORAGE_VERSIONS = [1, 2, 3, 4] as const;
const RESPONSE_DELAY_MS = 400;
const PASSWORD_APPLICATION_DELAY_MS = 1_200;

interface StoredValkeyInstance extends ValkeyInstance {
  passwordApplyAt?: string;
}

interface StoredValkeyState {
  version: typeof STORAGE_VERSION;
  instances: StoredValkeyInstance[];
  auditLogs: AuditLogEntry[];
}

export interface MutationAuthor {
  userId: string;
  email: string;
}

export interface CreateInstanceInput extends ValkeySize {
  name: string;
  prefix: string;
  mode: ValkeyMode;
  password: string;
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

function isValidInstance(value: unknown): value is StoredValkeyInstance {
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
    typeof instance.passwordHint === 'string' &&
    Number.isInteger(instance.passwordVersion) &&
    Number.isInteger(instance.appliedPasswordVersion) &&
    (instance.passwordVersion as number) >= 1 &&
    (instance.appliedPasswordVersion as number) >= 1 &&
    (instance.appliedPasswordVersion as number) <= (instance.passwordVersion as number) &&
    ((instance.appliedPasswordVersion === instance.passwordVersion &&
      instance.passwordApplyAt === undefined) ||
      (instance.appliedPasswordVersion !== instance.passwordVersion &&
        typeof instance.passwordApplyAt === 'string')) &&
    (instance.status === 'running' || instance.status === 'degraded') &&
    typeof instance.createdAt === 'string' &&
    typeof instance.updatedAt === 'string'
  );
}

function isValidAuditLog(value: unknown): value is AuditLogEntry {
  if (!value || typeof value !== 'object') {
    return false;
  }

  const item = value as Record<string, unknown>;
  return (
    typeof item.id === 'string' &&
    typeof item.instanceId === 'string' &&
    (item.action === 'instance.create' ||
      item.action === 'instance.update' ||
      item.action === 'instance.resize' ||
      item.action === 'instance.password.rotate' ||
      item.action === 'instance.delete') &&
    typeof item.userEmail === 'string' &&
    typeof item.createdAt === 'string'
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

function addCredentials(value: unknown) {
  if (!value || typeof value !== 'object') {
    return value;
  }

  return {
    passwordHint: 'demo*****',
    passwordVersion: 1,
    appliedPasswordVersion: 1,
    ...value,
  };
}

/**
 * Повреждённые данные дают ошибку загрузки, а не пустой список: очистка
 * хранилища выглядела бы для пользователя как удаление всех баз.
 */
function readState(): StoredValkeyState {
  const value = storage().getItem(VALKEY_INSTANCES_KEY);
  if (!value) {
    return { version: STORAGE_VERSION, instances: [], auditLogs: [] };
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
    const withWhitelist = state.version === STORAGE_VERSION ? withSlug : addWhitelist(withSlug);
    return state.version === STORAGE_VERSION ? withWhitelist : addCredentials(withWhitelist);
  });

  if (!instances.every(isValidInstance)) {
    corrupted();
  }

  const auditLogs = state.version === 4 || state.version === STORAGE_VERSION ? state.auditLogs : [];
  if (!Array.isArray(auditLogs) || !auditLogs.every(isValidAuditLog)) {
    corrupted();
  }

  return {
    version: STORAGE_VERSION,
    instances: instances as StoredValkeyInstance[],
    auditLogs: auditLogs as AuditLogEntry[],
  };
}

function writeState(instances: StoredValkeyInstance[], auditLogs: AuditLogEntry[]) {
  const state: StoredValkeyState = { version: STORAGE_VERSION, instances, auditLogs };
  storage().setItem(VALKEY_INSTANCES_KEY, JSON.stringify(state));
}

/** Копия DTO: страницы не должны править то, что лежит в хранилище. */
function toDto(instance: StoredValkeyInstance): ValkeyInstance {
  const { passwordApplyAt: _, ...dto } = instance;
  return { ...dto, whitelistCidrs: [...dto.whitelistCidrs] };
}

function ownedBy(instances: StoredValkeyInstance[], ownerId: string) {
  return instances.filter((instance) => instance.ownerId === ownerId);
}

function findOwned(instances: StoredValkeyInstance[], ownerId: string, instanceId: string) {
  const instance = instances.find(
    (candidate) => candidate.id === instanceId && candidate.ownerId === ownerId
  );

  if (!instance) {
    throw new ApiError('NOT_FOUND', 'База не найдена');
  }

  return instance;
}

function makeAuditLog(
  instanceId: string,
  action: AuditAction,
  author: MutationAuthor,
  createdAt: string
): AuditLogEntry {
  return {
    id: crypto.randomUUID(),
    instanceId,
    action,
    userEmail: author.email,
    createdAt,
  };
}

export function readAuditSnapshot(ownerId: string, instanceId: string) {
  const state = readState();
  findOwned(state.instances, ownerId, instanceId);
  return state.auditLogs
    .filter((item) => item.instanceId === instanceId)
    .map((item) => ({ ...item }));
}

export function readInstanceSnapshot(ownerId: string, instanceId: string) {
  return toDto(findOwned(readState().instances, ownerId, instanceId));
}

function toCredentials(instance: StoredValkeyInstance): ValkeyCredentials {
  return {
    username: 'app',
    passwordHint: instance.passwordHint,
    passwordVersion: instance.passwordVersion,
    appliedPasswordVersion: instance.appliedPasswordVersion,
  };
}

export function readCredentialsSnapshot(ownerId: string, instanceId: string) {
  const state = readState();
  const instance = findOwned(state.instances, ownerId, instanceId);

  if (instance.passwordApplyAt && new Date(instance.passwordApplyAt).getTime() <= Date.now()) {
    const applied: StoredValkeyInstance = {
      ...instance,
      appliedPasswordVersion: instance.passwordVersion,
    };
    delete applied.passwordApplyAt;

    writeState(
      state.instances.map((candidate) => (candidate.id === instanceId ? applied : candidate)),
      state.auditLogs
    );
    return toCredentials(applied);
  }

  return toCredentials(instance);
}

export function rotatePasswordSnapshot(
  author: MutationAuthor,
  instanceId: string,
  input: { password: string; expectedPasswordVersion: number }
) {
  const state = readState();
  const instance = findOwned(state.instances, author.userId, instanceId);
  const passwordError = validateValkeyPassword(input.password);

  if (passwordError) {
    throw new ApiError('VALIDATION_FAILED', passwordError);
  }

  if (instance.passwordVersion !== input.expectedPasswordVersion) {
    throw new ApiError('CONFLICT', 'Версия пароля изменилась');
  }

  if (instance.appliedPasswordVersion !== instance.passwordVersion) {
    throw new ApiError('OPERATION_IN_PROGRESS', 'Предыдущее изменение ещё применяется');
  }

  const now = new Date();
  const updated: StoredValkeyInstance = {
    ...instance,
    passwordHint: getValkeyPasswordHint(input.password),
    passwordVersion: instance.passwordVersion + 1,
    passwordApplyAt: new Date(now.getTime() + PASSWORD_APPLICATION_DELAY_MS).toISOString(),
    updatedAt: now.toISOString(),
  };

  writeState(
    state.instances.map((candidate) => (candidate.id === instanceId ? updated : candidate)),
    [
      ...state.auditLogs,
      makeAuditLog(instanceId, 'instance.password.rotate', author, now.toISOString()),
    ]
  );

  return toCredentials(updated);
}

function assertNameFree(
  instances: StoredValkeyInstance[],
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
  instances: StoredValkeyInstance[],
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
  author: MutationAuthor,
  input: CreateInstanceInput
): Promise<ValkeyInstance> {
  await delay();
  const { instances, auditLogs } = readState();
  const ownerId = author.userId;
  const size: ValkeySize = { vcpu: input.vcpu, ramGb: input.ramGb };
  const passwordError = validateValkeyPassword(input.password);

  if (passwordError) {
    throw new ApiError('VALIDATION_FAILED', passwordError);
  }

  assertSize(size);
  assertNameFree(instances, ownerId, input.name);
  assertPrefix(input.prefix);
  assertWhitelist(input.isWhitelistEnabled ?? false, input.whitelistCidrs ?? []);
  assertQuota(instances, ownerId, { size, mode: input.mode });

  const now = new Date().toISOString();
  const instance: StoredValkeyInstance = {
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
    passwordHint: getValkeyPasswordHint(input.password),
    passwordVersion: 1,
    appliedPasswordVersion: 1,
    vcpu: input.vcpu,
    ramGb: input.ramGb,
    status: 'running',
    createdAt: now,
    updatedAt: now,
  };

  writeState(
    [...instances, instance],
    [...auditLogs, makeAuditLog(instance.id, 'instance.create', author, now)]
  );
  return toDto(instance);
}

export async function renameInstance(
  author: MutationAuthor,
  instanceId: string,
  input: { name: string }
): Promise<ValkeyInstance> {
  await delay();
  const { instances, auditLogs } = readState();
  const ownerId = author.userId;
  const instance = findOwned(instances, ownerId, instanceId);

  assertNameFree(instances, ownerId, input.name, instanceId);

  const now = new Date().toISOString();
  const updated: ValkeyInstance = {
    ...instance,
    name: input.name,
    updatedAt: now,
  };

  writeState(
    instances.map((candidate) => (candidate.id === instanceId ? updated : candidate)),
    [...auditLogs, makeAuditLog(instanceId, 'instance.update', author, now)]
  );
  return toDto(updated);
}

export async function resizeInstance(
  author: MutationAuthor,
  instanceId: string,
  input: { vcpu: ValkeyVcpu; ramGb: ValkeyRamGb }
): Promise<ValkeyInstance> {
  await delay();
  const { instances, auditLogs } = readState();
  const ownerId = author.userId;
  const instance = findOwned(instances, ownerId, instanceId);
  const size: ValkeySize = { vcpu: input.vcpu, ramGb: input.ramGb };

  assertSize(size);
  assertQuota(instances, ownerId, { size, mode: instance.mode }, instanceId);

  const now = new Date().toISOString();
  const updated: ValkeyInstance = {
    ...instance,
    vcpu: input.vcpu,
    ramGb: input.ramGb,
    updatedAt: now,
  };

  writeState(
    instances.map((candidate) => (candidate.id === instanceId ? updated : candidate)),
    [...auditLogs, makeAuditLog(instanceId, 'instance.resize', author, now)]
  );
  return toDto(updated);
}

export async function deleteInstance(author: MutationAuthor, instanceId: string): Promise<void> {
  await delay();
  const { instances, auditLogs } = readState();
  const ownerId = author.userId;
  const deleted = instances.find(
    (candidate) => candidate.id === instanceId && candidate.ownerId === ownerId
  );
  const remaining = instances.filter(
    (candidate) => candidate.id !== instanceId || candidate.ownerId !== ownerId
  );

  if (deleted) {
    const now = new Date().toISOString();
    writeState(remaining, [...auditLogs, makeAuditLog(instanceId, 'instance.delete', author, now)]);
  }
}
