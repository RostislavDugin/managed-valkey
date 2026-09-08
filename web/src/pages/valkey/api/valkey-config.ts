export interface ValkeyDomain {
  domain: string;
  port: number;
}

/** Значения повторяют `VALKEY_BASE_DOMAIN` и `VALKEY_PUBLIC_PORT`: SYSTEM.md, раздел 7. */
const DEFAULT_DOMAIN = 'valkey.h3llo-demo.com';
const DEFAULT_PORT = 41379;
const RESPONSE_DELAY_MS = 400;

const delay = () => new Promise((resolve) => setTimeout(resolve, RESPONSE_DELAY_MS));

/**
 * Настройки домена приходят с сервера, поэтому и здесь это запрос, а не
 * константа: замена временного клиента не должна трогать страницы.
 */
export async function getValkeyDomain(): Promise<ValkeyDomain> {
  await delay();

  const domain = String(import.meta.env.VITE_VALKEY_BASE_DOMAIN ?? '');
  const port = Number.parseInt(String(import.meta.env.VITE_VALKEY_PUBLIC_PORT ?? ''), 10);

  return {
    domain: domain || DEFAULT_DOMAIN,
    port: Number.isInteger(port) ? port : DEFAULT_PORT,
  };
}
