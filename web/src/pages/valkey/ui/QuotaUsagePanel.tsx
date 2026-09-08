import { Button, Progress, Stack, Text } from '@mantine/core';
import { buttonVariants } from '@/shared/config';
import { getQuotaUsage, USER_QUOTA } from '../model/quota';
import { formatRam, type ValkeyInstance } from '../model/valkey';
import { SUPPORT_URL } from '../model/valkey-form';
import styles from './ValkeyPage.module.css';

interface UsageRowProps {
  format: (value: number) => string;
  label: string;
  total: number;
  used: number;
}

function UsageRow({ format, label, total, used }: UsageRowProps) {
  const percent = total === 0 ? 0 : Math.min(100, Math.round((used / total) * 100));
  const description = `${format(used)} / ${format(total)}`;

  return (
    <Stack className={styles.panelBlock} gap="h3_xs">
      <div className={styles.usageRow}>
        <Text size="h3_sm">{label}</Text>
        <Text c="h3_text_2" size="h3_sm">
          {description}
        </Text>
      </div>

      <Progress
        aria-label={`${label}: занято ${description}`}
        color="green.6"
        size="sm"
        value={percent}
      />
    </Stack>
  );
}

export function QuotaUsagePanel({ instances }: { instances: ValkeyInstance[] }) {
  const usage = getQuotaUsage(instances);

  return (
    <Stack gap={0}>
      <UsageRow format={String} label="vCPU" total={USER_QUOTA.vcpu} used={usage.vcpu} />
      <UsageRow format={formatRam} label="RAM" total={USER_QUOTA.ramGb} used={usage.ramGb} />

      <div className={styles.quotaAction}>
        <Button
          component="a"
          href={SUPPORT_URL}
          rel="noreferrer"
          size="sm"
          target="_blank"
          variant={buttonVariants.secondary}
          fullWidth
        >
          Увеличить через поддержку
        </Button>
      </div>
    </Stack>
  );
}
