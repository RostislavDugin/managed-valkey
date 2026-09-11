import { useCallback, useEffect, useMemo, useState } from 'react';
import { ChevronDown, ChevronUp, ChevronsUpDown, RotateCw, Search } from 'lucide-react';
import { createPortal } from 'react-dom';
import { Link, useNavigate } from 'react-router';
import {
  Alert,
  Anchor,
  Badge,
  Button,
  Container,
  ScrollArea,
  Skeleton,
  Stack,
  Table,
  Text,
  TextInput,
  Title,
  UnstyledButton,
} from '@mantine/core';
import { buttonVariants, routes, valkeyInstancePath } from '@/shared/config';
import { getValkeyCatalog, getValkeyQuota, listInstances } from '../api/valkey-api';
import type { ValkeyQuota } from '../model/quota';
import {
  formatPrice,
  formatRam,
  formatVcpu,
  getPeriodCoins,
  MODE_LABELS,
  STATUS_LABELS,
  type ValkeyCatalog,
  type ValkeyInstance,
  type ValkeyPricing,
} from '../model/valkey';
import { getRequestErrorMessage } from '../model/valkey-form';
import { QuotaUsagePanel } from './QuotaUsagePanel';
import { ValkeyAside } from './ValkeyAside';
import { useValkeySection } from './ValkeyLayout';
import styles from './ValkeyPage.module.css';

const QUOTA_DESCRIPTION =
  'Квота ограничивает количество ресурсов, которые вы можете заказать. Она защищает от случайного создания слишком большого количества серверов, например скриптом автоматизации. Чтобы увеличить квоту, обратитесь в поддержку.';

function matchesQuery(instance: ValkeyInstance, query: string) {
  const haystack = [
    instance.name,
    instance.slug,
    STATUS_LABELS[instance.status],
    MODE_LABELS[instance.mode],
    `${instance.vcpu} vCPU`,
    `${instance.ramGb} ГБ`,
  ]
    .join(' ')
    .toLowerCase();

  return haystack.includes(query.trim().toLowerCase());
}

type SortKey = 'name' | 'status' | 'mode' | 'vcpu' | 'ramGb' | 'price';
type SortDirection = 'asc' | 'desc';

interface SortHeaderProps {
  activeKey: SortKey;
  direction: SortDirection;
  label: string;
  sortKey: SortKey;
  onSort: (key: SortKey) => void;
}

function SortHeader({ activeKey, direction, label, sortKey, onSort }: SortHeaderProps) {
  const active = activeKey === sortKey;
  const Icon = active ? (direction === 'asc' ? ChevronUp : ChevronDown) : ChevronsUpDown;
  const sortLabel = active
    ? direction === 'asc'
      ? 'по возрастанию'
      : 'по убыванию'
    : 'без сортировки';

  return (
    <Table.Th
      aria-sort={active ? (direction === 'asc' ? 'ascending' : 'descending') : 'none'}
      className={styles.sortHeader}
    >
      <UnstyledButton
        aria-label={`${label}, ${sortLabel}`}
        className={styles.sortHeaderButton}
        onClick={() => onSort(sortKey)}
      >
        <span>{label}</span>
        <Icon aria-hidden="true" size={12} strokeWidth={2} />
      </UnstyledButton>
    </Table.Th>
  );
}

function compareInstances(
  left: ValkeyInstance,
  right: ValkeyInstance,
  key: SortKey,
  pricing: ValkeyPricing
) {
  switch (key) {
    case 'name':
      return left.name.localeCompare(right.name, 'ru', { numeric: true });
    case 'status':
      return STATUS_LABELS[left.status].localeCompare(STATUS_LABELS[right.status], 'ru');
    case 'mode':
      return MODE_LABELS[left.mode].localeCompare(MODE_LABELS[right.mode], 'ru');
    case 'vcpu':
      return left.vcpu - right.vcpu;
    case 'ramGb':
      return left.ramGb - right.ramGb;
    case 'price':
      return (
        getPeriodCoins(left, left.mode, 'month', pricing) -
        getPeriodCoins(right, right.mode, 'month', pricing)
      );
  }
}

