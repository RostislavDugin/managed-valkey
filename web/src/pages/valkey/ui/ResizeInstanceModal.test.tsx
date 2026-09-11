import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { seedSession, TEST_USER_ID, withProviders } from '../../../../test/render';
import type { ValkeyCatalog, ValkeyInstance } from '../model/valkey';
import { ResizeInstanceModal } from './ResizeInstanceModal';

const now = '2026-09-09T00:00:00Z';
const instance = {
  id: '01930000-0000-7000-8000-000000000002',
  name: 'cache',
  slug: 'shop-abc123',
  mode: 'single',
  vcpu: 1,
  ramGb: 1,
  appliedVcpu: 1,
  appliedRamGb: 1,
  host: 'shop-abc123.valkey.test',
  hostRo: null,
  port: 41379,
  isWhitelistEnabled: false,
  whitelistCidrs: [],
  maintenance: null,
  passwordHint: 'abcd*****',
  passwordVersion: 1,
  appliedPasswordVersion: 1,
  status: 'running',
  phaseReason: null,
  desiredGeneration: 1,
  observedGeneration: 1,
  observedAt: now,
  isStale: false,
  isUpdating: false,
  isRecoveryRequired: false,
  networkVerificationStatus: 'verified',
  networkVerifiedAt: now,
  createdAt: now,
  updatedAt: now,
  configurationRequestedAt: now,
  deletionRequestedAt: null,
} satisfies ValkeyInstance;

const catalog = {
  items: [
    { vcpu: 1, ramGb: 1 },
    { vcpu: 2, ramGb: 8 },
  ],
  pricing: {
    vcpuCoinsPerHour: 125,
    ramGbCoinsPerHour: 50,
    hoursPerMonth: 720,
  },
  connection: { domain: 'valkey.test', port: 41379 },
} satisfies ValkeyCatalog;

function capacityDto(clusterVcpu = 1, clusterRamGb = 1, instances = 1) {
  return {
    user: { limit: { vcpu: 4, ram_gb: 12 }, used: { vcpu: 1, ram_gb: 1 } },
    cluster: {
      limit: { vcpu: 12, ram_gb: 48 },
      used: { vcpu: clusterVcpu, ram_gb: clusterRamGb },
    },
    instances: { limit: 32, used: instances },
  };
}

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function responseDto() {
  return {
    id: instance.id,
    name: instance.name,
    slug: instance.slug,
    mode: instance.mode,
    vcpu: 2,
    ram_gb: 8,
    applied_vcpu: 1,
    applied_ram_gb: 1,
    host: instance.host,
    host_ro: null,
    port: instance.port,
    is_whitelist_enabled: false,
    whitelist_cidrs: [],
    maintenance: null,
    password_hint: instance.passwordHint,
    password_version: 1,
    applied_password_version: 1,
    status: 'running',
    phase_reason: null,
    desired_generation: 2,
    observed_generation: 1,
    observed_at: now,
    is_stale: false,
    is_updating: true,
    is_recovery_required: false,
    network_verification_status: 'verified',
    network_verified_at: now,
    created_at: now,
    updated_at: now,
    configuration_requested_at: now,
    deletion_requested_at: null,
  };
}

beforeEach(() => {
  seedSession();
});

