import { readFileSync } from "node:fs";
import { Valkey } from "iovalkey";

export interface ValkeyEndpoint {
  host: string;
  port: number;
  password: string;
}

export interface ValkeyInstanceConnection {
  primary: ValkeyEndpoint;
  readOnly: ValkeyEndpoint;
}

function tlsOptions(host: string) {
  const caPath = process.env.MANAGED_VALKEY_E2E_CA_FILE;
  return {
    servername: host,
    ...(caPath ? { ca: readFileSync(caPath) } : {}),
  };
}

export function newValkeyClient(endpoint: ValkeyEndpoint, reconnect = false) {
  return new Valkey({
    host: endpoint.host,
    port: endpoint.port,
    username: "app",
    password: endpoint.password,
    tls: tlsOptions(endpoint.host),
    lazyConnect: true,
    connectTimeout: 1_000,
    commandTimeout: reconnect ? 1_000 : 2_000,
    enableOfflineQueue: false,
    maxRetriesPerRequest: 0,
    retryStrategy: reconnect ? () => 100 : () => null,
  });
}

export async function connectValkey(endpoint: ValkeyEndpoint, reconnect = false) {
  const client = newValkeyClient(endpoint, reconnect);
  client.on("error", () => undefined);
  try {
    await client.connect();
    return client;
  } catch (error) {
    client.disconnect(false);
    throw error;
  }
}

export async function disconnectValkey(client: Valkey) {
  if (client.status === "end") {
    return;
  }
  client.disconnect(false);
}

export function parseInfoValue(info: string, field: string) {
  const line = info
    .split("\n")
    .map((item) => item.trim())
    .find((item) => item.startsWith(`${field}:`));
  if (!line) {
    throw new Error(`INFO не содержит поле ${field}`);
  }
  return line.slice(field.length + 1);
}

export async function readMaxmemory(client: Valkey) {
  const raw = parseInfoValue(await client.info("memory"), "maxmemory");
  const value = Number.parseInt(raw, 10);
  if (!Number.isSafeInteger(value)) {
    throw new Error(`Некорректное значение maxmemory: ${raw}`);
  }
  return value;
}

export function expectedMaxmemoryBytes(ramGb: number) {
  return (ramGb * 1024 * 1024 * 1024 * 3) / 4;
}

export async function canConnect(endpoint: ValkeyEndpoint) {
  let client: Valkey | undefined;
  try {
    client = await connectValkey(endpoint);
    await client.ping();
    return true;
  } catch {
    return false;
  } finally {
    if (client) {
      await disconnectValkey(client);
    }
  }
}

export async function writeAndRead(endpoint: ValkeyEndpoint, key: string, value: string) {
  const client = await connectValkey(endpoint);
  try {
    await client.set(key, value);
    return await client.get(key);
  } finally {
    await disconnectValkey(client);
  }
}
