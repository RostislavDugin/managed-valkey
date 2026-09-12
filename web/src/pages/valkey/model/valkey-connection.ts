export type ValkeyConnectionScheme = 'redis' | 'rediss';

export function getValkeyConnectionScheme(webProtocol: string): ValkeyConnectionScheme {
  return webProtocol === 'https:' ? 'rediss' : 'redis';
}

export function formatValkeyAddress(
  host: string,
  port: number | string | undefined,
  scheme: ValkeyConnectionScheme
) {
  return `${scheme}://${host}${port === undefined ? '' : `:${port}`}`;
}
