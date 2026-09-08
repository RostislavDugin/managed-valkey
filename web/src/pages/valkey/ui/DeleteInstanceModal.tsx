import { useRef, useState } from 'react';
import { Button, Code, Group, Modal, Stack, Text } from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { buttonVariants } from '@/shared/config';
import { deleteInstance, type MutationAuthor } from '../api/valkey-storage';
import type { ValkeyInstance } from '../model/valkey';
import { getRequestErrorMessage } from '../model/valkey-form';

interface DeleteInstanceModalProps {
  author: MutationAuthor;
  instance: ValkeyInstance | null;
  onClose: () => void;
  onDeleted: (instanceId: string) => void;
}

export function DeleteInstanceModal({
  author,
  instance,
  onClose,
  onDeleted,
}: DeleteInstanceModalProps) {
  const submittingRef = useRef(false);
  const [loading, setLoading] = useState(false);

  const confirm = async () => {
    if (!instance || submittingRef.current) {
      return;
    }

    submittingRef.current = true;
    setLoading(true);

    try {
      await deleteInstance(author, instance.id);

      onDeleted(instance.id);
      notifications.show({ message: `База ${instance.name} удалена.`, title: 'База удалена' });
      onClose();
    } catch (error) {
      notifications.show({
        color: 'red',
        message: getRequestErrorMessage(error),
        title: 'Не удалось удалить базу',
      });
    } finally {
      submittingRef.current = false;
      setLoading(false);
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
          Данные базы не восстанавливаются, освободившаяся квота вернётся сразу.
        </Text>

        <Group justify="flex-end">
          <Button onClick={onClose} type="button" variant={buttonVariants.ghost}>
            Отмена
          </Button>

          <Button
            color="var(--mantine-color-error)"
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
