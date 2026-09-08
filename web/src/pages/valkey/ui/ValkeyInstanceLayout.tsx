import { createContext, use, useCallback, useEffect, useMemo, useState } from 'react';
import { MoreHorizontal, Pencil, RotateCw, Trash2 } from 'lucide-react';
import { createPortal } from 'react-dom';
import { Link, Outlet, useLocation, useNavigate, useParams } from 'react-router';
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
import { getValkeyDomain, type ValkeyDomain } from '../api/valkey-config';
import { getInstance, listInstances } from '../api/valkey-storage';
import { STATUS_LABELS, type PricePeriod, type ValkeyInstance } from '../model/valkey';
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
  instances: ValkeyInstance[];
  domain: ValkeyDomain | null;
  applyUpdate: (updated: ValkeyInstance) => void;
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
  const navigate = useNavigate();

  const [instance, setInstance] = useState<ValkeyInstance | null>(null);
  const [instances, setInstances] = useState<ValkeyInstance[]>([]);
  const [domain, setDomain] = useState<ValkeyDomain | null>(null);
  const [error, setError] = useState<Error | null>(null);
  const [period, setPeriod] = useState<PricePeriod>('month');
  const [deleteOpened, setDeleteOpened] = useState(false);
  const [renameOpened, setRenameOpened] = useState(false);

  const load = useCallback(async () => {
    setError(null);
    setInstance(null);

    try {
      // Недоступный домен оставляет карточку рабочей: адрес откатывается к slug.
      const [loaded, all, loadedDomain] = await Promise.all([
        getInstance(session.userId, instanceId),
        listInstances(session.userId),
        getValkeyDomain().catch(() => null),
      ]);

      setInstance(loaded);
      setInstances(all);
      setDomain(loadedDomain);
    } catch (loadError) {
      setError(loadError instanceof Error ? loadError : new Error(String(loadError)));
    }
  }, [instanceId, session.userId]);

  useEffect(() => {
    void load();
  }, [load]);

  useEffect(() => {
    setTrailingCrumb(instance?.name ?? null);
    return () => setTrailingCrumb(null);
  }, [instance?.name, setTrailingCrumb]);

  const applyUpdate = useCallback((updated: ValkeyInstance) => {
    setInstance(updated);
    setInstances((current) => current.map((item) => (item.id === updated.id ? updated : item)));
  }, []);

  const context = useMemo<ValkeyInstanceContextValue | null>(
    () => (instance ? { session, instance, instances, domain, applyUpdate } : null),
    [applyUpdate, domain, instance, instances, session]
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
              onClick={() => void load()}
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
                <span aria-hidden="true" className={styles.statusDot} />
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
        <PricePanel mode={context.instance.mode} period={period} size={context.instance} />
      </ValkeyAside>

      <ValkeyInstanceContext value={context}>
        <Outlet />
      </ValkeyInstanceContext>

      <DeleteInstanceModal
        author={session}
        instance={deleteOpened ? context.instance : null}
        onClose={() => setDeleteOpened(false)}
        onDeleted={() => void navigate(routes.valkeyManagement)}
      />

      <RenameInstanceModal
        author={session}
        instance={renameOpened ? context.instance : null}
        onClose={() => setRenameOpened(false)}
        onRenamed={applyUpdate}
      />
    </>
  );
}
