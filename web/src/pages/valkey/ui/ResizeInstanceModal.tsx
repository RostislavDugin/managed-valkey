import { useEffect, useRef, useState } from 'react';
import { AlertTriangle, Info } from 'lucide-react';
import { Alert, Anchor, Button, Group, List, Modal, Stack, Text } from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { ApiError } from '@/shared/api';
import { buttonVariants } from '@/shared/config';
import { createUuidV7 } from '@/shared/lib';
import { resizeInstance } from '../api/valkey-api';
import {
  formatPriceWithPeriod,
  formatRam,
  formatVcpu,
  getPeriodCoins,
  MODE_LABELS,
  type ValkeyCatalog,
  type ValkeyInstance,
  type ValkeySize,
} from '../model/valkey';
import { useValkeyCapacityPolling } from '../model/valkey-capacity-polling';
import {
  checkCandidateCapacity,
  describeMissingQuota,
  getRequestErrorMessage,
  getResizeWarnings,
  shouldReuseSubmission,
  SUPPORT_URL,
} from '../model/valkey-form';
import { SizePlans } from './SizePlans';
import styles from './ValkeyPage.module.css';

interface ResizeInstanceModalProps {
  accountId: string;
  catalog: ValkeyCatalog;
  instance: ValkeyInstance | null;
  onClose: () => void;
  onRefresh: () => void;
  onResized: (instance: ValkeyInstance) => void;
}

