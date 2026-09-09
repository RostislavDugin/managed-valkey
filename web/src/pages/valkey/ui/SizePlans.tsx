import { Group, Radio, SimpleGrid, Stack, Text } from '@mantine/core';
import {
  formatPrice,
  formatRam,
  formatVcpu,
  getPeriodCoins,
  type ValkeyMode,
  type ValkeyPricing,
  type ValkeySize,
} from '../model/valkey';
import { FormRow } from './FormRow';
import styles from './ValkeyPage.module.css';

interface SizePlansProps {
  isAvailable: (size: ValkeySize) => boolean;
  mode: ValkeyMode;
  onChange: (size: ValkeySize) => void;
  plans: readonly ValkeySize[];
  pricing: ValkeyPricing;
  size: ValkeySize;
}

function planValue(size: ValkeySize) {
  return `${size.vcpu}:${size.ramGb}`;
}

export function SizePlans({ isAvailable, mode, onChange, plans, pricing, size }: SizePlansProps) {
  return (
    <FormRow
      fullWidth
      hint="Готовые сочетания процессора и памяти подходят для типичных нагрузок Valkey."
      label="Конфигурация"
    >
      <Radio.Group
        onChange={(value) => {
          const selected = plans.find((plan) => planValue(plan) === value);
          if (selected) {
            onChange(selected);
          }
        }}
        value={planValue(size)}
      >
        <SimpleGrid cols={{ base: 1, mobile: 2 }} spacing={10}>
          {plans.map((plan) => {
            const available = isAvailable(plan);
            const label = `${formatVcpu(plan.vcpu)}, ${formatRam(plan.ramGb)} RAM`;

            return (
              <Radio.Card
                key={planValue(plan)}
                className={styles.planCard}
                data-unavailable={!available || undefined}
                p={10}
                radius="h3_md"
                value={planValue(plan)}
              >
                <Group align="flex-start" gap={10} wrap="nowrap">
                  <Radio.Indicator aria-label={label} color="h3_bg_accent" />

                  <Stack aria-hidden="true" gap={2}>
                    <Text size="h3_sm">
                      {formatVcpu(plan.vcpu)} · {formatRam(plan.ramGb)} RAM
                    </Text>
                    <Text c={available ? 'h3_text_2' : 'red'} size="h3_xs">
                      {formatPrice(getPeriodCoins(plan, mode, 'month', pricing))} / мес.
                      {!available && ' · Не хватает квоты'}
                    </Text>
                  </Stack>
                </Group>
              </Radio.Card>
            );
          })}
        </SimpleGrid>
      </Radio.Group>
    </FormRow>
  );
}
