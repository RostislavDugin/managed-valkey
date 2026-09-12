import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react';
import { Info } from 'lucide-react';
import { Alert, Button, Container, Group, Skeleton, Stack, Text, Tooltip } from '@mantine/core';
import { buttonVariants } from '@/shared/config';
import { formatDateTime, formatRelativeTime } from '@/shared/lib';
import { getValkeyCredentials } from '../api/valkey-api';
import { formatSize, MODE_LABELS } from '../model/valkey';
import { formatValkeyAddress, getValkeyConnectionScheme } from '../model/valkey-connection';
import type { ValkeyCredentials } from '../model/valkey-credentials';
import { getRequestErrorMessage } from '../model/valkey-form';
import { formatLocalMaintenance, MAINTENANCE_HINT } from '../model/valkey-maintenance';
import { ConnectionExamples } from './ConnectionExamples';
import { CopyAction } from './CopyAction';
import { MaintenanceInstanceModal } from './MaintenanceInstanceModal';
import { ResizeInstanceModal } from './ResizeInstanceModal';
import { useValkeyInstance } from './ValkeyInstanceLayout';
import { ValkeyPasswordModal } from './ValkeyPasswordModal';
import { WhitelistModal } from './WhitelistModal';
import styles from './ValkeyPage.module.css';

const CONNECTION_HINT =
  'В отказоустойчивом режиме адрес для записи и чтения ведёт на primary. Адрес только для чтения ведёт на реплики. Если primary недоступен, одна из реплик принимает его роль. В режиме с одной нодой оба адреса ведут на один инстанс.';
function PropertyRow({ hint, label, value }: { hint?: string; label: string; value: ReactNode }) {
  return (
    <div aria-label={label} className={styles.propertyRow} role="group">
      <Group align="center" gap={4} wrap="nowrap">
        <Text size="h3_sm">{label}</Text>
        {hint ? (
          <Tooltip label={hint} multiline w={280} withArrow>
            <Info
              aria-label={`Подсказка: ${label}`}
              className={styles.formHint}
              role="img"
              size={16}
              strokeWidth={1.5}
              tabIndex={0}
            />
          </Tooltip>
        ) : null}
      </Group>
      {value}
    </div>
  );
}

export function ValkeyInstancePage() {
  const { applyUpdate, catalog, instance, refresh, session } = useValkeyInstance();
  const [resizeOpened, setResizeOpened] = useState(false);
  const [passwordOpened, setPasswordOpened] = useState(false);
  const [whitelistOpened, setWhitelistOpened] = useState(false);
  const [maintenanceOpened, setMaintenanceOpened] = useState(false);
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

  const timezoneOffsetMinutes = new Date().getTimezoneOffset();
  const host = credentials?.host ?? instance.host;
  const hostRo = credentials?.hostRo ?? instance.hostRo;
  const port = credentials?.port ?? instance.port;
  const connectionScheme = getValkeyConnectionScheme(window.location.protocol);
  const address = formatValkeyAddress(host, port, connectionScheme);
  const readOnlyAddress = formatValkeyAddress(hostRo, port, connectionScheme);
  const canConfigure =
    !instance.isUpdating &&
    !instance.isStale &&
    instance.status !== 'deleting' &&
    (instance.status === 'running' || instance.status === 'degraded');

  return (
    <Container className={`${styles.page} ${styles.instanceContent}`} fluid>
      <Stack gap="h3_lg">
        <ConnectionExamples
          host={host}
          hostRo={hostRo}
          port={port}
          secure={connectionScheme === 'rediss'}
        />

        <div className={styles.properties}>
          <PropertyRow
            hint={CONNECTION_HINT}
            label="Адрес для записи и чтения"
            value={
              <Group className={styles.addressValue} gap="h3_xs" wrap="nowrap">
                <Text className={styles.monoValue} c="h3_text_2" size="h3_sm">
                  {address}
                </Text>
                <CopyAction label="Скопировать адрес для записи и чтения" value={address} />
              </Group>
            }
          />
          <PropertyRow
            hint={CONNECTION_HINT}
            label="Адрес только для чтения"
            value={
              <Group className={styles.addressValue} gap="h3_xs" wrap="nowrap">
                <Text className={styles.monoValue} c="h3_text_2" size="h3_sm">
                  {readOnlyAddress}
                </Text>
                <CopyAction label="Скопировать адрес для чтения" value={readOnlyAddress} />
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
                      aria-label="Изменить пароль"
                      className={`${styles.inlineAction} ${styles.touchTarget}`}
                      component="button"
                      disabled={
                        credentials.appliedPasswordVersion !== credentials.passwordVersion ||
                        !canConfigure
                      }
                      onClick={() => setPasswordOpened(true)}
                      size="h3_sm"
                    >
                      (изменить)
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
            label="Текущая конфигурация"
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
            label="Белый список"
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
                  aria-label="Изменить белый список"
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
            hint={MAINTENANCE_HINT}
            label="Окно обслуживания"
            value={
              <Group gap="h3_xs" wrap="wrap">
                <Text c="h3_text_2" size="h3_sm">
                  {instance.maintenance
                    ? formatLocalMaintenance(instance.maintenance, timezoneOffsetMinutes)
                    : 'Не задано'}
                </Text>
                <Text
                  aria-label="Изменить окно обслуживания"
                  className={`${styles.inlineAction} ${styles.touchTarget}`}
                  component="button"
                  disabled={instance.status === 'deleting'}
                  onClick={() => setMaintenanceOpened(true)}
                  size="h3_sm"
                >
                  (изменить)
                </Text>
              </Group>
            }
          />
          <PropertyRow
            label="Создано"
            value={
              <Text c="h3_text_2" size="h3_sm">
                {formatDateTime(instance.createdAt)} ({formatRelativeTime(instance.createdAt)})
              </Text>
            }
          />
        </div>
      </Stack>

      <ResizeInstanceModal
        accountId={session.userId}
        catalog={catalog}
        instance={resizeOpened ? instance : null}
        onClose={() => setResizeOpened(false)}
        onRefresh={refresh}
        onResized={applyUpdate}
      />

      <WhitelistModal
        instance={whitelistOpened ? instance : null}
        onClose={() => setWhitelistOpened(false)}
        onRefresh={refresh}
        onUpdated={applyUpdate}
      />

      <MaintenanceInstanceModal
        instance={maintenanceOpened ? instance : null}
        onClose={() => setMaintenanceOpened(false)}
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