export function ResizeInstanceModal({
  accountId,
  catalog,
  instance,
  onClose,
  onRefresh,
  onResized,
}: ResizeInstanceModalProps) {
  const [size, setSize] = useState<ValkeySize>({ vcpu: 1, ramGb: 1 });
  const [loading, setLoading] = useState(false);
  const pendingRef = useRef<{ fingerprint: string; idempotencyKey: string } | null>(null);
  const requestControllerRef = useRef<AbortController | null>(null);
  const initializedInstanceRef = useRef<string | null>(null);
  const openedInstanceId = instance?.id ?? null;
  const openedVcpu = instance?.vcpu ?? 1;
  const openedRamGb = instance?.ramGb ?? 1;
  const {
    capacity: capacitySnapshot,
    error: capacityError,
    refresh: refreshCapacity,
  } = useValkeyCapacityPolling(openedInstanceId ? `${accountId}:${openedInstanceId}` : null);

  useEffect(() => {
    if (!openedInstanceId) {
      initializedInstanceRef.current = null;
      return;
    }
    if (initializedInstanceRef.current === openedInstanceId) {
      return;
    }

    initializedInstanceRef.current = openedInstanceId;
    setSize({ vcpu: openedVcpu, ramGb: openedRamGb });
  }, [openedInstanceId, openedRamGb, openedVcpu]);

  useEffect(() => {
    requestControllerRef.current?.abort();
    requestControllerRef.current = null;
    pendingRef.current = null;
    setLoading(false);

    return () => {
      requestControllerRef.current?.abort();
      requestControllerRef.current = null;
      pendingRef.current = null;
    };
  }, [openedInstanceId]);

  if (!instance) {
    return null;
  }

  const capacity = capacitySnapshot
    ? checkCandidateCapacity(capacitySnapshot, { size, mode: instance.mode }, instance)
    : null;
  const changed = size.vcpu !== instance.vcpu || size.ramGb !== instance.ramGb;
  const warnings = getResizeWarnings(instance.mode, instance, size);
  const currentSize = { vcpu: instance.vcpu, ramGb: instance.ramGb } satisfies ValkeySize;
  const hasCurrentPlan = catalog.items.some(
    (plan) => plan.vcpu === currentSize.vcpu && plan.ramGb === currentSize.ramGb
  );
  const plans = hasCurrentPlan ? catalog.items : [currentSize, ...catalog.items];
  const canMutate =
    !instance.isUpdating &&
    !instance.isStale &&
    instance.status !== 'deleting' &&
    (instance.status === 'running' || instance.status === 'degraded');

  const submit = async () => {
    setLoading(true);
    const fingerprint = `${size.vcpu}:${size.ramGb}`;
    const existing = pendingRef.current;
    const submission =
      existing?.fingerprint === fingerprint
        ? existing
        : { fingerprint, idempotencyKey: createUuidV7() };
    pendingRef.current = submission;
    const controller = new AbortController();
    requestControllerRef.current = controller;

    try {
      const updated = await resizeInstance(
        instance.id,
        size,
        submission.idempotencyKey,
        controller.signal
      );
      if (requestControllerRef.current !== controller) {
        return;
      }

      pendingRef.current = null;
      void refreshCapacity();
      onResized(updated);
      notifications.show({
        message: `Новый тариф: ${formatVcpu(updated.vcpu)} и ${formatRam(updated.ramGb)}.`,
        title: 'Тариф изменён',
      });
      onClose();
    } catch (error) {
      if (
        requestControllerRef.current !== controller ||
        (error instanceof DOMException && error.name === 'AbortError')
      ) {
        return;
      }
      if (!shouldReuseSubmission(error)) {
        pendingRef.current = null;
      }
      if (error instanceof ApiError && error.status === 409) {
        onRefresh();
      }
      if (
        error instanceof ApiError &&
        (error.code === 'QUOTA_EXCEEDED' || error.code === 'NOT_ENOUGH_RESOURCES')
      ) {
        onRefresh();
        await refreshCapacity();
      }
      notifications.show({
        color: 'red',
        message: getRequestErrorMessage(error),
        title: 'Не удалось изменить тариф',
      });
    } finally {
      if (requestControllerRef.current === controller) {
        requestControllerRef.current = null;
        setLoading(false);
      }
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
          getAvailabilityReason={(candidate) =>
            capacitySnapshot
              ? checkCandidateCapacity(
                  capacitySnapshot,
                  { size: candidate, mode: instance.mode },
                  instance
                ).reason
              : 'available'
          }
          mode={instance.mode}
          onChange={setSize}
          plans={plans}
          pricing={catalog.pricing}
          size={size}
        />

        <Text className={styles.resizePrice} fw="var(--h3-fw-bold)" size="h3_md">
          {formatPriceWithPeriod(
            getPeriodCoins(size, instance.mode, 'month', catalog.pricing),
            'month'
          )}
        </Text>

        {!capacitySnapshot ? (
          <Alert
            color={capacityError ? 'red' : 'h3_bg_accent_1'}
            icon={<Info aria-hidden="true" size={16} strokeWidth={1.5} />}
            title={capacityError ? 'Не удалось проверить ресурсы' : 'Проверяем доступные ресурсы'}
          >
            <Stack align="flex-start" gap="h3_xs">
              {capacityError ? (
                <Text size="h3_sm">{getRequestErrorMessage(capacityError)}</Text>
              ) : null}
              {capacityError ? (
                <Button onClick={() => void refreshCapacity()} variant={buttonVariants.secondary}>
                  Повторить
                </Button>
              ) : null}
            </Stack>
          </Alert>
        ) : null}

        {capacity?.reason === 'user_quota' ? (
          <Alert
            color="h3_bg_accent_1"
            icon={<Info aria-hidden="true" size={16} strokeWidth={1.5} />}
            title="Не хватает квоты"
          >
            <Stack align="flex-start" gap="h3_xs">
              <Text size="h3_sm">
                {describeMissingQuota(capacity.user) ?? 'Выбранный размер не помещается в квоту.'}
              </Text>
              <Anchor href={SUPPORT_URL} rel="noreferrer" size="h3_sm" target="_blank">
                Напишите в поддержку для увеличения квоты
              </Anchor>
            </Stack>
          </Alert>
        ) : null}

        {capacity?.reason === 'cluster_resources' ? (
          <Alert
            color="h3_bg_accent_1"
            icon={<Info aria-hidden="true" size={16} strokeWidth={1.5} />}
            title="Недостаточно ресурсов Managed Kubernetes"
          >
            <Text size="h3_sm">В кластере сейчас нет ресурсов для выбранной конфигурации.</Text>
          </Alert>
        ) : null}

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
            disabled={!capacity?.fits || !changed || !canMutate}
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
