import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
import { withProviders } from '../../../../test/render';
import type { CapacityReason } from '../model/quota';
import type { ValkeySize } from '../model/valkey';
import { SizePlans } from './SizePlans';

const plans = [
  { vcpu: 1, ramGb: 1 },
  { vcpu: 2, ramGb: 2 },
  { vcpu: 4, ramGb: 4 },
] satisfies ValkeySize[];

describe('карточки тарифов', () => {
  it('показывает цену и причину ограничения, но разрешает выбрать недоступный тариф', async () => {
    const onChange = vi.fn();
    const reasons: Record<number, CapacityReason> = {
      1: 'user_quota',
      2: 'cluster_resources',
      4: 'instance_limit',
    };
    const user = userEvent.setup();

    render(
      withProviders(
        <SizePlans
          getAvailabilityReason={(size) => reasons[size.vcpu]}
          mode="single"
          onChange={onChange}
          plans={plans}
          pricing={{ vcpuCoinsPerHour: 1, ramGbCoinsPerHour: 1, hoursPerMonth: 1 }}
          size={plans[0]}
        />
      )
    );

    expect(screen.getByText(/Не хватает квоты/)).toBeVisible();
    expect(screen.getByText(/Недостаточно ресурсов Managed Kubernetes/)).toBeVisible();
    expect(screen.getByText(/Достигнут предел числа баз/)).toBeVisible();
    expect(screen.getAllByText(/\/ мес\./)).toHaveLength(3);

    await user.click(screen.getByRole('radio', { name: /2.*vCPU.*2.*ГБ RAM/ }));
    expect(onChange).toHaveBeenCalledWith({ vcpu: 2, ramGb: 2 });
  });
});
