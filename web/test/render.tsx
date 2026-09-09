import { useState, type ReactElement } from 'react';
import { render } from '@testing-library/react';
import { createMemoryRouter, Outlet, RouterProvider, type RouteObject } from 'react-router';
import { vi } from 'vitest';
import { CodeHighlightAdapterProvider } from '@mantine/code-highlight';
import { MantineProvider } from '@mantine/core';
import { Notifications } from '@mantine/notifications';
import { codeHighlightAdapter } from '@/app/styles/code-highlight';
import { cssVariablesResolver, theme } from '@/app/styles/theme';
import {
  CreateValkeyPage,
  ValkeyAuditPage,
  ValkeyInstanceLayout,
  ValkeyInstancePage,
  ValkeyLayout,
  ValkeyManagementPage,
  ValkeyMonitoringPage,
} from '@/pages/valkey';
import { AUTH_TOKEN_KEY, type Session } from '@/shared/api';

export const TEST_USER_ID = '01930000-0000-7000-8000-000000000001';
export const TEST_VALKEY_PASSWORD = 'abcdEFGHijklMNOPqrstUVWXyz01_234';

interface TestInstanceInput {
  id: string;
  name: string;
  mode: 'single' | 'ha';
  vcpu: number;
  ramGb: number;
}

function instanceDto(instance: TestInstanceInput) {
  const now = new Date().toISOString();
  return {
    id: instance.id,
    name: instance.name,
    slug: `valkey-${instance.id}`,
    mode: instance.mode,
    vcpu: instance.vcpu,
    ram_gb: instance.ramGb,
    applied_vcpu: instance.vcpu,
    applied_ram_gb: instance.ramGb,
    host: `valkey-${instance.id}.valkey.test`,
    host_ro: null,
    port: 41379,
    is_whitelist_enabled: false,
    whitelist_cidrs: [],
    maintenance: null,
    password_hint: 'demo*****',
    password_version: 1,
    applied_password_version: 1,
    status: 'running',
    phase_reason: null,
    desired_generation: 1,
    observed_generation: 1,
    observed_at: now,
    is_stale: false,
    is_updating: false,
    is_recovery_required: false,
    network_verification_status: 'verified',
    network_verified_at: now,
    created_at: now,
    updated_at: now,
    configuration_requested_at: now,
    deletion_requested_at: null,
  };
}

function installValkeyApi(instances: TestInstanceInput[]) {
  const dtos = instances.map(instanceDto);
  window.fetch = vi.fn(async (input) => {
    const path = String(input);
    let body: unknown;
    let status = 200;

    if (path === '/v1/me') {
      body = {
        user: { id: TEST_USER_ID, email: 'user@example.com' },
        quota: { max_vcpu: 4, max_ram_gb: 16 },
        usage: { used_vcpu: 0, used_ram_gb: 0 },
      };
    } else if (path === '/v1/managed/valkey/sizes') {
      body = {
        items: [
          { vcpu: 1, ram_gb: 1 },
          { vcpu: 1, ram_gb: 2 },
          { vcpu: 2, ram_gb: 4 },
        ],
        pricing: {
          vcpu_coins_per_hour: 125,
          ram_gb_coins_per_hour: 50,
          hours_per_month: 720,
        },
        connection: { domain: 'valkey.test', port: 41379 },
      };
    } else if (path === '/v1/managed/valkey/instances') {
      body = { items: dtos };
    } else {
      const id = path.match(/^\/v1\/managed\/valkey\/instances\/([^/]+)(?:\/credentials)?$/)?.[1];
      const instance = dtos.find((item) => item.id === id);
      if (instance && path.endsWith('/credentials')) {
        body = {
          host: instance.host,
          host_ro: instance.host_ro,
          port: instance.port,
          username: 'app',
          password_hint: instance.password_hint,
          password_version: instance.password_version,
          applied_password_version: instance.applied_password_version,
        };
      } else if (instance) {
        body = instance;
      } else {
        status = 404;
        body = { error: { code: 'NOT_FOUND', message: 'База не найдена', details: {} } };
      }
    }

    return new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    });
  });
  globalThis.fetch = window.fetch;
}

export function seedSession(
  userId = TEST_USER_ID,
  email = 'user@example.com',
  expiresInSeconds = 3600
) {
  const encode = (value: object) =>
    btoa(JSON.stringify(value)).replaceAll('+', '-').replaceAll('/', '_').replace(/=+$/, '');
  const exp = Math.floor(Date.now() / 1000) + expiresInSeconds;

  localStorage.setItem(
    AUTH_TOKEN_KEY,
    `${encode({ alg: 'HS256', typ: 'JWT' })}.${encode({ sub: userId, exp })}.signature`
  );

  installValkeyApi([]);

  return { userId, email, expiresAt: exp * 1000, token: 'demo' } satisfies Session;
}

export function seedInstances(instances: TestInstanceInput[]) {
  installValkeyApi(instances);
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
  let updateSession: (nextSession: Session) => void = () => undefined;

  function SessionShell() {
    const [currentSession, setCurrentSession] = useState(session);
    updateSession = setCurrentSession;
    return <TestConsoleShell session={currentSession} />;
  }

  const rendered = renderRoutes(
    [
      {
        element: <SessionShell />,
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
                  { path: 'monitoring', Component: ValkeyMonitoringPage },
                  { path: 'audit-logs', Component: ValkeyAuditPage },
                ],
              },
            ],
          },
        ],
      },
    ],
    initialPath
  );

  return { ...rendered, setSession: (nextSession: Session) => updateSession(nextSession) };
}
