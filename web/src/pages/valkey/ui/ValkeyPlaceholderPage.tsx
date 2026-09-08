import { Container, Stack, Text, Title } from '@mantine/core';
import styles from './ValkeyPage.module.css';

export function ValkeyPlaceholderPage({ title }: { title: string }) {
  return (
    <Container className={`${styles.page} ${styles.instanceContent}`} component="section" fluid>
      <Stack gap="h3_sm">
        <Title order={2}>{title}</Title>
        <Text c="h3_text_2">Раздел появится позже</Text>
      </Stack>
    </Container>
  );
}
