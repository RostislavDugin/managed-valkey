import dayjs from 'dayjs';
import 'dayjs/locale/ru';
import relativeTime from 'dayjs/plugin/relativeTime';
import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import { Alert, Button, Container, Group, Skeleton, Stack, Text } from '@mantine/core';
import { buttonVariants } from '@/shared/config';
import { getValkeyCredentials } from '../api/valkey-credentials';
import {
  formatDateTime,
  formatInstanceAddress,
  formatInstanceHost,
  formatRam,
  formatSize,
  formatVcpu,
  getTotalResources,
  MODE_LABELS,
} from '../model/valkey';
import type { ValkeyCredentials } from '../model/valkey-credentials';
import { getRequestErrorMessage } from '../model/valkey-form';
import { ConnectionExamples } from './ConnectionExamples';
import { CopyAction } from './CopyAction';
import { ResizeInstanceModal } from './ResizeInstanceModal';
import { useValkeyInstance } from './ValkeyInstanceLayout';
import { ValkeyPasswordModal } from './ValkeyPasswordModal';
import styles from './ValkeyPage.module.css';

dayjs.extend(relativeTime);
dayjs.locale('ru');

function PropertyRow({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div className={styles.propertyRow}>
      <Text size="h3_sm">{label}</Text>
      {value}
    </div>
  );
}

export function ValkeyInstancePage() {
  const { applyUpdate, domain, instance, instances, session } = useValkeyInstance();
  const [resizeOpened, setResizeOpened] = useState(false);
  const [passwordOpened, setPasswordOpened] = useState(false);
  const [credentials, setCredentials] = useState<ValkeyCredentials | null>(null);
  const [credentialsError, setCredentialsError] = useState<string | null>(null);
  const credentialsRequestRef = useRef(0);
  const closePasswordModal = useCallback(() => setPasswordOpened(false), []);

  const loadCredentials = useCallback(
    async (showLoading = true) => {
      const requestId = credentialsRequestRef.current + 1;
      credentialsRequestRef.current = requestId;

      if (showLoading) {
        setCredentials(null);
      }
      setCredentialsError(null);

      try {
        const loaded = await getValkeyCredentials(session.userId, instance.id);
        if (credentialsRequestRef.current === requestId) {
          setCredentials(loaded);
        }
      } catch (error) {
        if (credentialsRequestRef.current === requestId) {
          setCredentialsError(getRequestErrorMessage(error));
        }
      }
    },
    [instance.id, session.userId]
  );

  useEffect(() => {
    void loadCredentials();
    return () => {
      credentialsRequestRef.current += 1;
    };
  }, [loadCredentials]);

  useEffect(() => {
    if (!credentials || credentials.appliedPasswordVersion === credentials.passwordVersion) {
      return;
    }

    const timeout = window.setTimeout(() => void loadCredentials(false), 500);
    return () => window.clearTimeout(timeout);
  }, [credentials, loadCredentials]);

  const total = getTotalResources(instance, instance.mode);
  const host = domain ? formatInstanceHost(instance.slug, domain.domain) : '<HOST>';
  const port = domain?.port ?? '<PORT>';
  const address = domain
    ? formatInstanceAddress(instance.slug, domain.domain, domain.port)
    : instance.slug;

  return (
    <Container className={`${styles.page} ${styles.instanceContent}`} fluid>
      <Stack gap="h3_lg">
        <ConnectionExamples host={host} port={port} />

        <div className={styles.properties}>
          <PropertyRow
            label="Адрес"
            value={
              <Group gap="h3_xs" wrap="nowrap">
                <Text className={styles.monoValue} c="h3_text_2" size="h3_sm">
                  {address}
                </Text>
                <CopyAction label="Скопировать адрес" value={address} />
              </Group>
            }
          />
          {credentialsError ? (
            <Alert
              className={styles.credentialsError}
              color="red"
              title="Не удалось загрузить пароль"
            >
              <Group align="center" justify="space-between">
                <Text size="h3_sm">{credentialsError}</Text>
                <Button
                  onClick={() => void loadCredentials()}
                  size="compact-sm"
                  variant={buttonVariants.secondary}
                >
                  Повторить
                </Button>
              </Group>
            </Alert>
          ) : credentials ? (
            <>
              <PropertyRow
                label="Пользователь"
                value={
                  <Text c="h3_text_2" size="h3_sm">
                    {credentials.username}
                  </Text>
                }
              />
              <PropertyRow
                label="Пароль"
                value={
                  <Stack align="flex-end" gap={2}>
                    <Group gap="h3_xs" wrap="wrap">
                      <Text c="h3_text_2" size="h3_sm">
                        {credentials.passwordHint}
                      </Text>
                      {credentials.appliedPasswordVersion !== credentials.passwordVersion && (
                        <Text c="h3_text_2" size="h3_xs">
                          Применяется
                        </Text>
                      )}
                      <Text
                        aria-label="Сменить пароль"
                        className={`${styles.inlineAction} ${styles.touchTarget}`}
                        component="button"
                        disabled={
                          credentials.appliedPasswordVersion !== credentials.passwordVersion
                        }
                        onClick={() => setPasswordOpened(true)}
                        size="h3_sm"
                      >
                        (сменить)
                      </Text>
                    </Group>
                    {credentials.appliedPasswordVersion !== credentials.passwordVersion && (
                      <Text c="h3_text_2" size="h3_xs">
                        Дождитесь завершения текущей операции.
                      </Text>
                    )}
                  </Stack>
                }
              />
            </>
          ) : (
            <PropertyRow label="Учётные данные" value={<Skeleton height={20} width={160} />} />
          )}
          <PropertyRow
            label="Режим"
            value={
              <Text c="h3_text_2" size="h3_sm">
                {MODE_LABELS[instance.mode]}
              </Text>
            }
          />
          <PropertyRow
            label="Текущий тариф"
            value={
              <Group gap="h3_xs" wrap="nowrap">
                <Text c="h3_text_2" size="h3_sm">
                  {formatSize(instance)}
                </Text>
                <Text
                  aria-label="Изменить тариф"
                  className={`${styles.inlineAction} ${styles.touchTarget}`}
                  component="button"
                  onClick={() => setResizeOpened(true)}
                  size="h3_sm"
                >
                  (изменить)
                </Text>
              </Group>
            }
          />
          <PropertyRow
            label="Суммарные ресурсы"
            value={
              <Text c="h3_text_2" size="h3_sm">
                {formatVcpu(total.vcpu)} / {formatRam(total.ramGb)}
              </Text>
            }
          />
          <PropertyRow
            label="Создана"
            value={
              <Text c="h3_text_2" size="h3_sm">
                {formatDateTime(instance.createdAt)} ({dayjs(instance.createdAt).fromNow()})
              </Text>
            }
          />
        </div>
      </Stack>

      <ResizeInstanceModal
        author={session}
        instance={resizeOpened ? instance : null}
        instances={instances}
        onClose={() => setResizeOpened(false)}
        onResized={applyUpdate}
      />

      {credentials && (
        <ValkeyPasswordModal
          credentials={credentials}
          instance={instance}
          onClose={closePasswordModal}
          onCredentialsChange={setCredentials}
          onCredentialsReload={() => void loadCredentials()}
          rotationOpened={passwordOpened}
        />
      )}
    </Container>
  );
}
