import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Database, Gauge, KeyRound, Pencil, RotateCw, Trash2, type LucideIcon } from 'lucide-react';
import { Alert, Button, Code, Container, Group, Skeleton, Stack, Text, Title } from '@mantine/core';
import { buttonVariants } from '@/shared/config';
import { formatDateTime, formatRelativeTime } from '@/shared/lib';
import { getAuditLogPage } from '../api/valkey-audit';
import { getRequestErrorMessage } from '../model/valkey-form';
import {
  AUDIT_ACTION_LABELS,
  groupAuditLogs,
  type AuditAction,
  type AuditLogEntry,
} from '../model/valkey-observability';
import { useValkeyInstance } from './ValkeyInstanceLayout';
import styles from './ValkeyAuditPage.module.css';
import pageStyles from './ValkeyPage.module.css';

const ACTION_ICONS: Record<AuditAction, LucideIcon> = {
  'instance.create': Database,
  'instance.update': Pencil,
  'instance.resize': Gauge,
  'instance.password.rotate': KeyRound,
  'instance.delete': Trash2,
};

function AuditRow({ item }: { item: AuditLogEntry }) {
  const Icon = ACTION_ICONS[item.action];

  return (
    <article className={styles.row}>
      <div className={styles.icon}>
        <Icon aria-hidden="true" size={18} strokeWidth={1.5} />
      </div>

      <div className={styles.action}>
        <Text fw="var(--h3-fw-medium)" size="h3_sm">
          {AUDIT_ACTION_LABELS[item.action]}
        </Text>
        <Code className={styles.technicalAction}>{item.action}</Code>
      </div>

      <Text className={styles.email} c="h3_text_2" size="h3_sm">
        {item.userEmail}
      </Text>

      <div className={styles.time}>
        <Text size="h3_sm">{formatDateTime(item.createdAt)}</Text>
        <Text c="h3_text_2" size="h3_xs">
          {formatRelativeTime(item.createdAt)}
        </Text>
      </div>
    </article>
  );
}

export function ValkeyAuditPage() {
  const { instance, session } = useValkeyInstance();
  const [items, setItems] = useState<AuditLogEntry[]>([]);
  const [nextCursor, setNextCursor] = useState<string | null>(null);
  const [initialLoading, setInitialLoading] = useState(true);
  const [moreLoading, setMoreLoading] = useState(false);
  const [error, setError] = useState<Error | null>(null);
  const [revision, setRevision] = useState(0);
  const [now, setNow] = useState(() => new Date());
  const requestIdRef = useRef(0);
  const moreLoadingRef = useRef(false);

  useEffect(() => {
    const interval = setInterval(() => setNow(new Date()), 60_000);
    return () => clearInterval(interval);
  }, []);

  useEffect(() => {
    const requestId = requestIdRef.current + 1;
    requestIdRef.current = requestId;
    setInitialLoading(true);
    setError(null);

    void getAuditLogPage({ ownerId: session.userId, instanceId: instance.id })
      .then((page) => {
        if (requestIdRef.current !== requestId) {
          return;
        }

        setItems(page.items);
        setNextCursor(page.nextCursor);
      })
      .catch((requestError: unknown) => {
        if (requestIdRef.current === requestId) {
          setError(requestError instanceof Error ? requestError : new Error(String(requestError)));
        }
      })
      .finally(() => {
        if (requestIdRef.current === requestId) {
          setInitialLoading(false);
        }
      });
  }, [instance.id, revision, session.userId]);

  const groups = useMemo(() => groupAuditLogs(items, now), [items, now]);
  const retry = useCallback(() => setRevision((current) => current + 1), []);
  const loadMore = useCallback(async () => {
    if (!nextCursor || moreLoadingRef.current) {
      return;
    }

    moreLoadingRef.current = true;
    setMoreLoading(true);
    setError(null);

    try {
      const page = await getAuditLogPage({
        ownerId: session.userId,
        instanceId: instance.id,
        before: nextCursor,
      });
      setItems((current) => {
        const known = new Set(current.map((item) => item.id));
        return [...current, ...page.items.filter((item) => !known.has(item.id))];
      });
      setNextCursor(page.nextCursor);
    } catch (requestError) {
      setError(requestError instanceof Error ? requestError : new Error(String(requestError)));
    } finally {
      moreLoadingRef.current = false;
      setMoreLoading(false);
    }
  }, [instance.id, nextCursor, session.userId]);

  return (
    <Container
      className={`${pageStyles.page} ${pageStyles.instanceContent} ${styles.page}`}
      component="section"
      fluid
    >
      <Stack gap="h3_lg">
        <Title order={2}>Аудит</Title>

        {error ? (
          <Alert color="red" title="Не удалось загрузить аудит">
            <Stack align="flex-start" gap="h3_sm">
              <Text size="h3_sm">{getRequestErrorMessage(error)}</Text>
              <Button
                leftSection={<RotateCw aria-hidden="true" size={16} strokeWidth={1.5} />}
                onClick={items.length === 0 ? retry : () => void loadMore()}
                variant={buttonVariants.secondary}
              >
                Повторить
              </Button>
            </Stack>
          </Alert>
        ) : null}

        {initialLoading ? (
          <Stack aria-label="Загрузка аудита" className={styles.state} gap="h3_sm">
            <Skeleton height={30} width={120} />
            <Skeleton height={76} />
            <Skeleton height={76} />
            <Skeleton height={76} />
          </Stack>
        ) : null}

        {!initialLoading && !error && items.length === 0 ? (
          <Stack className={styles.state} gap="h3_sm">
            <Title order={3}>Действий с базой пока не было</Title>
            <Text c="h3_text_2">Здесь появятся создание и изменения этой базы.</Text>
          </Stack>
        ) : null}

        {!initialLoading && items.length > 0 ? (
          <Stack gap="h3_lg">
            {groups.map((group) => (
              <section className={styles.group} key={group.key}>
                <Title order={3}>{group.label}</Title>
                <div className={styles.rows}>
                  {group.items.map((item) => (
                    <AuditRow item={item} key={item.id} />
                  ))}
                </div>
              </section>
            ))}

            {nextCursor ? (
              <Group justify="center">
                <Button
                  disabled={moreLoading}
                  loading={moreLoading}
                  onClick={() => void loadMore()}
                  variant={buttonVariants.secondary}
                >
                  Показать ещё
                </Button>
              </Group>
            ) : null}
          </Stack>
        ) : null}
      </Stack>
    </Container>
  );
}
