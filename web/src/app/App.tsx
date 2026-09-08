import '@mantine/code-highlight/styles.layer.css';

import { useSyncExternalStore } from 'react';
import { RouterProvider } from 'react-router';
import { CodeHighlightAdapterProvider } from '@mantine/code-highlight';
import { Center, Loader, MantineProvider } from '@mantine/core';
import { Notifications } from '@mantine/notifications';
import { router } from './routes';
import { codeHighlightAdapter } from './styles/code-highlight';
import { cssVariablesResolver, theme } from './styles/theme';
import './styles/index.css';

function subscribeToRouter(onStoreChange: () => void) {
  return router.subscribe(onStoreChange);
}

function InitialSessionLoader() {
  return (
    <Center mih="100dvh">
      <Loader aria-label="Проверка сессии" color="h3_bg_accent" />
    </Center>
  );
}

function AppRouter() {
  const initialized = useSyncExternalStore(subscribeToRouter, () => router.state.initialized);

  return initialized ? <RouterProvider router={router} /> : <InitialSessionLoader />;
}

export function App() {
  return (
    <MantineProvider
      theme={theme}
      cssVariablesResolver={cssVariablesResolver}
      defaultColorScheme="auto"
    >
      <CodeHighlightAdapterProvider adapter={codeHighlightAdapter}>
        <Notifications position="top-right" containerWidth={440} />
        <AppRouter />
      </CodeHighlightAdapterProvider>
    </MantineProvider>
  );
}
