import { useEffect, useRef, useState } from 'react';
import { Button, Group, Modal, NumberInput, Stack, Switch, TextInput } from '@mantine/core';
import { useForm } from '@mantine/form';
import { notifications } from '@mantine/notifications';
import { ApiError } from '@/shared/api';
import { buttonVariants } from '@/shared/config';
import { patchInstance } from '../api/valkey-api';
import { validateInstanceName, type ValkeyInstance } from '../model/valkey';
import { getRequestErrorMessage } from '../model/valkey-form';

interface RenameInstanceModalProps {
  instance: ValkeyInstance | null;
  onClose: () => void;
  onRefresh: () => void;
  onRenamed: (instance: ValkeyInstance) => void;
}

export function RenameInstanceModal({
  instance,
  onClose,
  onRefresh,
  onRenamed,
}: RenameInstanceModalProps) {
  const [loading, setLoading] = useState(false);
  const requestControllerRef = useRef<AbortController | null>(null);
  const initializedInstanceRef = useRef<string | null>(null);
  const openedInstanceId = instance?.id ?? null;
  const openedName = instance?.name ?? '';
  const openedMaintenanceDow = instance?.maintenance?.dow ?? 0;
  const openedMaintenanceHourUtc = instance?.maintenance?.hourUtc ?? 0;
  const openedMaintenanceDurationMin = instance?.maintenance?.durationMin ?? 30;
  const openedMaintenanceEnabled = instance?.maintenance !== null && instance !== null;

  const form = useForm({
    mode: 'controlled',
    initialValues: {
      name: '',
      maintenanceEnabled: false,
      maintenanceDow: 0,
      maintenanceHourUtc: 0,
      maintenanceDurationMin: 30,
    },
    validateInputOnBlur: true,
    validate: {
      name: (value) => validateInstanceName(value.trim()),
      maintenanceDow: (value, values) =>
        values.maintenanceEnabled && (!Number.isInteger(value) || value < 0 || value > 6)
          ? 'Укажите число от 0 до 6'
          : null,
      maintenanceHourUtc: (value, values) =>
        values.maintenanceEnabled && (!Number.isInteger(value) || value < 0 || value > 23)
          ? 'Укажите час от 0 до 23'
          : null,
      maintenanceDurationMin: (value, values) =>
        values.maintenanceEnabled && (!Number.isInteger(value) || value < 1 || value > 1440)
          ? 'Укажите длительность от 1 до 1440 минут'
          : null,
    },
  });

  const { setValues } = form;

  useEffect(() => {
    if (!openedInstanceId) {
      initializedInstanceRef.current = null;
      return;
    }
    if (initializedInstanceRef.current === openedInstanceId) {
      return;
    }

    initializedInstanceRef.current = openedInstanceId;
    setValues({
      name: openedName,
      maintenanceEnabled: openedMaintenanceEnabled,
      maintenanceDow: openedMaintenanceDow,
      maintenanceHourUtc: openedMaintenanceHourUtc,
      maintenanceDurationMin: openedMaintenanceDurationMin,
    });
  }, [
    openedInstanceId,
    openedMaintenanceDow,
    openedMaintenanceDurationMin,
    openedMaintenanceEnabled,
    openedMaintenanceHourUtc,
    openedName,
    setValues,
  ]);

  useEffect(() => {
    requestControllerRef.current?.abort();
    requestControllerRef.current = null;
    setLoading(false);

    return () => {
      requestControllerRef.current?.abort();
      requestControllerRef.current = null;
    };
  }, [openedInstanceId]);

  const submit = async (values: typeof form.values) => {
    if (!instance) {
      return;
    }

    setLoading(true);
    const controller = new AbortController();
    requestControllerRef.current = controller;

    try {
      const updated = await patchInstance(
        instance.id,
        {
          name: values.name.trim(),
          maintenance: values.maintenanceEnabled
            ? {
                dow: values.maintenanceDow,
                hourUtc: values.maintenanceHourUtc,
                durationMin: values.maintenanceDurationMin,
              }
            : null,
        },
        controller.signal
      );
      if (requestControllerRef.current !== controller) {
        return;
      }

      onRenamed(updated);
      notifications.show({
        message: `Настройки базы ${updated.name} сохранены.`,
        title: 'Настройки сохранены',
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
      if (
        error instanceof ApiError &&
        (error.code === 'CONFLICT' || error.code === 'VALIDATION_FAILED')
      ) {
        form.setFieldError('name', error.message);
      } else {
        notifications.show({
          color: 'red',
          message: getRequestErrorMessage(error),
          title: 'Не удалось переименовать базу',
        });
      }
    } finally {
      if (requestControllerRef.current === controller) {
        requestControllerRef.current = null;
        setLoading(false);
      }
    }
  };

  return (
    <Modal
      onClose={onClose}
      opened={instance !== null}
      radius="h3_xl"
      title="Настройки базы"
      centered
    >
      <form onSubmit={form.onSubmit((values) => void submit(values))}>
        <Stack gap="h3_md">
          <TextInput
            label="Имя базы"
            placeholder="valkey-1474"
            {...form.getInputProps('name')}
            data-autofocus
          />

          <Switch
            label="Задать окно обслуживания"
            {...form.getInputProps('maintenanceEnabled', { type: 'checkbox' })}
          />

          {form.values.maintenanceEnabled ? (
            <>
              <NumberInput
                label="День недели, 0–6"
                max={6}
                min={0}
                {...form.getInputProps('maintenanceDow')}
              />
              <NumberInput
                label="Час UTC, 0–23"
                max={23}
                min={0}
                {...form.getInputProps('maintenanceHourUtc')}
              />
              <NumberInput
                label="Длительность, минуты"
                max={1440}
                min={1}
                {...form.getInputProps('maintenanceDurationMin')}
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
