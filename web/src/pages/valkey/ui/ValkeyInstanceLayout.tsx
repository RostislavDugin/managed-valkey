import { createContext, use, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { MoreHorizontal, Pencil, RotateCw, Trash2 } from 'lucide-react';
import { createPortal } from 'react-dom';
import { Link, Outlet, useLocation, useParams } from 'react-router';
import {
  ActionIcon,
  Alert,
  Button,
  Container,
  Group,
  Menu,
  Skeleton,
  Stack,
  Tabs,
  Text,
  Title,
  Tooltip,
} from '@mantine/core';
import { ApiError, type Session } from '@/shared/api';
import {
  buttonVariants,
  routes,
  valkeyInstancePath,
  type ValkeyInstanceTab,
} from '@/shared/config';
import { getInstance, getValkeyCatalog, getValkeyQuota } from '../api/valkey-api';
import type { ValkeyQuota } from '../model/quota';
import {
  STATUS_LABELS,
  type PricePeriod,
  type ValkeyCatalog,
  type ValkeyInstance,
} from '../model/valkey';
import { getRequestErrorMessage } from '../model/valkey-form';
import { CopyAction } from './CopyAction';
import { DeleteInstanceModal } from './DeleteInstanceModal';
import { PricePanel, PricePeriodTabs } from './PricePanel';
import { RenameInstanceModal } from './RenameInstanceModal';
import { ValkeyAside } from './ValkeyAside';
import { useValkeySection } from './ValkeyLayout';
import styles from './ValkeyPage.module.css';

interface ValkeyInstanceContextValue {
  session: Session;
  instance: ValkeyInstance;
  catalog: ValkeyCatalog;
  quota: ValkeyQuota;
  applyUpdate: (updated: ValkeyInstance) => void;
  refresh: () => void;
}

const ValkeyInstanceContext = createContext<ValkeyInstanceContextValue | null>(null);

export function useValkeyInstance() {
  const value = use(ValkeyInstanceContext);
  if (!value) {
    throw new Error('useValkeyInstance доступен только внутри ValkeyInstanceLayout');
  }
  return value;
}

const TABS: Array<{ label: string; tab?: ValkeyInstanceTab; value: string }> = [
  { value: 'management', label: 'Управление' },
  { value: 'monitoring', label: 'Мониторинг', tab: 'monitoring' },
  { value: 'audit-logs', label: 'Аудит', tab: 'audit-logs' },
];

function getActiveTab(pathname: string) {
  if (pathname.endsWith('/monitoring')) {
    return 'monitoring';
  }

  if (pathname.endsWith('/audit-logs')) {
    return 'audit-logs';
  }

  return 'management';
}

export function ValkeyInstanceLayout() {
  const { instanceId = '' } = useParams();
  const { headerSlot, session, setTrailingCrumb } = useValkeySection();
  const { pathname } = useLocation();

  const [instance, setInstance] = useState<ValkeyInstance | null>(null);
  const [catalog, setCatalog] = useState<ValkeyCatalog | null>(null);
  const [quota, setQuota] = useState<ValkeyQuota | null>(null);
  const [error, setError] = useState<Error | null>(null);
  const [period, setPeriod] = useState<PricePeriod>('month');
  const [deleteOpened, setDeleteOpened] = useState(false);
  const [renameOpened, setRenameOpened] = useState(false);
  const [revision, setRevision] = useState(0);
  const loadedIdentityRef = useRef('');
  const loadIdentity = `${session.userId}:${instanceId}`;

  const load = useCallback(
    async (signal: AbortSignal, showLoading: boolean) => {
      setError(null);
      if (showLoading) {
        setInstance(null);
      }

      try {
        const [loaded, loadedCatalog, loadedQuota] = await Promise.all([
          getInstance(instanceId, signal),
          getValkeyCatalog(signal),
          getValkeyQuota(signal),
        ]);
        if (signal.aborted) {
          return;
        }

        setInstance(loaded);
        setCatalog(loadedCatalog);
        setQuota(loadedQuota);
      } catch (loadError) {
        if (loadError instanceof DOMException && loadError.name === 'AbortError') {
          return;
        }
        setError(loadError instanceof Error ? loadError : new Error(String(loadError)));
      }
    },
    [instanceId, session.userId]
  );

  useEffect(() => {
    const controller = new AbortController();
    let timeout: ReturnType<typeof setTimeout> | undefined;
    let stopped = false;

    const poll = async (showLoading: boolean) => {
      const startedAt = Date.now();
      await load(controller.signal, showLoading);
      if (!stopped) {
        const elapsed = Date.now() - startedAt;
        timeout = setTimeout(() => void poll(false), Math.max(0, 5_000 - elapsed));
      }
    };

    const showLoading = loadedIdentityRef.current !== loadIdentity;
    loadedIdentityRef.current = loadIdentity;
    void poll(showLoading);
    return () => {
      stopped = true;
      clearTimeout(timeout);
      controller.abort();
    };
  }, [load, loadIdentity, revision]);

  useEffect(() => {
    setTrailingCrumb(instance?.name ?? null);
    return () => setTrailingCrumb(null);
  }, [instance?.name, setTrailingCrumb]);

  const applyUpdate = useCallback((updated: ValkeyInstance) => {
    setInstance(updated);
    setRevision((current) => current + 1);
  }, []);
  const refresh = useCallback(() => setRevision((current) => current + 1), []);

  const context = useMemo<ValkeyInstanceContextValue | null>(
    () =>
      instance && catalog && quota
        ? { session, instance, catalog, quota, applyUpdate, refresh }
        : null,
    [applyUpdate, catalog, instance, quota, refresh, session]
  );

  if (error instanceof ApiError && error.code === 'NOT_FOUND') {
    return (
      <Container className={styles.page} size="h3_page">
        <Stack align="flex-start" gap="h3_md">
          <Title order={1}>База не найдена</Title>
          <Text c="h3_text_2">Проверьте адрес или вернитесь к списку баз.</Text>

          <Button component={Link} to={routes.valkeyManagement} variant={buttonVariants.secondary}>
            К списку баз
          </Button>
        </Stack>
      </Container>
    );
  }

  if (error) {
    return (
      <Container className={styles.page} size="h3_page">
        <Alert color="red" title="Не удалось загрузить базу">
          <Stack align="flex-start" gap="h3_sm">
            <Text size="h3_sm">{getRequestErrorMessage(error)}</Text>
            <Button
              leftSection={<RotateCw aria-hidden="true" size={16} strokeWidth={1.5} />}
              onClick={refresh}
              variant={buttonVariants.secondary}
            >
              Повторить
            </Button>
          </Stack>
        </Alert>
      </Container>
    );
  }

  if (!context) {
    return (
      <Container className={styles.page} size="h3_page">
        <Stack gap="h3_md">
          <Skeleton height={30} width={240} />
          <Skeleton height={36} width={320} />
          <Skeleton height={220} />
        </Stack>
      </Container>
    );
  }

  return (
    <>
      {headerSlot
        ? createPortal(
            <Group className={styles.instanceHeaderActions} gap="h3_xs" wrap="nowrap">
              <span className={styles.statusCell}>
                <span
                  aria-hidden="true"
                  className={styles.statusDot}
                  data-status={context.instance.status}
                />
                <Text size="h3_sm">{STATUS_LABELS[context.instance.status]}</Text>
              </span>

              <Menu position="bottom-end" shadow="md" width={206}>
                <Menu.Target>
                  <ActionIcon aria-label="Действия с базой" variant={buttonVariants.ghost}>
                    <MoreHorizontal aria-hidden="true" size={18} strokeWidth={1.5} />
                  </ActionIcon>
                </Menu.Target>

                <Menu.Dropdown>
                  <Menu.Item
                    color="red"
                    disabled={context.instance.status === 'deleting'}
                    leftSection={<Trash2 aria-hidden="true" size={16} strokeWidth={1.5} />}
                    onClick={() => setDeleteOpened(true)}
                  >
                    Удалить
                  </Menu.Item>
                </Menu.Dropdown>
              </Menu>
            </Group>,
            headerSlot
          )
        : null}

      <Container className={styles.instanceHeading} fluid>
        <Stack gap="h3_lg">
          <Stack gap={0}>
            <Group gap="h3_xs" wrap="nowrap">
              <Title order={1}>{context.instance.name}</Title>
              <Tooltip label="Изменить имя">
                <ActionIcon
                  aria-label="Изменить имя"
                  className={styles.touchTarget}
                  onClick={() => setRenameOpened(true)}
                  variant={buttonVariants.ghost}
                >
                  <Pencil aria-hidden="true" size={17} strokeWidth={1.5} />
                </ActionIcon>
              </Tooltip>
            </Group>

            <Group className={styles.instanceMeta} gap="h3_xs" wrap="nowrap">
              <Text size="h3_sm">База данных</Text>
              <Text aria-hidden="true" size="h3_sm">
                /
              </Text>
              <Text size="h3_sm">Valkey</Text>
              <Text aria-hidden="true" size="h3_sm">
                /
              </Text>
              <Text className={styles.instanceId} fw={450} size="h3_sm">
                {context.instance.id}
              </Text>
              <CopyAction label="Скопировать ID" value={context.instance.id} />
            </Group>
          </Stack>

          <Tabs className={styles.instanceTabs} value={getActiveTab(pathname)} variant="default">
            <Tabs.List aria-label={`Разделы базы ${context.instance.name}`}>
              {TABS.map((tab) => (
                <Tabs.Tab
                  key={tab.value}
                  renderRoot={(props) => (
                    <Link {...props} to={valkeyInstancePath(instanceId, tab.tab)} />
                  )}
                  value={tab.value}
                >
                  {tab.label}
                </Tabs.Tab>
              ))}
            </Tabs.List>
          </Tabs>
        </Stack>
      </Container>

      <ValkeyAside
        header={<PricePeriodTabs onChange={setPeriod} period={period} />}
        label="Текущая стоимость"
      >
        <PricePanel
          mode={context.instance.mode}
          period={period}
          pricing={context.catalog.pricing}
          size={context.instance}
        />
      </ValkeyAside>

      <ValkeyInstanceContext value={context}>
        <Outlet />
      </ValkeyInstanceContext>

      <DeleteInstanceModal
        instance={deleteOpened ? context.instance : null}
        onClose={() => setDeleteOpened(false)}
        onDeleted={refresh}
      />

      <RenameInstanceModal
        instance={renameOpened ? context.instance : null}
        onClose={() => setRenameOpened(false)}
        onRefresh={refresh}
        onRenamed={applyUpdate}
      />
    </>
  );
}
