import { useState, type ReactElement } from 'react';
import { render } from '@testing-library/react';
import { createMemoryRouter, Outlet, RouterProvider, type RouteObject } from 'react-router';
import { CodeHighlightAdapterProvider } from '@mantine/code-highlight';
import { MantineProvider } from '@mantine/core';
import { Notifications } from '@mantine/notifications';
import { codeHighlightAdapter } from '@/app/styles/code-highlight';
import { cssVariablesResolver, theme } from '@/app/styles/theme';
import {
  CreateValkeyPage,
  ValkeyInstanceLayout,
  ValkeyInstancePage,
  ValkeyLayout,
  ValkeyManagementPage,
  ValkeyPlaceholderPage,
} from '@/pages/valkey';
import { AUTH_TOKEN_KEY, AUTH_USERS_KEY, type Session } from '@/shared/api';

export const TEST_USER_ID = '01930000-0000-7000-8000-000000000001';

export function seedSession(userId = TEST_USER_ID, email = 'user@example.com') {
  const encode = (value: object) =>
    btoa(JSON.stringify(value)).replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/, '');
  const exp = Math.floor(Date.now() / 1000) + 3600;

  localStorage.setItem(
    AUTH_USERS_KEY,
    JSON.stringify([{ id: userId, email, password: 'password1' }])
  );
  localStorage.setItem(
    AUTH_TOKEN_KEY,
    `${encode({ alg: 'none', typ: 'JWT' })}.${encode({ sub: userId, exp })}.demo`
  );

  return { userId, email, expiresAt: exp * 1000, token: 'demo' } satisfies Session;
}

/** Готовые базы в хранилище браузера: тесты не ходят через клиент раздела. */
export function seedInstances(
  instances: Array<{
    id: string;
    name: string;
    mode: 'single' | 'ha';
    vcpu: number;
    ramGb: number;
    ownerId?: string;
  }>
) {
  const now = new Date().toISOString();

  localStorage.setItem(
    'mv_valkey_instances',
    JSON.stringify({
      version: 3,
      instances: instances.map((instance) => ({
        ownerId: TEST_USER_ID,
        status: 'running',
        isWhitelistEnabled: false,
        whitelistCidrs: [],
        prefix: 'valkey',
        slug: `valkey-${instance.id}`,
        createdAt: now,
        updatedAt: now,
        ...instance,
      })),
    })
  );
}

/**
 * Итог расчёта набирается двумя элементами, поэтому сверяется общий текст.
 * Пробелы приводятся к обычным, как это делает штатный поиск по строке:
 * в числах стоят неразрывные.
 */
export function wholeText(text: string) {
  const flat = (value: string) => value.replace(/\s+/g, ' ').trim();

  return (_: string, element: Element | null) =>
    element?.tagName === 'P' && flat(element.textContent ?? '') === flat(text);
}

export function withProviders(children: ReactElement) {
  return (
    <MantineProvider
      cssVariablesResolver={cssVariablesResolver}
      defaultColorScheme="light"
      theme={theme}
    >
      <CodeHighlightAdapterProvider adapter={codeHighlightAdapter}>
        <Notifications />
        {children}
      </CodeHighlightAdapterProvider>
    </MantineProvider>
  );
}

export function renderRoutes(routes: RouteObject[], initialPath: string) {
  const router = createMemoryRouter(routes, { initialEntries: [initialPath] });
  return { ...render(withProviders(<RouterProvider router={router} />)), router };
}

/** Повторяет узлы шапки и правой колонки, которые раздел наполняет порталом. */
function TestConsoleShell({ session }: { session: Session }) {
  const [headerSlot, setHeaderSlot] = useState<HTMLElement | null>(null);
  const [asideSlot, setAsideSlot] = useState<HTMLElement | null>(null);

  return (
    <>
      <div data-testid="console-header" ref={setHeaderSlot} />
      <div data-testid="console-aside" ref={setAsideSlot} />

      <Outlet context={{ session, headerSlot, asideSlot }} />
    </>
  );
}

/** Раздел Valkey с той же вложенностью маршрутов, что и в приложении. */
export function renderValkeySection(initialPath: string, session: Session = seedSession()) {
  return renderRoutes(
    [
      {
        element: <TestConsoleShell session={session} />,
        children: [
          {
            path: '/valkey',
            Component: ValkeyLayout,
            children: [
              { path: 'management', Component: ValkeyManagementPage },
              { path: 'management/new', Component: CreateValkeyPage },
              {
                path: 'management/:instanceId',
                Component: ValkeyInstanceLayout,
                children: [
                  { index: true, Component: ValkeyInstancePage },
                  { path: 'monitoring', element: <ValkeyPlaceholderPage title="Мониторинг" /> },
                  { path: 'audit-logs', element: <ValkeyPlaceholderPage title="Аудит логи" /> },
                ],
              },
            ],
          },
        ],
      },
    ],
    initialPath
  );
}
