import { Check, Copy } from 'lucide-react';
import { ActionIcon, CopyButton, Tooltip } from '@mantine/core';
import { buttonVariants } from '@/shared/config';
import styles from './ValkeyPage.module.css';

export function CopyAction({ label, value }: { label: string; value: string }) {
  return (
    <CopyButton value={value}>
      {({ copied, copy }) => (
        <Tooltip label={copied ? 'Скопировано' : label}>
          <ActionIcon
            aria-label={copied ? 'Скопировано' : label}
            className={`${styles.touchTarget} ${styles.copyAction}`}
            onClick={copy}
            variant={buttonVariants.ghost}
          >
            {copied ? (
              <Check aria-hidden="true" size={16} strokeWidth={1.5} />
            ) : (
              <Copy aria-hidden="true" size={16} strokeWidth={1.5} />
            )}
          </ActionIcon>
        </Tooltip>
      )}
    </CopyButton>
  );
}
