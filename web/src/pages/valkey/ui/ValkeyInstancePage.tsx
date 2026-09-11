import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import { Alert, Button, Container, Group, Skeleton, Stack, Text } from '@mantine/core';
import { buttonVariants } from '@/shared/config';
import { formatDateTime, formatRelativeTime } from '@/shared/lib';
import { getValkeyCredentials } from '../api/valkey-api';
import { formatRam, formatSize, formatVcpu, getTotalResources, MODE_LABELS } from '../model/valkey';
import type { ValkeyCredentials } from '../model/valkey-credentials';
import { getRequestErrorMessage } from '../model/valkey-form';
import { ConnectionExamples } from './ConnectionExamples';
import { CopyAction } from './CopyAction';
import { ResizeInstanceModal } from './ResizeInstanceModal';
import { useValkeyInstance } from './ValkeyInstanceLayout';
import { ValkeyPasswordModal } from './ValkeyPasswordModal';
import { WhitelistModal } from './WhitelistModal';
import styles from './ValkeyPage.module.css';

function PropertyRow({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div className={styles.propertyRow}>
      <Text size="h3_sm">{label}</Text>
      {value}
    </div>
  );
}

export function ValkeyInstancePage() {
  const { applyUpdate, catalog, instance, quota, refresh } = useValkeyInstance();
  const [resizeOpened, setResizeOpened] = useState(false);
  const [passwordOpened, setPasswordOpened] = useState(false);
  const [whitelistOpened, setWhitelistOpened] = useState(false);
  const [credentials, setCredentials] = useState<ValkeyCredentials | null>(null);
  const [credentialsError, setCredentialsError] = useState<string | null>(null);
  const credentialsRequestRef = useRef(0);
  const credentialsControllerRef = useRef<AbortController | null>(null);
  const credentialsStartedAtRef = useRef(0);
  const closePasswordModal = useCallback(() => setPasswordOpened(false), []);

  const loadCredentials = useCallback(
    async (showLoading = true, signal?: AbortSignal) => {
      const requestId = credentialsRequestRef.current + 1;
      credentialsRequestRef.current = requestId;

      if (showLoading) {
        setCredentials(null);
      }
      setCredentialsError(null);

      try {
        const loaded = await getValkeyCredentials(instance.id, signal);
        if (!signal?.aborted && credentialsRequestRef.current === requestId) {
          setCredentials(loaded);
        }
      } catch (error) {
        if (
          credentialsRequestRef.current === requestId &&
          !(error instanceof DOMException && error.name === 'AbortError')
        ) {
          setCredentialsError(getRequestErrorMessage(error));
        }
      }
    },
    [instance.id]
  );

  const requestCredentials = useCallback(
    async (showLoading = true) => {
      credentialsControllerRef.current?.abort();
      const controller = new AbortController();
      credentialsControllerRef.current = controller;
      credentialsStartedAtRef.current = Date.now();
      await loadCredentials(showLoading, controller.signal).finally(() => {
        if (credentialsControllerRef.current === controller) {
          credentialsControllerRef.current = null;
        }
      });
    },
    [loadCredentials]
  );

  useEffect(() => {
    void requestCredentials();
    return () => {
      credentialsRequestRef.current += 1;
      credentialsControllerRef.current?.abort();
      credentialsControllerRef.current = null;
    };
  }, [requestCredentials]);

  useEffect(() => {
    if (!credentials || credentials.appliedPasswordVersion === credentials.passwordVersion) {
      return;
    }

    let stopped = false;
    let timeout: number | undefined;
    const poll = async () => {
      await requestCredentials(false);
      if (!stopped) {
        schedule();
      }
    };
    const schedule = () => {
      const elapsed = Date.now() - credentialsStartedAtRef.current;
      timeout = window.setTimeout(() => void poll(), Math.max(0, 5_000 - elapsed));
    };

    schedule();
    return () => {
      stopped = true;
      window.clearTimeout(timeout);
    };
  }, [credentials, requestCredentials]);

  const total = getTotalResources(instance, instance.mode);
  const host = credentials?.host ?? instance.host;
  const hostRo = credentials?.hostRo ?? instance.hostRo;
  const port = credentials?.port ?? instance.port;
  const address = `${host}:${port}`;
  const readOnlyAddress = hostRo ? `${hostRo}:${port}` : null;
  const canConfigure =
    !instance.isUpdating &&
    !instance.isStale &&
    instance.status !== 'deleting' &&
    (instance.status === 'running' || instance.status === 'degraded');

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
          {readOnlyAddress && (
            <PropertyRow
              label="Адрес только для чтения"
              value={
                <Group gap="h3_xs" wrap="nowrap">
                  <Text className={styles.monoValue} c="h3_text_2" size="h3_sm">
                    {readOnlyAddress}
                  </Text>
                  <CopyAction label="Скопировать адрес только для чтения" value={readOnlyAddress} />
                </Group>
              }
            />
          )}
          {credentialsError ? (
            <Alert
              className={styles.credentialsError}
              color="red"
              title="Не удалось загрузить пароль"
            >
              <Group align="center" justify="space-between">
                <Text size="h3_sm">{credentialsError}</Text>
                <Button
                  onClick={() => void requestCredentials()}
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
                  <Group gap="h3_xs" wrap="wrap">
                    <Text c="h3_text_2" size="h3_sm">
                      {credentials.passwordHint}
                    </Text>
                    <Text
                      aria-label="Сменить пароль"
                      className={`${styles.inlineAction} ${styles.touchTarget}`}
                      component="button"
                      disabled={
                        credentials.appliedPasswordVersion !== credentials.passwordVersion ||
                        !canConfigure
                      }
                      onClick={() => setPasswordOpened(true)}
                      size="h3_sm"
                    >
                      (сменить)
                    </Text>
                  </Group>
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
                  disabled={!canConfigure}
                  onClick={() => setResizeOpened(true)}
                  size="h3_sm"
                >
                  (изменить)
                </Text>
              </Group>
            }
          />
          <PropertyRow
            label="Применённая конфигурация"
            value={
              <Text c="h3_text_2" size="h3_sm">
                {instance.appliedVcpu > 0 && instance.appliedRamGb > 0
                  ? `${formatVcpu(instance.appliedVcpu)} / ${formatRam(instance.appliedRamGb)}`
                  : 'Ещё не применена'}
              </Text>
            }
          />
          <PropertyRow
            label="Доступ по IP"
            value={
              <Group gap="h3_xs" wrap="wrap">
                <Text c="h3_text_2" size="h3_sm">
                  {instance.isWhitelistEnabled
                    ? instance.whitelistCidrs.length === 0
                      ? 'Все подключения запрещены'
                      : instance.whitelistCidrs.join(', ')
                    : 'Без ограничений'}
                </Text>
                <Text
                  aria-label="Изменить доступ по IP"
                  className={`${styles.inlineAction} ${styles.touchTarget}`}
                  component="button"
                  disabled={!canConfigure}
                  onClick={() => setWhitelistOpened(true)}
                  size="h3_sm"
                >
                  (изменить)
                </Text>
              </Group>
            }
          />
          <PropertyRow
            label="Окно обслуживания"
            value={
              <Text c="h3_text_2" size="h3_sm">
                {instance.maintenance
                  ? `День ${instance.maintenance.dow}, ${instance.maintenance.hourUtc}:00 UTC, ${instance.maintenance.durationMin} мин.`
                  : 'Не задано'}
              </Text>
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
                {formatDateTime(instance.createdAt)} ({formatRelativeTime(instance.createdAt)})
              </Text>
            }
          />
        </div>
      </Stack>

      <ResizeInstanceModal
        catalog={catalog}
        instance={resizeOpened ? instance : null}
        onClose={() => setResizeOpened(false)}
        onRefresh={refresh}
        onResized={applyUpdate}
        quota={quota}
      />

      <WhitelistModal
        instance={whitelistOpened ? instance : null}
        onClose={() => setWhitelistOpened(false)}
        onRefresh={refresh}
        onUpdated={applyUpdate}
      />

      {credentials && (
        <ValkeyPasswordModal
          credentials={credentials}
          instance={instance}
          onClose={closePasswordModal}
          onCredentialsChange={setCredentials}
          onCredentialsReload={() => void requestCredentials()}
          onInstanceRefresh={refresh}
          rotationOpened={passwordOpened}
        />
      )}
    </Container>
  );
}
