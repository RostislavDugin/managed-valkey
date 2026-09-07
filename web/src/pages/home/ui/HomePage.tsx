import { Moon, Sun } from 'lucide-react';
import {
  ActionIcon,
  Button,
  Container,
  Group,
  Stack,
  Text,
  TextInput,
  Title,
  useMantineColorScheme,
} from '@mantine/core';
import { buttonVariants } from '@/shared/config';

export function HomePage() {
  const { colorScheme, toggleColorScheme } = useMantineColorScheme();
  const isDark = colorScheme === 'dark';

  return (
    <Container size="h3_page" py="h3_xl">
      <Stack gap="h3_lg">
        <Group justify="space-between" align="center">
          <Title order={1}>Managed Valkey</Title>
          <ActionIcon
            variant={buttonVariants.ghost}
            size="md"
            aria-label={isDark ? 'Включить светлую схему' : 'Включить тёмную схему'}
            onClick={toggleColorScheme}
          >
            {isDark ? <Sun size={16} strokeWidth={1.5} /> : <Moon size={16} strokeWidth={1.5} />}
          </ActionIcon>
        </Group>

        <Text c="h3_text_2">
          Каркас консоли. Страница проверяет, что тема Mantine и слой Tailwind получают одни и те же
          токены в обеих цветовых схемах.
        </Text>

        <Group gap="h3_xs">
          <Button variant={buttonVariants.accent}>Создать инстанс</Button>
          <Button variant={buttonVariants.secondary}>Обновить</Button>
          <Button variant={buttonVariants.stroke}>Сгенерировать имя</Button>
          <Button variant={buttonVariants.ghost}>Отмена</Button>
        </Group>

        <TextInput label="Имя инстанса" placeholder="valkey-1474" />

        <div className="rounded-h3-sm border border-h3-border bg-h3-bg-1 p-h3-md text-h3-sm text-h3-text-2">
          Блок оформлен классами Tailwind: фон <code className="font-mono">h3_bg_1</code>, граница{' '}
          <code className="font-mono">h3_border</code>, текст{' '}
          <code className="font-mono">h3_text_2</code>.{' '}
          <span className="text-h3-text-accent">Акцентный текст</span> берёт тот же токен, что и
          ссылки Mantine.
        </div>
      </Stack>
    </Container>
  );
}
