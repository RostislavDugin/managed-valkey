import { useEffect, useState } from 'react';
import { Button, Group, Modal, Stack, TextInput } from '@mantine/core';
import { useForm } from '@mantine/form';
import { notifications } from '@mantine/notifications';
import { ApiError } from '@/shared/api';
import { buttonVariants } from '@/shared/config';
import { renameInstance, type MutationAuthor } from '../api/valkey-storage';
import { validateInstanceName, type ValkeyInstance } from '../model/valkey';
import { getRequestErrorMessage } from '../model/valkey-form';

interface RenameInstanceModalProps {
  instance: ValkeyInstance | null;
  author: MutationAuthor;
  onClose: () => void;
  onRenamed: (instance: ValkeyInstance) => void;
}

/** Короткой форме отдельный адрес не нужен: web/DESIGN.md, «Обратная связь и состояния». */
export function RenameInstanceModal({
  author,
  instance,
  onClose,
  onRenamed,
}: RenameInstanceModalProps) {
  const [loading, setLoading] = useState(false);

  const form = useForm<{ name: string }>({
    mode: 'controlled',
    initialValues: { name: '' },
    validateInputOnBlur: true,
    validate: { name: (value) => validateInstanceName(value.trim()) },
  });

  const { setValues } = form;

  useEffect(() => {
    if (instance) {
      setValues({ name: instance.name });
    }
  }, [instance, setValues]);

  const submit = async (values: { name: string }) => {
    if (!instance) {
      return;
    }

    setLoading(true);

    try {
      const updated = await renameInstance(author, instance.id, { name: values.name.trim() });

      onRenamed(updated);
      notifications.show({ message: `База называется ${updated.name}.`, title: 'Имя изменено' });
      onClose();
    } catch (error) {
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
      setLoading(false);
    }
  };

  return (
    <Modal
      onClose={onClose}
      opened={instance !== null}
      radius="h3_xl"
      title="Переименовать базу"
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
              Сохранить имя
            </Button>
          </Group>
        </Stack>
      </form>
    </Modal>
  );
}
