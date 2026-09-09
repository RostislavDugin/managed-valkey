import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  Database,
  Gauge,
  KeyRound,
  Pencil,
  RotateCw,
  ShieldCheck,
  Trash2,
  type LucideIcon,
} from 'lucide-react';
import { Alert, Button, Container, Group, Skeleton, Stack, Text, Title } from '@mantine/core';
import { buttonVariants } from '@/shared/config';
import { formatDateTime, formatRelativeTime } from '@/shared/lib';
import { getAuditLogPage } from '../api/valkey-audit';
import {
  AUDIT_ACTION_LABELS,
  groupAuditLogs,
  type AuditAction,
  type AuditLogEntry,
} from '../model/valkey-audit';
import { getRequestErrorMessage } from '../model/valkey-form';
import { useValkeyInstance } from './ValkeyInstanceLayout';
import styles from './ValkeyAuditPage.module.css';
import pageStyles from './ValkeyPage.module.css';

const ACTION_ICONS: Record<AuditAction, LucideIcon> = {
  'instance.create': Database,
  'instance.update': Pencil,
  'instance.resize': Gauge,
  'instance.whitelist.update': ShieldCheck,
  'instance.password.rotate': KeyRound,
  'instance.delete': Trash2,
};

interface LoadError {
  phase: 'initial' | 'more';
  value: unknown;
}

function isAbortError(error: unknown) {
  return error instanceof DOMException && error.name === 'AbortError';
}

function AuditRow({ item, now }: { item: AuditLogEntry; now: Date }) {
  const Icon = ACTION_ICONS[item.action];

  return (
    <div className={styles.row}>
      <span className={styles.icon}>
        <Icon aria-hidden="true" size={20} strokeWidth={1.5} />
      </span>
      <div className={styles.action}>
        <Text fw={600} size="h3_sm">
          {AUDIT_ACTION_LABELS[item.action]}
        </Text>
        <Text className={styles.technicalAction} c="h3_text_2" component="code">
          {item.action}
        </Text>
      </div>
      <Text className={styles.email} c="h3_text_2" size="h3_sm" title={item.userEmail}>
        {item.userEmail}
      </Text>
      <div className={styles.time}>
        <Text size="h3_sm">{formatDateTime(item.createdAt)}</Text>
        <Text c="h3_text_2" size="h3_xs">
          {formatRelativeTime(item.createdAt, now)}
        </Text>
      </div>
    </div>
  );
}

export function ValkeyAuditPage() {
  const { instance } = useValkeyInstance();
  const [items, setItems] = useState<AuditLogEntry[]>([]);
  const [nextCursor, setNextCursor] = useState<string | null>(null);
  const [initialLoading, setInitialLoading] = useState(true);
  const [moreLoading, setMoreLoading] = useState(false);
  const [error, setError] = useState<LoadError | null>(null);
  const [revision, setRevision] = useState(0);
  const [now, setNow] = useState(() => new Date());
  const generationRef = useRef(0);
  const moreControllerRef = useRef<AbortController | null>(null);

  useEffect(() => {
    const interval = window.setInterval(() => setNow(new Date()), 60_000);
    return () => window.clearInterval(interval);
  }, []);

  useEffect(() => {
    const generation = generationRef.current + 1;
    generationRef.current = generation;
    const controller = new AbortController();

    moreControllerRef.current?.abort();
    moreControllerRef.current = null;
    setItems([]);
    setNextCursor(null);
    setError(null);
    setInitialLoading(true);
    setMoreLoading(false);

    void getAuditLogPage({ instanceId: instance.id, signal: controller.signal })
      .then((page) => {
        if (generationRef.current !== generation || controller.signal.aborted) {
          return;
        }
        setItems(page.items);
        setNextCursor(page.nextCursor);
      })
      .catch((loadError: unknown) => {
        if (generationRef.current === generation && !isAbortError(loadError)) {
          setError({ phase: 'initial', value: loadError });
        }
      })
      .finally(() => {
        if (generationRef.current === generation && !controller.signal.aborted) {
          setInitialLoading(false);
        }
      });

    return () => {
      generationRef.current += 1;
      controller.abort();
      moreControllerRef.current?.abort();
      moreControllerRef.current = null;
    };
  }, [instance.id, revision]);

  const loadMore = useCallback(async () => {
    if (!nextCursor || moreControllerRef.current) {
      return;
    }

    const generation = generationRef.current;
    const controller = new AbortController();
    moreControllerRef.current = controller;
    setMoreLoading(true);
    setError(null);

    try {
      const page = await getAuditLogPage({
        instanceId: instance.id,
        before: nextCursor,
        signal: controller.signal,
      });
      if (generationRef.current !== generation || controller.signal.aborted) {
        return;
      }

      setItems((current) => {
        const known = new Set(current.map((item) => item.id));
        return [...current, ...page.items.filter((item) => !known.has(item.id))];
      });
      setNextCursor(page.nextCursor);
    } catch (loadError) {
      if (generationRef.current === generation && !isAbortError(loadError)) {
        setError({ phase: 'more', value: loadError });
      }
    } finally {
      if (moreControllerRef.current === controller) {
        moreControllerRef.current = null;
        setMoreLoading(false);
      }
    }
  }, [instance.id, nextCursor]);

  const groups = useMemo(() => groupAuditLogs(items, now), [items, now]);

  return (
    <Container
      className={`${pageStyles.page} ${pageStyles.instanceContent} ${styles.page}`}
      component="section"
      fluid
    >
      <Stack gap="h3_lg">
        <Title order={2}>Аудит</Title>

        {error?.phase === 'initial' ? (
          <Alert color="red" title="Не удалось загрузить аудит">
            <Stack align="flex-start" gap="h3_sm">
              <Text size="h3_sm">{getRequestErrorMessage(error.value)}</Text>
              <Button
                leftSection={<RotateCw aria-hidden="true" size={16} strokeWidth={1.5} />}
                onClick={() => setRevision((current) => current + 1)}
                variant={buttonVariants.secondary}
              >
                Повторить
              </Button>
            </Stack>
          </Alert>
        ) : initialLoading ? (
          <Stack aria-label="Загрузка аудита" className={styles.state} gap="h3_sm" role="status">
            <Skeleton height={76} />
            <Skeleton height={76} />
            <Skeleton height={76} />
          </Stack>
        ) : items.length === 0 ? (
          <Stack align="center" className={styles.state} justify="center">
            <Text c="dimmed" size="h3_sm">
              Действий с базой пока не было
            </Text>
          </Stack>
        ) : (
          <Stack gap="h3_lg">
            {groups.map((group) => (
              <section className={styles.group} key={group.key}>
                <Title order={3}>{group.label}</Title>
                <div className={styles.rows}>
                  {group.items.map((item) => (
                    <AuditRow item={item} key={item.id} now={now} />
                  ))}
                </div>
              </section>
            ))}

            {error?.phase === 'more' ? (
              <Alert color="red" title="Не удалось загрузить следующие события">
                <Group align="center" justify="space-between">
                  <Text size="h3_sm">{getRequestErrorMessage(error.value)}</Text>
                  <Button onClick={() => void loadMore()} variant={buttonVariants.secondary}>
                    Повторить
                  </Button>
                </Group>
              </Alert>
            ) : nextCursor ? (
              <Button
                loading={moreLoading}
                onClick={() => void loadMore()}
                variant={buttonVariants.secondary}
              >
                Показать ещё
              </Button>
            ) : null}
          </Stack>
        )}
      </Stack>
    </Container>
  );
}
