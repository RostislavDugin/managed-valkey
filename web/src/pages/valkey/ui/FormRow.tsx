import type { ReactNode } from 'react';
import { Info } from 'lucide-react';
import { Group, Input, Tooltip } from '@mantine/core';
import styles from './ValkeyPage.module.css';

interface FormRowProps {
  children: ReactNode;
  label?: string;
  hint?: string;
  htmlFor?: string;
  labelId?: string;
  /** Поле, которому нужна вся ширина строки: подпись встаёт над ним. */
  fullWidth?: boolean;
}

export function FormRow({ children, fullWidth, hint, htmlFor, label, labelId }: FormRowProps) {
  const labelGroup = label ? (
    <Group align="center" gap={4}>
      <Input.Label
        component={htmlFor ? 'label' : 'span'}
        htmlFor={htmlFor}
        id={labelId}
        size="h3_md"
      >
        {label}
      </Input.Label>

      {hint ? (
        <Tooltip label={hint} multiline w={240} withArrow>
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
  ) : null;

  if (fullWidth) {
    return (
      <div className={styles.formRowFull}>
        {labelGroup}
        {children}
      </div>
    );
  }

  return (
    <div className={styles.formRow}>
      <div className={styles.formLabel}>{labelGroup}</div>
      <div className={styles.formControl}>{children}</div>
    </div>
  );
}
