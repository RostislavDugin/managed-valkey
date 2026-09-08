import { useEffect, useRef, useState } from 'react';
import { AlertTriangle, Copy, KeyRound } from 'lucide-react';
import { Alert, Button, CopyButton, Group, Modal, Stack, Text, TextInput } from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { ApiError } from '@/shared/api';
import { buttonVariants } from '@/shared/config';
import { rotateValkeyPassword } from '../api/valkey-credentials';
import type { ValkeyInstance } from '../model/valkey';
import { generateValkeyPassword, type ValkeyCredentials } from '../model/valkey-credentials';
import { getRequestErrorMessage } from '../model/valkey-form';
import { useValkeySection } from './ValkeyLayout';
import styles from './ValkeyPage.module.css';

interface ValkeyPasswordModalProps {
  credentials: ValkeyCredentials;
  instance: ValkeyInstance;
  rotationOpened: boolean;
  onClose: () => void;
  onCredentialsChange: (credentials: ValkeyCredentials) => void;
  onCredentialsReload: () => void;
}

export function ValkeyPasswordModal({
  credentials,
  instance,
  rotationOpened,
  onClose,
  onCredentialsChange,
  onCredentialsReload,
}: ValkeyPasswordModalProps) {
  const { clearEphemeralPassword, ephemeralPassword, session, setEphemeralPassword } =
    useValkeySection();
  const [loading, setLoading] = useState(false);
  const attemptRef = useRef(0);

  const revealed = ephemeralPassword?.instanceId === instance.id ? ephemeralPassword : null;
  const opened = rotationOpened || revealed !== null;

  const close = () => {
    attemptRef.current += 1;
    clearEphemeralPassword();
    setLoading(false);
    onClose();
  };

  useEffect(() => {
    const hide = () => {
      attemptRef.current += 1;
      clearEphemeralPassword();
      setLoading(false);
      onClose();
    };

    window.addEventListener('pagehide', hide);
    return () => {
      window.removeEventListener('pagehide', hide);
      attemptRef.current += 1;
    };
  }, [clearEphemeralPassword, onClose]);

  const rotate = async () => {
    const attempt = attemptRef.current + 1;
    attemptRef.current = attempt;
    setLoading(true);

    const password = generateValkeyPassword();

    try {
      const updated = await rotateValkeyPassword(session, instance.id, {
        password,
        expectedPasswordVersion: credentials.passwordVersion,
      });

      if (attemptRef.current !== attempt) {
        return;
      }

      onCredentialsChange(updated);
      setEphemeralPassword({ instanceId: instance.id, password, source: 'rotation' });
    } catch (error) {
      if (attemptRef.current !== attempt) {
        return;
      }

      if (error instanceof ApiError && error.code === 'CONFLICT') {
        onCredentialsReload();
      }

      notifications.show({
        color: 'red',
        message: getRequestErrorMessage(error),
        title: 'Не удалось сменить пароль',
      });
    } finally {
      if (attemptRef.current === attempt) {
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
      title={revealed ? 'Сохраните пароль' : 'Сменить пароль'}
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

          {revealed.source === 'rotation' && (
            <Text c="h3_text_2" size="h3_sm">
              Состояние: Применяется
            </Text>
          )}

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
              aria-label="Подтвердить смену пароля"
              loading={loading}
              onClick={() => void rotate()}
              variant={buttonVariants.accent}
            >
              Сменить пароль
            </Button>
          </Group>
        </Stack>
      )}
    </Modal>
  );
}