describe('изменение тарифа', () => {
  it('при фоновом обновлении той же базы сохраняет активный запрос изменения тарифа и закрывает окно после ответа', async () => {
    let resolveRequest: (response: Response) => void = () => undefined;
    const request = new Promise<Response>((resolve) => {
      resolveRequest = resolve;
    });
    const fetchMock = vi.spyOn(window, 'fetch').mockImplementation(async (input) => {
      if (String(input) === '/v1/managed/valkey/capacity') {
        return jsonResponse(capacityDto());
      }
      return request;
    });
    const onClose = vi.fn();
    const onRefresh = vi.fn();
    const onResized = vi.fn();
    const user = userEvent.setup();
    const props = {
      accountId: TEST_USER_ID,
      catalog,
      instance,
      onClose,
      onRefresh,
      onResized,
    };
    const view = render(withProviders(<ResizeInstanceModal {...props} />));

    await user.click(screen.getByRole('radio', { name: /2.*vCPU.*8.*ГБ RAM/ }));
    expect(
      screen.getByText('База будет недоступна во время ресайза, кеш очистится полностью.')
    ).toBeVisible();
    expect(
      screen.getByText('Ресайз нельзя отменить, новый тариф действует с момента принятия запроса.')
    ).toBeVisible();
    await user.click(screen.getByRole('button', { name: 'Изменить тариф' }));
    await waitFor(() =>
      expect(
        fetchMock.mock.calls.filter(([path]) => String(path).endsWith('/resize'))
      ).toHaveLength(1)
    );

    const resizeCall = fetchMock.mock.calls.find(([path]) => String(path).endsWith('/resize'));
    const signal = resizeCall?.[1]?.signal;
    view.rerender(
      withProviders(
        <ResizeInstanceModal
          {...props}
          instance={{ ...instance, vcpu: 4, ramGb: 16, desiredGeneration: 2 }}
        />
      )
    );

    expect(signal?.aborted).toBe(false);
    expect(screen.getByRole('radio', { name: /2.*vCPU.*8.*ГБ RAM/ })).toBeChecked();
    resolveRequest(
      new Response(JSON.stringify(responseDto()), {
        status: 202,
        headers: { 'Content-Type': 'application/json' },
      })
    );

    await waitFor(() => expect(onResized).toHaveBeenCalledOnce());
    expect(onClose).toHaveBeenCalledOnce();
    expect(
      fetchMock.mock.calls.filter(([path]) => String(path) === '/v1/managed/valkey/capacity').length
    ).toBeGreaterThanOrEqual(2);
  });

  it('вычитает текущий резерв и не применяет предел числа баз к изменению тарифа', async () => {
    vi.spyOn(window, 'fetch').mockResolvedValue(jsonResponse(capacityDto(11, 41, 32)));
    const user = userEvent.setup();

    render(
      withProviders(
        <ResizeInstanceModal
          accountId={TEST_USER_ID}
          catalog={catalog}
          instance={instance}
          onClose={vi.fn()}
          onRefresh={vi.fn()}
          onResized={vi.fn()}
        />
      )
    );

    await user.click(await screen.findByRole('radio', { name: /2.*vCPU.*8.*ГБ RAM/ }));
    expect(screen.getByRole('button', { name: 'Изменить тариф' })).toBeEnabled();
    expect(screen.queryByText('Достигнут предел числа баз')).not.toBeInTheDocument();
  });

  it('при нехватке общего бюджета показывает Managed Kubernetes без ссылки на поддержку', async () => {
    vi.spyOn(window, 'fetch').mockResolvedValue(jsonResponse(capacityDto(12, 48)));
    const user = userEvent.setup();

    render(
      withProviders(
        <ResizeInstanceModal
          accountId={TEST_USER_ID}
          catalog={catalog}
          instance={instance}
          onClose={vi.fn()}
          onRefresh={vi.fn()}
          onResized={vi.fn()}
        />
      )
    );

    await user.click(await screen.findByRole('radio', { name: /2.*vCPU.*8.*ГБ RAM/ }));
    expect(screen.getByText('Недостаточно ресурсов Managed Kubernetes')).toBeVisible();
    expect(screen.getByRole('button', { name: 'Изменить тариф' })).toBeDisabled();
    expect(
      screen.queryByRole('link', { name: 'Напишите в поддержку для увеличения квоты' })
    ).not.toBeInTheDocument();
  });

  it('при закрытии отменяет выполняющийся запрос доступной ёмкости', async () => {
    let capacitySignal: AbortSignal | null | undefined;
    vi.spyOn(window, 'fetch').mockImplementation(
      (_input, init) =>
        new Promise<Response>(() => {
          capacitySignal = init?.signal;
        })
    );
    const onClose = vi.fn();
    const user = userEvent.setup();

    const view = render(
      withProviders(
        <ResizeInstanceModal
          accountId={TEST_USER_ID}
          catalog={catalog}
          instance={instance}
          onClose={onClose}
          onRefresh={vi.fn()}
          onResized={vi.fn()}
        />
      )
    );

    await user.click(screen.getByRole('button', { name: 'Отмена' }));
    expect(onClose).toHaveBeenCalledOnce();
    view.rerender(
      withProviders(
        <ResizeInstanceModal
          accountId={TEST_USER_ID}
          catalog={catalog}
          instance={null}
          onClose={onClose}
          onRefresh={vi.fn()}
          onResized={vi.fn()}
        />
      )
    );
    expect(capacitySignal?.aborted).toBe(true);
  });
});
