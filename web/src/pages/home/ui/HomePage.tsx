import { Button, Container, Stack, Text, Title } from '@mantine/core';
import { buttonVariants } from '@/shared/config';

export function HomePage() {
  return (
    <Container component="section" size="h3_page" py="h3_xl">
      <Stack gap="h3_lg">
        <Title order={1}>Инстансы</Title>

        <Text c="h3_text_2">
          Здесь появятся созданные инстансы Valkey и сведения об использовании квоты.
        </Text>

        <Button variant={buttonVariants.accent} w="fit-content">
          Создать инстанс
        </Button>
      </Stack>
    </Container>
  );
}
