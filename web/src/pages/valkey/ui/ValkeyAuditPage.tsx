import { Container, Stack, Text, Title } from '@mantine/core';
import pageStyles from './ValkeyPage.module.css';

export function ValkeyAuditPage() {
  return (
    <Container
      className={`${pageStyles.page} ${pageStyles.instanceContent}`}
      component="section"
      fluid
    >
      <Stack gap="h3_lg">
        <Title order={2}>Аудит</Title>
        <div className={pageStyles.emptyData}>
          <Text c="dimmed" size="h3_sm">
            Событий аудита пока нет
          </Text>
        </div>
      </Stack>
    </Container>
  );
}
