import { RouterProvider } from 'react-router';
import { MantineProvider } from '@mantine/core';
import { Notifications } from '@mantine/notifications';
import { router } from './routes';
import { cssVariablesResolver, theme } from './styles/theme';
import './styles/index.css';

export function App() {
  return (
    <MantineProvider
      theme={theme}
      cssVariablesResolver={cssVariablesResolver}
      defaultColorScheme="auto"
    >
      <Notifications position="top-right" containerWidth={440} />
      <RouterProvider router={router} />
    </MantineProvider>
  );
}
