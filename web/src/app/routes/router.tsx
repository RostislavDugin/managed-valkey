import { createBrowserRouter, redirect, type LoaderFunctionArgs } from 'react-router';
import { AuthPage } from '@/pages/auth';
import { HomePage } from '@/pages/home';
import { NotFoundPage } from '@/pages/not-found';
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

export const router = createBrowserRouter([
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
        Component: HomePage,
      },
      {
        path: '*',
        Component: NotFoundPage,
      },
    ],
  },
]);
