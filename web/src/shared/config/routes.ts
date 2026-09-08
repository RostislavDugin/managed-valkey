/** Пути маршрутов приложения. Страницы и навигация ссылаются на них, а не на строки. */
export const routes = {
  auth: '/auth',
  home: '/',
  valkey: '/valkey',
  valkeyManagement: '/valkey/management',
  valkeyCreate: '/valkey/management/new',
  valkeyInstance: '/valkey/management/:instanceId',
} as const;

export type ValkeyInstanceTab = 'monitoring' | 'audit-logs';

export function valkeyInstancePath(instanceId: string, tab?: ValkeyInstanceTab) {
  const path = routes.valkeyInstance.replace(':instanceId', instanceId);
  return tab ? `${path}/${tab}` : path;
}