function ListSkeleton() {
  return (
    <Stack gap="h3_md">
      <Skeleton height={30} width={180} />
      <Skeleton height={36} />
      <Skeleton height={220} />
    </Stack>
  );
}

function EmptyState({ quota }: { quota: ValkeyQuota }) {
  return (
    <>
      <ValkeyAside description={QUOTA_DESCRIPTION} label="Квота">
        <QuotaUsagePanel quota={quota} />
      </ValkeyAside>

      <div className={styles.empty}>
        <Badge color="h3_bg_2" c="h3_text" radius="h3_full" size="lg" variant="filled">
          VALKEY
        </Badge>

        <Title order={1}>Управляемые базы Valkey на DDR5</Title>

        <Text c="h3_text" size="h3_lg">
          Valkey - это быстрое key/value хранилище. Мы управляем серверами и устанавливаем
          обновления. А ещё мы в среднем на ~83% быстрее других облаков, потому что используем DDR5
          и Inten Xeon 5-го поколения.{' '}
          <Anchor
            href="https://docs.google.com/document/d/1JGJM1RDjsvCVrncYPFv0NYWUjgwOfRd38CmaUwbP8YI/edit?usp=sharing"
            rel="noreferrer"
            target="_blank"
          >
            Читать сравнение на Хабре
          </Anchor>
          .
        </Text>

        <Button
          className={styles.emptyButton}
          component={Link}
          mt="h3_md"
          to={routes.valkeyCreate}
          variant={buttonVariants.accent}
        >
          Создать базу данных
        </Button>
      </div>
    </>
  );
}

