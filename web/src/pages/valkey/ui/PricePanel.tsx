import { Info } from 'lucide-react';
import { SegmentedControl, Stack, Text, Tooltip } from '@mantine/core';
import {
  formatPrice,
  formatRam,
  formatVcpu,
  getPriceBreakdown,
  getTotalResources,
  PRICE_PERIOD_LABELS,
  PRICE_PERIOD_SUFFIXES,
  type PricePeriod,
  type ValkeyMode,
  type ValkeySize,
} from '../model/valkey';
import styles from './ValkeyPage.module.css';

interface PricePanelProps {
  mode: ValkeyMode;
  period: PricePeriod;
  size: ValkeySize;
}

interface PricePeriodTabsProps {
  period: PricePeriod;
  onChange: (period: PricePeriod) => void;
}

/** Переключатель стоит в шапке правой колонки, поэтому вынесен из панели. */
export function PricePeriodTabs({ onChange, period }: PricePeriodTabsProps) {
  return (
    <SegmentedControl
      aria-label="Период расчёта"
      classNames={{
        control: styles.periodControl,
        indicator: styles.periodIndicator,
        label: styles.periodLabel,
        root: styles.periodTabs,
      }}
      data={(Object.keys(PRICE_PERIOD_LABELS) as PricePeriod[]).map((value) => ({
        label: PRICE_PERIOD_LABELS[value],
        value,
      }))}
      onChange={(value) => onChange(value as PricePeriod)}
      size="sm"
      value={period}
    />
  );
}

export function PricePanel({ mode, period, size }: PricePanelProps) {
  const breakdown = getPriceBreakdown(size, mode, period);
  const total = getTotalResources(size, mode);

  return (
    <Stack gap={0}>
      <div className={styles.panelRow}>
        <Text size="h3_sm">{formatVcpu(total.vcpu)}</Text>
        <Text size="h3_sm">{formatPrice(breakdown.vcpuKopecks)}</Text>
      </div>

      <div className={styles.panelRow}>
        <Text size="h3_sm">{formatRam(total.ramGb)} RAM</Text>
        <Text size="h3_sm">{formatPrice(breakdown.ramKopecks)}</Text>
      </div>

      <div className={styles.panelRow}>
        <Text size="h3_sm">
          <Text component="span" fw="var(--h3-fw-medium)" size="lg">
            {formatPrice(breakdown.totalKopecks)}
          </Text>{' '}
          {PRICE_PERIOD_SUFFIXES[period]}
        </Text>

        <Tooltip label="* Расчет приблизительный и может отличаться от фактической стоимости">
          <Info
            aria-label="Как рассчитывается стоимость"
            className={styles.formHint}
            role="img"
            size={16}
            strokeWidth={1.5}
            tabIndex={0}
          />
        </Tooltip>
      </div>
    </Stack>
  );
}
