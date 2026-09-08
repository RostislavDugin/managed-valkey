import {
  createBrowserRouter,
  redirect,
  type LoaderFunctionArgs,
  type RouteObject,
} from 'react-router';
import { AuthPage } from '@/pages/auth';
import { NotFoundPage } from '@/pages/not-found';
import {
  CreateValkeyPage,
  ValkeyInstanceLayout,
  ValkeyInstancePage,
  ValkeyLayout,
  ValkeyManagementPage,
  ValkeyPlaceholderPage,
} from '@/pages/valkey';
import { ApiError, getSession } from '@/shared/api';
import { routes } from '@/shared/config';
import { ConsoleShell } from './ConsoleShell';

function getRequestedPath(request: Request) {
  const url = new URL(request.url);
  return `${url.pathname}${url.search}${url.hash}`;
}

function getSafeReturnTo(value: string | null) {
  return value && value.startsWith('/') && !value.startsWith('//') && value !== routes.auth
    ? value
    : routes.home;
}

async function authLoader({ request }: LoaderFunctionArgs) {
  const url = new URL(request.url);

  try {
    if (await getSession()) {
      return redirect(routes.home);
    }
  } catch (error) {
    if (!(error instanceof ApiError) || error.code !== 'UNAUTHORIZED') {
      throw error;
    }
  }

  return {
    reason: url.searchParams.get('reason'),
    returnTo: getSafeReturnTo(url.searchParams.get('returnTo')),
  };
}

async function consoleLoader({ request }: LoaderFunctionArgs) {
  try {
    const session = await getSession();
    if (session) {
      return { session };
    }
  } catch (error) {
    if (error instanceof ApiError && error.code === 'UNAUTHORIZED') {
      const search = new URLSearchParams({
        reason: 'session-expired',
        returnTo: getRequestedPath(request),
      });
      return redirect(`${routes.auth}?${search}`);
    }
    throw error;
  }

  const search = new URLSearchParams({ returnTo: getRequestedPath(request) });
  return redirect(`${routes.auth}?${search}`);
}

/** Отдельно от роутера, чтобы таблицу можно было проверить в тестах. */
export const routeTable: RouteObject[] = [
  {
    path: routes.auth,
    loader: authLoader,
    Component: AuthPage,
  },
  {
    loader: consoleLoader,
    shouldRevalidate: () => true,
    Component: ConsoleShell,
    children: [
      {
        path: routes.home,
        loader: () => redirect(routes.valkeyManagement),
      },
      {
        path: routes.valkey,
        Component: ValkeyLayout,
        children: [
          {
            index: true,
            loader: () => redirect(routes.valkeyManagement),
          },
          {
            path: 'management',
            Component: ValkeyManagementPage,
          },
          {
            path: 'management/new',
            Component: CreateValkeyPage,
          },
          {
            path: 'management/:instanceId',
            Component: ValkeyInstanceLayout,
            children: [
              {
                index: true,
                Component: ValkeyInstancePage,
              },
              {
                path: 'monitoring',
                Component: () => <ValkeyPlaceholderPage title="Мониторинг" />,
              },
              {
                path: 'audit-logs',
                Component: () => <ValkeyPlaceholderPage title="Аудит логи" />,
              },
            ],
          },
        ],
      },
      {
        path: '*',
        Component: NotFoundPage,
      },
    ],
  },
];

export const router = createBrowserRouter(routeTable);
