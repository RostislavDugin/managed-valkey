import type { ReactNode } from 'react';
import { Info, PanelRight } from 'lucide-react';
import { createPortal } from 'react-dom';
import {
  ActionIcon,
  Button,
  Drawer,
  Group,
  Stack,
  Title,
  Tooltip,
  VisuallyHidden,
} from '@mantine/core';
import { useDisclosure, useMediaQuery } from '@mantine/hooks';
import { buttonVariants } from '@/shared/config';
import { useValkeySection } from './ValkeyLayout';
import styles from './ValkeyPage.module.css';

interface ValkeyAsideProps {
  description?: string;
  label: string;
  children: ReactNode;
  header?: ReactNode;
}

/**
 * Правая колонка живёт в каркасе консоли, а не в контейнере страницы, поэтому
 * содержимое уезжает туда порталом. Точка `aside` (1280px) описана в
 * web/DESIGN.md, раздел «Правая панель».
 */
export function ValkeyAside({ children, description, header, label }: ValkeyAsideProps) {
  const { asideSlot } = useValkeySection();
  const [opened, { close, open }] = useDisclosure(false);
  const isWide = useMediaQuery('(min-width: 80em)', true);

  if (isWide && asideSlot) {
    return createPortal(
      <div className={styles.asideCard}>
        <div className={styles.asideHeader} data-custom-header={header ? true : undefined}>
          {header ? <VisuallyHidden component="h2">{label}</VisuallyHidden> : null}

          {header ?? (
            <div className={styles.asideTitle}>
              <Group gap={4} wrap="nowrap">
                <Title order={2} size="h3_md">
                  {label}
                </Title>

                {description ? (
                  <Tooltip label={description} multiline w={280}>
                    <ActionIcon
                      aria-label={`Что такое ${label.toLowerCase()}`}
                      size="md"
                      variant={buttonVariants.ghost}
                    >
                      <Info aria-hidden="true" size={16} strokeWidth={1.5} />
                    </ActionIcon>
                  </Tooltip>
                ) : null}
              </Group>
            </div>
          )}
        </div>

        <div className={styles.asideBody}>{children}</div>
      </div>,
      asideSlot
    );
  }

  return (
    <div className={styles.asideTrigger}>
      <Button
        leftSection={<PanelRight aria-hidden="true" size={16} strokeWidth={1.5} />}
        onClick={open}
        variant={buttonVariants.stroke}
        fullWidth
      >
        {label}
      </Button>

      <Drawer
        onClose={close}
        opened={opened}
        position="right"
        size="20rem"
        title={
          <div className={styles.asideTitle}>
            <Group gap={4} wrap="nowrap">
              <span>{label}</span>

              {description ? (
                <Tooltip label={description} multiline w={280}>
                  <ActionIcon
                    aria-label={`Что такое ${label.toLowerCase()}`}
                    size="md"
                    variant={buttonVariants.ghost}
                  >
                    <Info aria-hidden="true" size={16} strokeWidth={1.5} />
                  </ActionIcon>
                </Tooltip>
              ) : null}
            </Group>
          </div>
        }
      >
        <Stack gap="h3_md">
          {header}
          {children}
        </Stack>
      </Drawer>
    </div>
  );
}
