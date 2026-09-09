import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { seedSession, withProviders } from '../../../../test/render';
import type { ValkeyQuota } from '../model/quota';
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

const quota = {
  limit: { vcpu: 8, ramGb: 32 },
  usage: { vcpu: 1, ramGb: 1 },
} satisfies ValkeyQuota;

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
  it('не отменяет активный запрос после фонового обновления той же базы', async () => {
    let resolveRequest: (response: Response) => void = () => undefined;
    const request = new Promise<Response>((resolve) => {
      resolveRequest = resolve;
    });
    const fetchMock = vi.spyOn(window, 'fetch').mockReturnValue(request);
    const onClose = vi.fn();
    const onRefresh = vi.fn();
    const onResized = vi.fn();
    const user = userEvent.setup();
    const props = { catalog, instance, onClose, onRefresh, onResized, quota };
    const view = render(withProviders(<ResizeInstanceModal {...props} />));

    await user.click(screen.getByRole('radio', { name: /2.*vCPU.*8.*ГБ RAM/ }));
    expect(
      screen.getByText('База будет недоступна во время ресайза, кеш очистится полностью.')
    ).toBeVisible();
    expect(
      screen.getByText('Ресайз нельзя отменить, новый тариф действует с момента принятия запроса.')
    ).toBeVisible();
    await user.click(screen.getByRole('button', { name: 'Изменить тариф' }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledOnce());

    const signal = fetchMock.mock.calls[0][1]?.signal;
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
  });
});
