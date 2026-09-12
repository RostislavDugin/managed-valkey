import { useEffect, useRef, useState } from 'react';
import { AlertTriangle, Copy, KeyRound } from 'lucide-react';
import { Alert, Button, CopyButton, Group, Modal, Stack, Text, TextInput } from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { ApiError } from '@/shared/api';
import { buttonVariants } from '@/shared/config';
import { createUuidV7 } from '@/shared/lib';
import { rotateValkeyPassword } from '../api/valkey-api';
import type { ValkeyInstance } from '../model/valkey';
import { generateValkeyPassword, type ValkeyCredentials } from '../model/valkey-credentials';
import { getRequestErrorMessage, shouldReuseSubmission } from '../model/valkey-form';
import { useValkeySection } from './ValkeyLayout';
import styles from './ValkeyPage.module.css';

interface ValkeyPasswordModalProps {
  credentials: ValkeyCredentials;
  instance: ValkeyInstance;
  rotationOpened: boolean;
  onClose: () => void;
  onCredentialsChange: (credentials: ValkeyCredentials) => void;
  onCredentialsReload: () => void;
  onInstanceRefresh: () => void;
}

export function ValkeyPasswordModal({
  credentials,
  instance,
  rotationOpened,
  onClose,
  onCredentialsChange,
  onCredentialsReload,
  onInstanceRefresh,
}: ValkeyPasswordModalProps) {
  const { clearEphemeralPassword, ephemeralPassword, setEphemeralPassword } = useValkeySection();
  const [loading, setLoading] = useState(false);
  const attemptRef = useRef(0);
  const requestControllerRef = useRef<AbortController | null>(null);
  const pendingRef = useRef<{
    expectedPasswordVersion: number;
    idempotencyKey: string;
    password: string;
  } | null>(null);

  const revealed = ephemeralPassword?.instanceId === instance.id ? ephemeralPassword : null;
  const opened = rotationOpened || revealed !== null;

  const close = () => {
    attemptRef.current += 1;
    requestControllerRef.current?.abort();
    requestControllerRef.current = null;
    clearEphemeralPassword();
    pendingRef.current = null;
    setLoading(false);
    onClose();
  };

  useEffect(() => {
    const hide = () => {
      attemptRef.current += 1;
      requestControllerRef.current?.abort();
      requestControllerRef.current = null;
      clearEphemeralPassword();
      pendingRef.current = null;
      setLoading(false);
      onClose();
    };

    window.addEventListener('pagehide', hide);
    return () => {
      window.removeEventListener('pagehide', hide);
      attemptRef.current += 1;
      requestControllerRef.current?.abort();
      requestControllerRef.current = null;
      pendingRef.current = null;
    };
  }, [clearEphemeralPassword, onClose]);

  const rotate = async () => {
    const attempt = attemptRef.current + 1;
    attemptRef.current = attempt;
    setLoading(true);

    const submission =
      pendingRef.current?.expectedPasswordVersion === credentials.passwordVersion
        ? pendingRef.current
        : {
            expectedPasswordVersion: credentials.passwordVersion,
            idempotencyKey: createUuidV7(),
            password: generateValkeyPassword(),
          };
    pendingRef.current = submission;
    requestControllerRef.current?.abort();
    const controller = new AbortController();
    requestControllerRef.current = controller;

    try {
      const updated = await rotateValkeyPassword(
        instance.id,
        {
          password: submission.password,
          expectedPasswordVersion: submission.expectedPasswordVersion,
        },
        submission.idempotencyKey,
        controller.signal
      );

      if (attemptRef.current !== attempt || requestControllerRef.current !== controller) {
        return;
      }

      pendingRef.current = null;
      onCredentialsChange(updated);
      onInstanceRefresh();
      setEphemeralPassword({
        instanceId: instance.id,
        password: submission.password,
        source: 'rotation',
      });
    } catch (error) {
      if (
        attemptRef.current !== attempt ||
        requestControllerRef.current !== controller ||
        (error instanceof DOMException && error.name === 'AbortError')
      ) {
        return;
      }

      if (!shouldReuseSubmission(error)) {
        pendingRef.current = null;
      }

      if (error instanceof ApiError && error.status === 409) {
        onCredentialsReload();
        onInstanceRefresh();
      }

      notifications.show({
        color: 'red',
        message: getRequestErrorMessage(error),
        title: 'Не удалось изменить пароль',
      });
    } finally {
      if (attemptRef.current === attempt && requestControllerRef.current === controller) {
        requestControllerRef.current = null;
        setLoading(false);
      }
    }
  };

  return (
    <Modal
      closeButtonProps={{ 'aria-label': 'Закрыть окно' }}
      onClose={close}
      opened={opened}
      radius="h3_xl"
      title={revealed ? 'Сохраните пароль' : 'Изменить пароль'}
      centered
    >
      {revealed ? (
        <Stack gap="h3_md">
          <Alert
            color="yellow"
            icon={<AlertTriangle aria-hidden="true" size={16} strokeWidth={1.5} />}
            title="Пароль показывается один раз"
          >
            После закрытия окна посмотреть его снова не получится.
          </Alert>

          <TextInput
            className={styles.passwordInput}
            label={revealed.source === 'creation' ? 'Пароль базы' : 'Новый пароль'}
            value={revealed.password}
            readOnly
          />

          <Group justify="space-between">
            <CopyButton value={revealed.password}>
              {({ copied, copy }) => (
                <Button
                  leftSection={<Copy aria-hidden="true" size={16} strokeWidth={1.5} />}
                  onClick={copy}
                  variant={buttonVariants.secondary}
                >
                  {copied ? 'Пароль скопирован' : 'Скопировать пароль'}
                </Button>
              )}
            </CopyButton>

            <Button onClick={close} variant={buttonVariants.accent}>
              Закрыть
            </Button>
          </Group>
        </Stack>
      ) : (
        <Stack gap="h3_md">
          <Alert
            color="yellow"
            icon={<KeyRound aria-hidden="true" size={16} strokeWidth={1.5} />}
            title="Подключения будут разорваны"
          >
            Все подключения Valkey будут закрыты. Обновите пароль в приложениях.
          </Alert>

          {instance.status === 'degraded' && (
            <Text c="h3_text_2" size="h3_sm">
              Недоступный прежний процесс может задержать применение нового пароля.
            </Text>
          )}

          <Group justify="flex-end">
            <Button onClick={close} variant={buttonVariants.ghost}>
              Отмена
            </Button>
            <Button
              aria-label="Подтвердить изменение пароля"
              disabled={
                instance.isUpdating ||
                instance.isStale ||
                instance.status === 'deleting' ||
                (instance.status !== 'running' && instance.status !== 'degraded')
              }
              loading={loading}
              onClick={() => void rotate()}
              variant={buttonVariants.accent}
            >
              Изменить пароль
            </Button>
          </Group>
        </Stack>
      )}
    </Modal>
  );
}
