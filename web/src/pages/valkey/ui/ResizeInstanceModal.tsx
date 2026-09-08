import { useEffect, useState } from 'react';
import { AlertTriangle, Info } from 'lucide-react';
import { Alert, Anchor, Button, Group, List, Modal, Stack, Text } from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { buttonVariants } from '@/shared/config';
import { resizeInstance } from '../api/valkey-storage';
import {
  formatPriceWithPeriod,
  formatRam,
  formatVcpu,
  getPeriodKopecks,
  MODE_LABELS,
  VALKEY_PLANS,
  type ValkeyInstance,
  type ValkeyRamGb,
  type ValkeySize,
  type ValkeyVcpu,
} from '../model/valkey';
import {
  checkCandidateQuota,
  describeMissingQuota,
  getRequestErrorMessage,
  getResizeWarnings,
  SUPPORT_URL,
} from '../model/valkey-form';
import { SizePlans } from './SizePlans';
import styles from './ValkeyPage.module.css';

interface ResizeInstanceModalProps {
  instance: ValkeyInstance | null;
  instances: ValkeyInstance[];
  ownerId: string;
  onClose: () => void;
  onResized: (instance: ValkeyInstance) => void;
}

export function ResizeInstanceModal({
  instance,
  instances,
  onClose,
  onResized,
  ownerId,
}: ResizeInstanceModalProps) {
  const [size, setSize] = useState<ValkeySize>({ vcpu: 1, ramGb: 1 });
  const [loading, setLoading] = useState(false);

  useEffect(() => {
    if (instance) {
      setSize({ vcpu: instance.vcpu, ramGb: instance.ramGb });
    }
  }, [instance]);

  if (!instance) {
    return null;
  }

  const quota = checkCandidateQuota(instances, { size, mode: instance.mode }, instance.id);
  const changed = size.vcpu !== instance.vcpu || size.ramGb !== instance.ramGb;
  const warnings = getResizeWarnings(instance.mode, instance, size);
  const currentSize = { vcpu: instance.vcpu, ramGb: instance.ramGb } satisfies ValkeySize;
  const hasCurrentPlan = VALKEY_PLANS.some(
    (plan) => plan.vcpu === currentSize.vcpu && plan.ramGb === currentSize.ramGb
  );
  const plans = hasCurrentPlan ? VALKEY_PLANS : [currentSize, ...VALKEY_PLANS];

  const submit = async () => {
    setLoading(true);

    try {
      const updated = await resizeInstance(ownerId, instance.id, {
        vcpu: size.vcpu as ValkeyVcpu,
        ramGb: size.ramGb as ValkeyRamGb,
      });

      onResized(updated);
      notifications.show({
        message: `Новый тариф: ${formatVcpu(updated.vcpu)} и ${formatRam(updated.ramGb)}.`,
        title: 'Тариф изменён',
      });
      onClose();
    } catch (error) {
      notifications.show({
        color: 'red',
        message: getRequestErrorMessage(error),
        title: 'Не удалось изменить тариф',
      });
    } finally {
      setLoading(false);
    }
  };

  return (
    <Modal onClose={onClose} opened radius="h3_xl" size="lg" title="Изменить тариф" centered>
      <Stack gap="h3_md">
        <div className={styles.resizeModeRow}>
          <Text size="h3_sm">Режим</Text>
          <Text c="h3_text_2" size="h3_sm">
            {MODE_LABELS[instance.mode]}, изменить нельзя
          </Text>
        </div>

        <SizePlans
          isAvailable={(candidate) =>
            checkCandidateQuota(instances, { size: candidate, mode: instance.mode }, instance.id)
              .fits
          }
          mode={instance.mode}
          onChange={setSize}
          plans={plans}
          size={size}
        />

        <Text className={styles.resizePrice} fw="var(--h3-fw-bold)" size="h3_md">
          {formatPriceWithPeriod(getPeriodKopecks(size, instance.mode, 'month'), 'month')}
        </Text>

        {!quota.fits && (
          <Alert
            color="h3_bg_accent_1"
            icon={<Info aria-hidden="true" size={16} strokeWidth={1.5} />}
            title="Не хватает квоты"
          >
            <Stack align="flex-start" gap="h3_xs">
              <Text size="h3_sm">
                {describeMissingQuota(quota) ?? 'Выбранный размер не помещается в квоту.'}
              </Text>
              <Anchor href={SUPPORT_URL} rel="noreferrer" size="h3_sm" target="_blank">
                Напишите в поддержку для увеличения квоты
              </Anchor>
            </Stack>
          </Alert>
        )}

        {changed && (
          <Alert
            color="yellow"
            icon={<AlertTriangle aria-hidden="true" size={16} strokeWidth={1.5} />}
            title="Что произойдёт при ресайзе"
          >
            <List spacing="h3_xs">
              {warnings.map((warning) => (
                <List.Item key={warning}>
                  <Text size="h3_sm">{warning}</Text>
                </List.Item>
              ))}
            </List>
          </Alert>
        )}

        <Group justify="flex-end">
          <Button onClick={onClose} type="button" variant={buttonVariants.ghost}>
            Отмена
          </Button>

          <Button
            disabled={!quota.fits || !changed}
            loading={loading}
            onClick={() => void submit()}
            variant={buttonVariants.accent}
          >
            Изменить тариф
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}
