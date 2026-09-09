import { useEffect, useRef, useState } from 'react';
import { Button, Code, Group, Modal, Stack, Text, TextInput } from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { buttonVariants } from '@/shared/config';
import { deleteInstance } from '../api/valkey-api';
import type { ValkeyInstance } from '../model/valkey';
import { getRequestErrorMessage } from '../model/valkey-form';

interface DeleteInstanceModalProps {
  instance: ValkeyInstance | null;
  onClose: () => void;
  onDeleted: () => void;
}

export function DeleteInstanceModal({ instance, onClose, onDeleted }: DeleteInstanceModalProps) {
  const submittingRef = useRef(false);
  const requestControllerRef = useRef<AbortController | null>(null);
  const [loading, setLoading] = useState(false);
  const [confirmation, setConfirmation] = useState('');
  const openedInstanceId = instance?.id ?? null;

  useEffect(() => {
    requestControllerRef.current?.abort();
    requestControllerRef.current = null;
    submittingRef.current = false;
    setLoading(false);
    setConfirmation('');

    return () => {
      requestControllerRef.current?.abort();
      requestControllerRef.current = null;
      submittingRef.current = false;
    };
  }, [openedInstanceId]);

  const confirm = async () => {
    if (!instance || submittingRef.current) {
      return;
    }

    submittingRef.current = true;
    setLoading(true);
    const controller = new AbortController();
    requestControllerRef.current = controller;

    try {
      await deleteInstance(instance.id, controller.signal);
      if (requestControllerRef.current !== controller) {
        return;
      }

      onDeleted();
      onClose();
    } catch (error) {
      if (
        requestControllerRef.current !== controller ||
        (error instanceof DOMException && error.name === 'AbortError')
      ) {
        return;
      }
      notifications.show({
        color: 'red',
        message: getRequestErrorMessage(error),
        title: 'Не удалось удалить базу',
      });
    } finally {
      if (requestControllerRef.current === controller) {
        requestControllerRef.current = null;
        submittingRef.current = false;
        setLoading(false);
      }
    }
  };

  return (
    <Modal
      onClose={onClose}
      opened={instance !== null}
      radius="h3_xl"
      title={
        <Text fw="var(--h3-fw-bold)">
          Удалить базу <Code>{instance?.name}</Code>?
        </Text>
      }
      centered
    >
      <Stack gap="h3_md">
        <Text c="h3_text_2" size="h3_sm">
          Данные базы не восстанавливаются. Квота освободится после завершения удаления.
        </Text>

        <TextInput
          autoComplete="off"
          label={
            <>
              Введите <Code>{instance?.slug}</Code> для подтверждения
            </>
          }
          onChange={(event) => setConfirmation(event.currentTarget.value)}
          value={confirmation}
        />

        <Group justify="flex-end">
          <Button onClick={onClose} type="button" variant={buttonVariants.ghost}>
            Отмена
          </Button>

          <Button
            color="var(--mantine-color-error)"
            disabled={confirmation !== instance?.slug}
            loading={loading}
            onClick={() => void confirm()}
          >
            Удалить базу
          </Button>
        </Group>
      </Stack>
    </Modal>
  );
}
