import { useEffect, useMemo, useRef, useState } from 'react';
import { Button, Group, Modal, Stack, Text } from '@mantine/core';
import { useForm } from '@mantine/form';
import { notifications } from '@mantine/notifications';
import { ApiError } from '@/shared/api';
import { buttonVariants } from '@/shared/config';
import { patchInstance } from '../api/valkey-api';
import type { ValkeyInstance } from '../model/valkey';
import { getRequestErrorMessage } from '../model/valkey-form';
import {
  getDefaultLocalMaintenance,
  localMaintenanceToUtc,
  MAINTENANCE_HINT,
  MAINTENANCE_WEEKDAYS,
  utcMaintenanceToLocal,
  validateLocalMaintenanceTime,
} from '../model/valkey-maintenance';
import { MaintenanceWindowFields } from './MaintenanceWindowFields';

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
  const timezoneOffsetMinutesRef = useRef(new Date().getTimezoneOffset());
  const openedInstanceId = instance?.id ?? null;
  const timezoneOffsetMinutes = timezoneOffsetMinutesRef.current;
  const defaultMaintenance = useMemo(
    () => getDefaultLocalMaintenance(timezoneOffsetMinutes),
    [timezoneOffsetMinutes]
  );
  const form = useForm<{ dow: string; time: string; durationMin: number }>({
    mode: 'controlled',
    initialValues: {
      dow: String(defaultMaintenance.dow),
      time: defaultMaintenance.time,
      durationMin: defaultMaintenance.durationMin,
    },
    validateInputOnBlur: true,
    validate: {
      dow: (value) =>
        MAINTENANCE_WEEKDAYS.some((weekday) => weekday.value === value)
          ? null
          : 'Выберите день недели',
      time: (value) => validateLocalMaintenanceTime(value, timezoneOffsetMinutes),
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
    const maintenance = instance.maintenance
      ? utcMaintenanceToLocal(instance.maintenance, timezoneOffsetMinutes)
      : defaultMaintenance;
    setValues({
      dow: String(maintenance.dow),
      time: maintenance.time,
      durationMin: maintenance.durationMin,
    });
  }, [defaultMaintenance, instance, setValues, timezoneOffsetMinutes]);

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
          maintenance: localMaintenanceToUtc(
            { dow: Number(values.dow), time: values.time, durationMin: Number(values.durationMin) },
            timezoneOffsetMinutes
          ),
        },
        controller.signal
      );
      if (requestControllerRef.current !== controller) {
        return;
      }

      onUpdated(updated);
      notifications.show({
        message: 'Новое окно сохранено.',
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
          <Text c="h3_text_2" size="h3_sm">
            {MAINTENANCE_HINT}
          </Text>

          <MaintenanceWindowFields
            day={form.values.dow}
            dayError={form.errors.dow}
            onDayChange={(value) => form.setFieldValue('dow', value ?? '')}
            onTimeChange={(value) => form.setFieldValue('time', value ?? '')}
            time={form.values.time}
            timeError={form.errors.time}
            timezoneOffsetMinutes={timezoneOffsetMinutes}
          />

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
