import { useEffect, useRef, useState } from 'react';
import { Button, Group, Modal, Stack, TextInput } from '@mantine/core';
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

  const form = useForm({
    mode: 'controlled',
    initialValues: {
      name: '',
    },
    validateInputOnBlur: true,
    validate: {
      name: (value) => validateInstanceName(value.trim()),
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
    });
  }, [openedInstanceId, openedName, setValues]);

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
        { name: values.name.trim() },
        controller.signal
      );
      if (requestControllerRef.current !== controller) {
        return;
      }

      onRenamed(updated);
      notifications.show({
        message: `База переименована в ${updated.name}.`,
        title: 'Имя изменено',
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
      title="Изменить имя"
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
