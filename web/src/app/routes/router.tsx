import { createBrowserRouter } from 'react-router';
import { HomePage } from '@/pages/home';
import { routes } from '@/shared/config';

export const router = createBrowserRouter([
  {
    path: routes.home,
    Component: HomePage,
  },
]);
