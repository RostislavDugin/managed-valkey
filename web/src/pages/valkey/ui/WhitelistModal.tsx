import { useEffect, useRef, useState } from 'react';
import { Button, Group, Modal, Stack, Switch, Textarea } from '@mantine/core';
import { useForm } from '@mantine/form';
import { notifications } from '@mantine/notifications';
import { ApiError } from '@/shared/api';
import { buttonVariants } from '@/shared/config';
import { updateWhitelist } from '../api/valkey-api';
import { parseWhitelistCidrs, validateWhitelistCidrs, type ValkeyInstance } from '../model/valkey';
import { getRequestErrorMessage } from '../model/valkey-form';

interface WhitelistModalProps {
  instance: ValkeyInstance | null;
  onClose: () => void;
  onRefresh: () => void;
  onUpdated: (instance: ValkeyInstance) => void;
}

export function WhitelistModal({ instance, onClose, onRefresh, onUpdated }: WhitelistModalProps) {
  const [loading, setLoading] = useState(false);
  const requestControllerRef = useRef<AbortController | null>(null);
  const initializedInstanceRef = useRef<string | null>(null);
  const openedInstanceId = instance?.id ?? null;
  const openedWhitelistEnabled = instance?.isWhitelistEnabled ?? false;
  const openedWhitelistCidrs = instance?.whitelistCidrs.join('\n') ?? '';
  const form = useForm({
    mode: 'controlled',
    initialValues: { enabled: false, cidrs: '' },
    validateInputOnBlur: true,
    validate: {
      cidrs: (value, values) => (values.enabled ? validateWhitelistCidrs(value) : null),
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
      enabled: openedWhitelistEnabled,
      cidrs: openedWhitelistCidrs,
    });
  }, [openedInstanceId, openedWhitelistCidrs, openedWhitelistEnabled, setValues]);

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
      const updated = await updateWhitelist(
        instance.id,
        {
          isWhitelistEnabled: values.enabled,
          whitelistCidrs: values.enabled ? parseWhitelistCidrs(values.cidrs) : [],
        },
        controller.signal
      );
      if (requestControllerRef.current !== controller) {
        return;
      }
      onUpdated(updated);
      notifications.show({ title: 'Белый список изменён', message: 'Новые правила приняты.' });
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
        title: 'Не удалось изменить белый список',
        message: getRequestErrorMessage(error),
      });
    } finally {
      if (requestControllerRef.current === controller) {
        requestControllerRef.current = null;
        setLoading(false);
      }
    }
  };

  return (
    <Modal onClose={onClose} opened radius="h3_xl" title="Белый список" centered>
      <form onSubmit={form.onSubmit((values) => void submit(values))}>
        <Stack gap="h3_md">
          <Switch
            label="Ограничить доступ по IP-адресам"
            {...form.getInputProps('enabled', { type: 'checkbox' })}
          />
          {form.values.enabled ? (
            <Textarea
              description="Пустой список запрещает все подключения"
              label="Разрешённые IPv4-адреса и CIDR"
              rows={5}
              {...form.getInputProps('cidrs')}
            />
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
