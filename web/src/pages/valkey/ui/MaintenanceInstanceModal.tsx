import { useEffect, useRef, useState } from 'react';
import { Button, Group, Modal, NumberInput, Stack, Switch } from '@mantine/core';
import { useForm } from '@mantine/form';
import { notifications } from '@mantine/notifications';
import { ApiError } from '@/shared/api';
import { buttonVariants } from '@/shared/config';
import { patchInstance } from '../api/valkey-api';
import type { ValkeyInstance } from '../model/valkey';
import {
  getRequestErrorMessage,
  validateMaintenanceDow,
  validateMaintenanceDurationMin,
  validateMaintenanceHourUtc,
} from '../model/valkey-form';

interface MaintenanceInstanceModalProps {
  instance: ValkeyInstance | null;
  onClose: () => void;
  onRefresh: () => void;
  onUpdated: (instance: ValkeyInstance) => void;
}

export function MaintenanceInstanceModal({
  instance,
  onClose,
  onRefresh,
  onUpdated,
}: MaintenanceInstanceModalProps) {
  const [loading, setLoading] = useState(false);
  const requestControllerRef = useRef<AbortController | null>(null);
  const initializedInstanceRef = useRef<string | null>(null);
  const openedInstanceId = instance?.id ?? null;
  const form = useForm({
    mode: 'controlled',
    initialValues: {
      enabled: false,
      dow: 0,
      hourUtc: 0,
      durationMin: 30,
    },
    validateInputOnBlur: true,
    validate: {
      dow: (value, values) => validateMaintenanceDow(value, values.enabled),
      hourUtc: (value, values) => validateMaintenanceHourUtc(value, values.enabled),
      durationMin: (value, values) => validateMaintenanceDurationMin(value, values.enabled),
    },
  });
  const { setValues } = form;

  useEffect(() => {
    if (!instance) {
      initializedInstanceRef.current = null;
      return;
    }
    if (initializedInstanceRef.current === instance.id) {
      return;
    }

    initializedInstanceRef.current = instance.id;
    setValues({
      enabled: instance.maintenance !== null,
      dow: instance.maintenance?.dow ?? 0,
      hourUtc: instance.maintenance?.hourUtc ?? 0,
      durationMin: instance.maintenance?.durationMin ?? 30,
    });
  }, [instance, setValues]);

  useEffect(() => {
    requestControllerRef.current?.abort();
    requestControllerRef.current = null;
    setLoading(false);

    return () => {
      requestControllerRef.current?.abort();
      requestControllerRef.current = null;
    };
  }, [openedInstanceId]);

  if (!instance) {
    return null;
  }

  const submit = async (values: typeof form.values) => {
    setLoading(true);
    const controller = new AbortController();
    requestControllerRef.current = controller;

    try {
      const updated = await patchInstance(
        instance.id,
        {
          maintenance: values.enabled
            ? { dow: values.dow, hourUtc: values.hourUtc, durationMin: values.durationMin }
            : null,
        },
        controller.signal
      );
      if (requestControllerRef.current !== controller) {
        return;
      }

      onUpdated(updated);
      notifications.show({
        message: values.enabled ? 'Новое окно сохранено.' : 'Окно обслуживания очищено.',
        title: 'Окно обслуживания изменено',
      });
      onClose();
    } catch (error) {
      if (
        requestControllerRef.current !== controller ||
        (error instanceof DOMException && error.name === 'AbortError')
      ) {
        return;
      }
      if (error instanceof ApiError && error.status === 409) {
        onRefresh();
      }
      notifications.show({
        color: 'red',
        message: getRequestErrorMessage(error),
        title: 'Не удалось изменить окно обслуживания',
      });
    } finally {
      if (requestControllerRef.current === controller) {
        requestControllerRef.current = null;
        setLoading(false);
      }
    }
  };

  return (
    <Modal onClose={onClose} opened radius="h3_xl" title="Окно обслуживания" centered>
      <form onSubmit={form.onSubmit((values) => void submit(values))}>
        <Stack gap="h3_md">
          <Switch
            label="Задать окно обслуживания"
            {...form.getInputProps('enabled', { type: 'checkbox' })}
          />

          {form.values.enabled ? (
            <>
              <NumberInput
                label="День недели, 0–6"
                max={6}
                min={0}
                {...form.getInputProps('dow')}
              />
              <NumberInput
                label="Час UTC, 0–23"
                max={23}
                min={0}
                {...form.getInputProps('hourUtc')}
              />
              <NumberInput
                label="Длительность, минуты"
                max={1440}
                min={1}
                {...form.getInputProps('durationMin')}
              />
            </>
          ) : null}

          <Group justify="flex-end">
            <Button onClick={onClose} type="button" variant={buttonVariants.ghost}>
              Отмена
            </Button>
            <Button loading={loading} type="submit" variant={buttonVariants.accent}>
              Сохранить
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}
