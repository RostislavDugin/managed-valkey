import { vi } from 'vitest';
import { TEST_USER_ID } from './render';

export const TEST_INSTANCE_ID = '01930000-0000-7000-8000-000000000002';

export interface CapturedRequest {
  body: Record<string, unknown> | null;
  headers: Headers;
  method: string;
  path: string;
  signal: AbortSignal | null;
}

export function valkeyCatalogDto() {
  return {
    items: [
      { vcpu: 1, ram_gb: 1 },
      { vcpu: 1, ram_gb: 2 },
      { vcpu: 2, ram_gb: 8 },
    ],
    pricing: {
      vcpu_coins_per_hour: 125,
      ram_gb_coins_per_hour: 50,
      hours_per_month: 720,
    },
    connection: { domain: 'valkey.test', port: 41379 },
  };
}

export function valkeyInstanceDto(
  overrides: Record<string, unknown> = {}
): Record<string, unknown> {
  const now = '2026-09-09T00:00:00Z';
  return {
    id: TEST_INSTANCE_ID,
    name: 'cache',
    slug: 'shop-abc123',
    mode: 'single',
    vcpu: 1,
    ram_gb: 1,
    applied_vcpu: 1,
    applied_ram_gb: 1,
    host: 'shop-abc123.valkey.test',
    host_ro: null,
    port: 41379,
    is_whitelist_enabled: false,
    whitelist_cidrs: [],
    maintenance: null,
    password_hint: 'abcd*****',
    password_version: 1,
    applied_password_version: 1,
    status: 'running',
    phase_reason: null,
    desired_generation: 1,
    observed_generation: 1,
    observed_at: now,
    is_stale: false,
    is_updating: false,
    is_recovery_required: false,
    network_verification_status: 'verified',
    network_verified_at: now,
    created_at: now,
    updated_at: now,
    configuration_requested_at: now,
    deletion_requested_at: null,
    ...overrides,
  };
}

export function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function parseBody(init?: RequestInit) {
  if (typeof init?.body !== 'string') {
    return null;
  }
  return JSON.parse(init.body) as Record<string, unknown>;
}

function credentialsDto(instance: Record<string, unknown>) {
  return {
    host: instance.host,
    host_ro: instance.host_ro,
    port: instance.port,
    username: 'app',
    password_hint: instance.password_hint,
    password_version: instance.password_version,
    applied_password_version: instance.applied_password_version,
  };
}

function capacityUsage(instances: Array<Record<string, unknown>>) {
  return instances.reduce<{ vcpu: number; ramGb: number }>(
    (usage, instance) => {
      const nodes = instance.mode === 'ha' ? 3 : 1;
      usage.vcpu += nodes * Math.max(Number(instance.vcpu), Number(instance.applied_vcpu));
      usage.ramGb += nodes * Math.max(Number(instance.ram_gb), Number(instance.applied_ram_gb));
      return usage;
    },
    { vcpu: 0, ramGb: 0 }
  );
}

