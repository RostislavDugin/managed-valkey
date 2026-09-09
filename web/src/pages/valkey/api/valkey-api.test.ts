import { beforeEach, describe, expect, it, vi } from 'vitest';
import {
  createInstance,
  deleteInstance,
  getInstance,
  getValkeyCatalog,
  getValkeyCredentials,
  getValkeyQuota,
  listInstances,
  patchInstance,
  resizeInstance,
  rotateValkeyPassword,
  updateWhitelist,
} from './valkey-api';

const PASSWORD = '0123456789abcdefghijklmnopqrstuv';
const INSTANCE_ID = '01930000-0000-7000-8000-000000000002';

function instanceDto(overrides: Record<string, unknown> = {}) {
  return {
    id: INSTANCE_ID,
    name: 'cache',
    slug: 'shop-abc123',
    mode: 'single',
    vcpu: 2,
    ram_gb: 4,
    applied_vcpu: 1,
    applied_ram_gb: 2,
    host: 'shop-abc123.valkey.test',
    host_ro: null,
    port: 41379,
    is_whitelist_enabled: true,
    whitelist_cidrs: ['192.0.2.0/24'],
    maintenance: { dow: 0, hour_utc: 1, duration_min: 30 },
    password_hint: '0123*****',
    password_version: 2,
    applied_password_version: 1,
    status: 'provisioning',
    phase_reason: null,
    desired_generation: 3,
    observed_generation: 2,
    observed_at: '2026-09-09T00:00:00Z',
    is_stale: false,
    is_updating: true,
    is_recovery_required: false,
    network_verification_status: 'pending',
    network_verified_at: null,
    created_at: '2026-09-08T23:00:00Z',
    updated_at: '2026-09-09T00:00:00Z',
    configuration_requested_at: '2026-09-09T00:00:00Z',
    deletion_requested_at: null,
    ...overrides,
  };
}

function credentialsDto() {
  return {
    host: 'shop-abc123.valkey.test',
    host_ro: null,
    port: 41379,
    username: 'app',
    password_hint: '0123*****',
    password_version: 2,
    applied_password_version: 1,
  };
}

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

beforeEach(() => {
  localStorage.clear();
  localStorage.setItem('mv_token', 'server-token');
});

