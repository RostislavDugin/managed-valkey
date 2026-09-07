import { Link } from 'react-router';
import { Anchor, Container, Stack, Text, Title } from '@mantine/core';
import { routes } from '@/shared/config';

export function NotFoundPage() {
  return (
    <Container component="section" size="h3_page" py="h3_xl">
      <Stack gap="h3_md">
        <Title order={1}>Страница не найдена</Title>
        <Text c="h3_text_2">Проверьте адрес или вернитесь к списку инстансов.</Text>
        <Anchor component={Link} to={routes.home} underline="hover">
          Вернуться к инстансам
        </Anchor>
      </Stack>
    </Container>
  );
}