export function installStatefulValkeyApi(
  initialInstances: Array<Record<string, unknown>> = [valkeyInstanceDto()]
) {
  let instances = initialInstances.map((instance) => ({ ...instance }));
  const requests: CapturedRequest[] = [];

  const fetchMock = vi.fn<typeof fetch>(async (input, init) => {
    const path = String(input);
    const method = (init?.method ?? 'GET').toUpperCase();
    const body = parseBody(init);
    requests.push({
      body,
      headers: new Headers(init?.headers),
      method,
      path,
      signal: init?.signal instanceof AbortSignal ? init.signal : null,
    });

    if (path === '/v1/me' && method === 'GET') {
      const usage = capacityUsage(instances);
      return jsonResponse({
        user: { id: TEST_USER_ID, email: 'user@example.com' },
        quota: { max_vcpu: 4, max_ram_gb: 12 },
        usage: { used_vcpu: usage.vcpu, used_ram_gb: usage.ramGb },
      });
    }

    if (path === '/v1/managed/valkey/capacity' && method === 'GET') {
      const usage = capacityUsage(instances);
      return jsonResponse({
        user: {
          limit: { vcpu: 4, ram_gb: 12 },
          used: { vcpu: usage.vcpu, ram_gb: usage.ramGb },
        },
        cluster: {
          limit: { vcpu: 12, ram_gb: 48 },
          used: { vcpu: usage.vcpu, ram_gb: usage.ramGb },
        },
        instances: { limit: 32, used: instances.length },
      });
    }

    if (path === '/v1/managed/valkey/sizes' && method === 'GET') {
      return jsonResponse(valkeyCatalogDto());
    }

    if (path === '/v1/managed/valkey/instances' && method === 'GET') {
      return jsonResponse({ items: instances });
    }

    if (path === '/v1/managed/valkey/instances' && method === 'POST') {
      const created = valkeyInstanceDto({
        id: '01930000-0000-7000-8000-000000000003',
        name: body?.name,
        slug: `${String(body?.prefix)}-new123`,
        mode: body?.mode,
        vcpu: body?.vcpu,
        ram_gb: body?.ram_gb,
        applied_vcpu: 0,
        applied_ram_gb: 0,
        host: `${String(body?.prefix)}-new123.valkey.test`,
        is_whitelist_enabled: body?.is_whitelist_enabled,
        whitelist_cidrs: body?.whitelist_cidrs,
        password_hint: `${String(body?.password).slice(0, 4)}*****`,
        applied_password_version: 0,
        status: 'provisioning',
        desired_generation: 1,
        observed_generation: 0,
        observed_at: null,
        is_stale: true,
        is_updating: true,
        network_verification_status: 'unknown',
        network_verified_at: null,
      });
      instances = [created, ...instances];
      return jsonResponse(created, 202);
    }

    const pathname = path.split('?')[0];
    const match = pathname.match(/^\/v1\/managed\/valkey\/instances\/([^/]+)(?:\/(.+))?$/);
    if (!match) {
      return jsonResponse(
        { error: { code: 'NOT_FOUND', message: 'Маршрут не найден', details: {} } },
        404
      );
    }

    const [, instanceId, action] = match;
    const index = instances.findIndex((instance) => instance.id === instanceId);
    if (index < 0) {
      return jsonResponse(
        { error: { code: 'NOT_FOUND', message: 'База не найдена', details: {} } },
        404
      );
    }
    const current = instances[index];

    if (!action && method === 'GET') {
      return jsonResponse(current);
    }

    if (!action && method === 'PATCH') {
      const maintenance = body?.maintenance;
      const updated = {
        ...current,
        ...(body?.name === undefined ? {} : { name: body.name }),
        ...(maintenance === undefined ? {} : { maintenance }),
      };
      instances[index] = updated;
      return jsonResponse(updated);
    }

    if (!action && method === 'DELETE') {
      instances[index] = {
        ...current,
        status: 'deleting',
        deletion_requested_at: '2026-09-09T00:01:00Z',
      };
      return new Response(null, { status: 202 });
    }

    if (action === 'credentials' && method === 'GET') {
      return jsonResponse(credentialsDto(current));
    }

    if (action === 'metrics' && method === 'GET') {
      return jsonResponse({
        from: '2026-09-09T00:00:00Z',
        to: '2026-09-09T00:05:00Z',
        step_seconds: 10,
        nodes: [],
      });
    }

    if (action === 'credentials/rotate' && method === 'POST') {
      const version = Number(current.password_version) + 1;
      instances[index] = {
        ...current,
        password_hint: `${String(body?.password).slice(0, 4)}*****`,
        password_version: version,
        desired_generation: Number(current.desired_generation) + 1,
        is_updating: true,
      };
      return jsonResponse(credentialsDto(instances[index]), 202);
    }

    if (action === 'resize' && method === 'POST') {
      instances[index] = {
        ...current,
        vcpu: body?.vcpu,
        ram_gb: body?.ram_gb,
        desired_generation: Number(current.desired_generation) + 1,
        is_updating: true,
      };
      return jsonResponse(instances[index], 202);
    }

    if (action === 'whitelist' && method === 'PUT') {
      instances[index] = {
        ...current,
        is_whitelist_enabled: body?.is_whitelist_enabled,
        whitelist_cidrs: body?.whitelist_cidrs,
        desired_generation: Number(current.desired_generation) + 1,
        is_updating: true,
      };
      return jsonResponse(instances[index], 202);
    }

    return jsonResponse(
      { error: { code: 'NOT_FOUND', message: 'Маршрут не найден', details: {} } },
      404
    );
  });

  window.fetch = fetchMock;
  globalThis.fetch = fetchMock;

  return {
    fetchMock,
    requests,
    getInstances: () => instances.map((instance) => ({ ...instance })),
  };
}