describe('клиент Valkey API', () => {
  it('преобразует каталог, квоту, список, карточку и credentials', async () => {
    const fetchMock = vi
      .spyOn(window, 'fetch')
      .mockResolvedValueOnce(
        jsonResponse({
          items: [{ vcpu: 2, ram_gb: 4 }],
          pricing: {
            vcpu_coins_per_hour: 321,
            ram_gb_coins_per_hour: 123,
            hours_per_month: 744,
          },
          connection: { domain: 'valkey.test', port: 12345 },
        })
      )
      .mockResolvedValueOnce(
        jsonResponse({
          quota: { max_vcpu: 9, max_ram_gb: 27 },
          usage: { used_vcpu: 5, used_ram_gb: 11 },
        })
      )
      .mockResolvedValueOnce(jsonResponse({ items: [instanceDto()] }))
      .mockResolvedValueOnce(jsonResponse(instanceDto()))
      .mockResolvedValueOnce(jsonResponse(credentialsDto()));

    await expect(getValkeyCatalog()).resolves.toEqual({
      items: [{ vcpu: 2, ramGb: 4 }],
      pricing: { vcpuCoinsPerHour: 321, ramGbCoinsPerHour: 123, hoursPerMonth: 744 },
      connection: { domain: 'valkey.test', port: 12345 },
    });
    await expect(getValkeyQuota()).resolves.toEqual({
      limit: { vcpu: 9, ramGb: 27 },
      usage: { vcpu: 5, ramGb: 11 },
    });
    const [listed] = await listInstances();
    expect(listed).toMatchObject({
      id: INSTANCE_ID,
      ramGb: 4,
      appliedRamGb: 2,
      status: 'provisioning',
      isUpdating: true,
      maintenance: { dow: 0, hourUtc: 1, durationMin: 30 },
    });
    await expect(getInstance(INSTANCE_ID)).resolves.toEqual(listed);
    await expect(getValkeyCredentials(INSTANCE_ID)).resolves.toMatchObject({
      host: 'shop-abc123.valkey.test',
      port: 41379,
      passwordVersion: 2,
    });
    expect(fetchMock.mock.calls.map(([path]) => path)).toEqual([
      '/v1/managed/valkey/sizes',
      '/v1/me',
      '/v1/managed/valkey/instances',
      `/v1/managed/valkey/instances/${INSTANCE_ID}`,
      `/v1/managed/valkey/instances/${INSTANCE_ID}/credentials`,
    ]);
  });

  it('отправляет серверные DTO и ключи без автора и browser storage', async () => {
    const fetchMock = vi
      .spyOn(window, 'fetch')
      .mockResolvedValueOnce(jsonResponse(instanceDto(), 202))
      .mockResolvedValueOnce(jsonResponse(instanceDto({ name: 'renamed' })))
      .mockResolvedValueOnce(jsonResponse(instanceDto({ vcpu: 4 }), 202))
      .mockResolvedValueOnce(jsonResponse(instanceDto({ whitelist_cidrs: [] }), 202))
      .mockResolvedValueOnce(jsonResponse(credentialsDto(), 202))
      .mockResolvedValueOnce(new Response(null, { status: 202 }));

    await createInstance(
      {
        name: 'cache',
        prefix: 'shop',
        mode: 'ha',
        vcpu: 2,
        ramGb: 4,
        password: PASSWORD,
        isWhitelistEnabled: true,
        whitelistCidrs: ['192.0.2.0/24'],
      },
      'create-key'
    );
    await patchInstance(INSTANCE_ID, {
      name: 'renamed',
      maintenance: { dow: 0, hourUtc: 2, durationMin: 60 },
    });
    await resizeInstance(INSTANCE_ID, { vcpu: 4, ramGb: 8 }, 'resize-key');
    await updateWhitelist(INSTANCE_ID, { isWhitelistEnabled: true, whitelistCidrs: [] });
    await rotateValkeyPassword(
      INSTANCE_ID,
      { password: PASSWORD, expectedPasswordVersion: 2 },
      'rotate-key'
    );
    await deleteInstance(INSTANCE_ID);

    const createInit = fetchMock.mock.calls[0][1];
    expect(JSON.parse(String(createInit?.body))).toEqual({
      name: 'cache',
      prefix: 'shop',
      mode: 'ha',
      vcpu: 2,
      ram_gb: 4,
      password: PASSWORD,
      is_whitelist_enabled: true,
      whitelist_cidrs: ['192.0.2.0/24'],
    });
    expect(new Headers(createInit?.headers).get('Idempotency-Key')).toBe('create-key');
    expect(JSON.parse(String(fetchMock.mock.calls[1][1]?.body))).toEqual({
      name: 'renamed',
      maintenance: { dow: 0, hour_utc: 2, duration_min: 60 },
    });
    expect(new Headers(fetchMock.mock.calls[2][1]?.headers).get('Idempotency-Key')).toBe(
      'resize-key'
    );
    expect(JSON.parse(String(fetchMock.mock.calls[3][1]?.body))).toEqual({
      is_whitelist_enabled: true,
      whitelist_cidrs: [],
    });
    expect(new Headers(fetchMock.mock.calls[4][1]?.headers).get('Idempotency-Key')).toBe(
      'rotate-key'
    );
    expect(fetchMock.mock.calls[5][1]?.method).toBe('DELETE');
    expect(fetchMock.mock.calls.some(([path]) => String(path).includes(PASSWORD))).toBe(false);
    expect(localStorage.getItem('mv_valkey_instances')).toBeNull();
    expect([...Array(localStorage.length).keys()].map((index) => localStorage.key(index))).toEqual([
      'mv_token',
    ]);
  });

  it('не повторяет немедленный отказ по общему бюджету', async () => {
    const fetchMock = vi.spyOn(window, 'fetch').mockResolvedValue(
      jsonResponse(
        {
          error: {
            code: 'NOT_ENOUGH_RESOURCES',
            message: 'Недостаточно свободной квоты кластера',
            details: { reason: 'cluster_quota' },
          },
        },
        422
      )
    );

    await expect(
      createInstance(
        {
          name: 'cache',
          prefix: 'shop',
          mode: 'single',
          vcpu: 1,
          ramGb: 1,
          password: PASSWORD,
          isWhitelistEnabled: false,
          whitelistCidrs: [],
        },
        'one-key'
      )
    ).rejects.toMatchObject({
      code: 'NOT_ENOUGH_RESOURCES',
      details: { reason: 'cluster_quota' },
    });
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});
