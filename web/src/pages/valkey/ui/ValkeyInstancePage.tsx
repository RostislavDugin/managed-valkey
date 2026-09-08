import dayjs from 'dayjs';
import 'dayjs/locale/ru';
import relativeTime from 'dayjs/plugin/relativeTime';
import { useState, type ReactNode } from 'react';
import { Container, Group, Stack, Text } from '@mantine/core';
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
import { ConnectionExamples } from './ConnectionExamples';
import { CopyAction } from './CopyAction';
import { ResizeInstanceModal } from './ResizeInstanceModal';
import { useValkeyInstance } from './ValkeyInstanceLayout';
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
                  className={styles.inlineAction}
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
        instance={resizeOpened ? instance : null}
        instances={instances}
        onClose={() => setResizeOpened(false)}
        onResized={applyUpdate}
        ownerId={session.userId}
      />
    </Container>
  );
}
