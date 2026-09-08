import { useCallback, useEffect, useRef, useState } from 'react';
import { CircleHelp, Database, LogOut, Menu as MenuIcon, Moon, Sun, User } from 'lucide-react';
import { NavLink, Outlet, useLoaderData, useNavigate, useNavigation } from 'react-router';
import {
  ActionIcon,
  Drawer,
  Group,
  Menu,
  NavLink as MantineNavLink,
  Progress,
  Stack,
  Text,
  Tooltip,
  UnstyledButton,
  useComputedColorScheme,
  useMantineColorScheme,
} from '@mantine/core';
import { notifications } from '@mantine/notifications';
import { AUTH_INVALIDATED_EVENT, AUTH_TOKEN_KEY, logout, type Session } from '@/shared/api';
import { buttonVariants, routes } from '@/shared/config';
import { LogoWide } from '@/shared/ui';
import styles from './ConsoleShell.module.css';

function ConsoleNavigation({ email, onNavigate }: { email: string; onNavigate?: () => void }) {
  const navigate = useNavigate();
  const computedColorScheme = useComputedColorScheme('light');
  const { setColorScheme } = useMantineColorScheme();
  const isDark = computedColorScheme === 'dark';

  const handleLogout = async () => {
    await logout();
    onNavigate?.();
    await navigate(routes.auth, { replace: true });
  };

  return (
    <nav className={styles.navigation} aria-label="Основная навигация">
      <LogoWide className={styles.logo} width={117} />

      <div className={styles.navigationBody}>
        <MantineNavLink
          className={styles.navLink}
          label="Базы данных"
          leftSection={<Database aria-hidden="true" size={16} strokeWidth={1.5} />}
          onClick={onNavigate}
          renderRoot={(props) => <NavLink {...props} to={routes.valkey} />}
        />
      </div>

      <Stack className={styles.navigationFooter} gap={0}>
        <Menu position="top" shadow="md" width="target" withinPortal>
          <Menu.Target>
            <UnstyledButton className={styles.emailRow} title={email}>
              <User aria-hidden="true" size={16} strokeWidth={1.5} />
              <Text className={styles.email} size="h3_sm">
                {email}
              </Text>
            </UnstyledButton>
          </Menu.Target>

          <Menu.Dropdown>
            <Menu.Item
              leftSection={<LogOut aria-hidden="true" size={16} strokeWidth={1.5} />}
              onClick={handleLogout}
            >
              Выйти
            </Menu.Item>
          </Menu.Dropdown>
        </Menu>

        <Group className={styles.iconRow} gap="h3_xs">
          <Tooltip label={isDark ? 'Включить светлую схему' : 'Включить тёмную схему'}>
            <ActionIcon
              aria-label={isDark ? 'Включить светлую схему' : 'Включить тёмную схему'}
              onClick={() => setColorScheme(isDark ? 'light' : 'dark')}
              size="md"
              variant={buttonVariants.ghost}
            >
              {isDark ? (
                <Sun aria-hidden="true" size={16} strokeWidth={1.5} />
              ) : (
                <Moon aria-hidden="true" size={16} strokeWidth={1.5} />
              )}
            </ActionIcon>
          </Tooltip>
          <Tooltip label="Справка появится позже">
            <ActionIcon aria-label="Справка" disabled size="md" variant={buttonVariants.ghost}>
              <CircleHelp aria-hidden="true" size={16} strokeWidth={1.5} />
            </ActionIcon>
          </Tooltip>
        </Group>
      </Stack>
    </nav>
  );
}

export function ConsoleShell() {
  const { session } = useLoaderData() as { session: Session };
  const navigate = useNavigate();
  const navigation = useNavigation();
  const [drawerOpened, setDrawerOpened] = useState(false);
  const [headerSlot, setHeaderSlot] = useState<HTMLElement | null>(null);
  const [asideSlot, setAsideSlot] = useState<HTMLElement | null>(null);
  const endingSession = useRef(false);

  const endExpiredSession = useCallback(async () => {
    if (endingSession.current) {
      return;
    }
    endingSession.current = true;
    await logout();
    notifications.show({
      color: 'red',
      message: 'Войдите снова, чтобы продолжить работу.',
      title: 'Сессия истекла',
    });
    await navigate(routes.auth, { replace: true });
  }, [navigate]);

  useEffect(() => {
    const remaining = session.expiresAt - Date.now();
    if (remaining <= 0) {
      void endExpiredSession();
      return;
    }

    const timeout = window.setTimeout(() => void endExpiredSession(), remaining);
    return () => window.clearTimeout(timeout);
  }, [endExpiredSession, session.expiresAt]);

  useEffect(() => {
    const handleInvalidated = () => void endExpiredSession();
    const handleStorage = (event: StorageEvent) => {
      if (event.key === AUTH_TOKEN_KEY && event.newValue === null) {
        void endExpiredSession();
      }
    };

    window.addEventListener(AUTH_INVALIDATED_EVENT, handleInvalidated);
    window.addEventListener('storage', handleStorage);
    return () => {
      window.removeEventListener(AUTH_INVALIDATED_EVENT, handleInvalidated);
      window.removeEventListener('storage', handleStorage);
    };
  }, [endExpiredSession]);

  return (
    <div className={styles.shell}>
      <a className={styles.skipLink} href="#main-content">
        Пропустить навигацию
      </a>

      {navigation.state !== 'idle' && (
        <Progress
          aria-label="Загрузка страницы"
          className={styles.progress}
          color="h3_bg_accent"
          size="xs"
          value={100}
          animated
        />
      )}

      <aside className={styles.desktopNavigation}>
        <ConsoleNavigation email={session.email} />
      </aside>

      <header className={styles.mobileHeader}>
        <LogoWide width={124} />
        <ActionIcon
          aria-label="Открыть навигацию"
          onClick={() => setDrawerOpened(true)}
          size="lg"
          variant={buttonVariants.ghost}
        >
          <MenuIcon aria-hidden="true" size={16} strokeWidth={1.5} />
        </ActionIcon>
      </header>

      <Drawer
        classNames={{ body: styles.drawerBody }}
        onClose={() => setDrawerOpened(false)}
        opened={drawerOpened}
        padding={0}
        size="var(--h3-navbar-width)"
        withCloseButton={false}
      >
        <ConsoleNavigation email={session.email} onNavigate={() => setDrawerOpened(false)} />
      </Drawer>

      <aside className={styles.asideColumn} ref={setAsideSlot} />

      <div className={styles.header} ref={setHeaderSlot} />

      <main className={styles.main} id="main-content" tabIndex={-1}>
        <Outlet context={{ session, headerSlot, asideSlot }} />
      </main>
    </div>
  );
}