export function ValkeyManagementPage() {
  const { headerSlot, session, setTrailingCrumb } = useValkeySection();
  const navigate = useNavigate();

  const [instances, setInstances] = useState<ValkeyInstance[] | null>(null);
  const [catalog, setCatalog] = useState<ValkeyCatalog | null>(null);
  const [quota, setQuota] = useState<ValkeyQuota | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [query, setQuery] = useState('');
  const [sortKey, setSortKey] = useState<SortKey>('name');
  const [sortDirection, setSortDirection] = useState<SortDirection>('asc');

  const load = useCallback(
    async (signal?: AbortSignal, showLoading = true) => {
      setError(null);
      if (showLoading) {
        setInstances(null);
      }

      try {
        const [loadedInstances, loadedCatalog, loadedQuota] = await Promise.all([
          listInstances(signal),
          getValkeyCatalog(signal),
          getValkeyQuota(signal),
        ]);
        if (signal?.aborted) {
          return;
        }
        setInstances(loadedInstances);
        setCatalog(loadedCatalog);
        setQuota(loadedQuota);
      } catch (loadError) {
        if (loadError instanceof DOMException && loadError.name === 'AbortError') {
          return;
        }
        setError(getRequestErrorMessage(loadError));
      }
    },
    [session.userId]
  );

  useEffect(() => {
    setTrailingCrumb(null);
  }, [setTrailingCrumb]);

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

    void poll(true);
    return () => {
      stopped = true;
      clearTimeout(timeout);
      controller.abort();
    };
  }, [load]);

  const found = useMemo(() => {
    if (!catalog) {
      return [];
    }

    const filtered = (instances ?? []).filter((instance) => matchesQuery(instance, query));
    const direction = sortDirection === 'asc' ? 1 : -1;

    return filtered.toSorted(
      (left, right) => compareInstances(left, right, sortKey, catalog.pricing) * direction
    );
  }, [catalog, instances, query, sortDirection, sortKey]);

  const handleSort = (key: SortKey) => {
    if (key === sortKey) {
      setSortDirection((current) => (current === 'asc' ? 'desc' : 'asc'));
      return;
    }

    setSortKey(key);
    setSortDirection('asc');
  };

  if (error) {
    return (
      <Container className={styles.page} size="h3_page">
        <Alert color="red" title="Не удалось загрузить базы">
          <Stack align="flex-start" gap="h3_sm">
            <Text size="h3_sm">{error}</Text>
            <Button
              leftSection={<RotateCw aria-hidden="true" size={16} strokeWidth={1.5} />}
              onClick={() => void load(undefined, true)}
              variant={buttonVariants.secondary}
            >
              Повторить
            </Button>
          </Stack>
        </Alert>
      </Container>
    );
  }

  if (!instances || !catalog || !quota) {
    return (
      <Container className={styles.page} size="h3_page">
        <ListSkeleton />
      </Container>
    );
  }

  if (instances.length === 0) {
    return (
      <Container className={styles.emptyPage} fluid>
        <EmptyState quota={quota} />
      </Container>
    );
  }

  return (
    <Container className={styles.page} fluid>
      {headerSlot
        ? createPortal(
            <Button
              className={styles.headerAction}
              component={Link}
              to={routes.valkeyCreate}
              variant={buttonVariants.accent}
            >
              Создать
            </Button>,
            headerSlot
          )
        : null}

      <ValkeyAside description={QUOTA_DESCRIPTION} label="Квота">
        <QuotaUsagePanel quota={quota} />
      </ValkeyAside>

      <Stack className={styles.managementContent}>
        <Title order={1}>Базы данных</Title>

        <Stack gap="h3_sm">
          <TextInput
            aria-label="Поиск по базам"
            leftSection={<Search aria-hidden="true" size={16} strokeWidth={1.5} />}
            onChange={(event) => setQuery(event.currentTarget.value)}
            placeholder="Поиск по всем полям..."
            value={query}
          />

          {found.length === 0 ? (
            <Text c="h3_text_2">Ничего не найдено</Text>
          ) : (
            <div className={styles.tableCard}>
              <ScrollArea scrollbarSize={4} type="auto">
                <Table highlightOnHover miw={720} verticalSpacing="h3_sm">
                  <Table.Thead>
                    <Table.Tr>
                      <SortHeader
                        activeKey={sortKey}
                        direction={sortDirection}
                        label="Имя"
                        onSort={handleSort}
                        sortKey="name"
                      />
                      <SortHeader
                        activeKey={sortKey}
                        direction={sortDirection}
                        label="Статус"
                        onSort={handleSort}
                        sortKey="status"
                      />
                      <SortHeader
                        activeKey={sortKey}
                        direction={sortDirection}
                        label="Режим"
                        onSort={handleSort}
                        sortKey="mode"
                      />
                      <SortHeader
                        activeKey={sortKey}
                        direction={sortDirection}
                        label="vCPU"
                        onSort={handleSort}
                        sortKey="vcpu"
                      />
                      <SortHeader
                        activeKey={sortKey}
                        direction={sortDirection}
                        label="RAM"
                        onSort={handleSort}
                        sortKey="ramGb"
                      />
                      <SortHeader
                        activeKey={sortKey}
                        direction={sortDirection}
                        label="Цена за месяц"
                        onSort={handleSort}
                        sortKey="price"
                      />
                    </Table.Tr>
                  </Table.Thead>

                  <Table.Tbody>
                    {found.map((instance) => (
                      <Table.Tr
                        className={styles.databaseRow}
                        key={instance.id}
                        onClick={() => void navigate(valkeyInstancePath(instance.id))}
                      >
                        <Table.Td>
                          <Link className={styles.nameLink} to={valkeyInstancePath(instance.id)}>
                            {instance.name}
                          </Link>
                        </Table.Td>

                        <Table.Td>
                          <span className={styles.statusCell}>
                            <span
                              aria-hidden="true"
                              className={styles.statusDot}
                              data-status={instance.status}
                            />
                            {STATUS_LABELS[instance.status]}
                          </span>
                        </Table.Td>

                        <Table.Td>{MODE_LABELS[instance.mode]}</Table.Td>
                        <Table.Td>{formatVcpu(instance.vcpu)}</Table.Td>
                        <Table.Td>{formatRam(instance.ramGb)}</Table.Td>
                        <Table.Td>
                          {formatPrice(
                            getPeriodCoins(instance, instance.mode, 'month', catalog.pricing)
                          )}
                        </Table.Td>
                      </Table.Tr>
                    ))}
                  </Table.Tbody>
                </Table>
              </ScrollArea>
            </div>
          )}
        </Stack>
      </Stack>
    </Container>
  );
}
