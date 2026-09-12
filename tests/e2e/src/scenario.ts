import { randomUUID } from "node:crypto";
import { expect } from "@playwright/test";
import type { Valkey } from "iovalkey";
import type { ValkeySize } from "./console.ts";
import {
  canConnect,
  connectValkey,
  disconnectValkey,
  expectedMaxmemoryBytes,
  readMaxmemory,
  type ValkeyEndpoint,
} from "./valkey.ts";

export function scenarioIdentity(prefix: string) {
  const suffix = randomUUID().slice(0, 6);
  return {
    name: `${prefix}-${suffix}`.slice(0, 40),
    prefix: `${prefix}-${suffix}`.slice(0, 20),
  };
}

export function sameSize(left: ValkeySize, right: ValkeySize) {
  return left.vcpu === right.vcpu && left.ramGb === right.ramGb;
}

export function minimumSize(sizes: readonly ValkeySize[]) {
  const size = sizes[0];
  if (!size) {
    throw new Error("Каталог Valkey пуст");
  }
  return size;
}

export function nextSize(sizes: readonly ValkeySize[], current: ValkeySize) {
  const size = sizes.find(
    (candidate) =>
      candidate.vcpu >= current.vcpu &&
      candidate.ramGb >= current.ramGb &&
      !sameSize(candidate, current),
  );
  if (!size) {
    throw new Error("В каталоге нет следующей конфигурации для проверки изменения размера");
  }
  return size;
}

export async function assertValkeyReadWrite(
  endpoint: ValkeyEndpoint,
  keyPrefix: string,
  expectedSize: ValkeySize,
) {
  await expect(async () => {
    const client = await connectValkey(endpoint);
    try {
      const key = `${keyPrefix}:${randomUUID()}`;
      const value = randomUUID();
      await client.set(key, value);
      expect(await client.get(key)).toBe(value);
      expect(await readMaxmemory(client)).toBe(expectedMaxmemoryBytes(expectedSize.ramGb));
      expect(await client.info()).toContain("redis_version");
    } finally {
      await disconnectValkey(client);
    }
  }).toPass({ timeout: 5 * 60_000, intervals: [100, 250, 500, 1_000] });
}

export async function assertValkeyReadOnly(
  primary: ValkeyEndpoint,
  readOnly: ValkeyEndpoint,
  keyPrefix: string,
  expectedSize: ValkeySize,
) {
  await expect(async () => {
    const primaryClient = await connectValkey(primary);
    const readOnlyClient = await connectValkey(readOnly);
    try {
      const key = `${keyPrefix}:${randomUUID()}`;
      const value = randomUUID();
      await primaryClient.set(key, value);
      await expect.poll(() => readOnlyClient.get(key), { timeout: 30_000 }).toBe(value);
      expect(await primaryClient.get(key)).toBe(value);
      await expect(readOnlyClient.set(`${key}:readonly`, value)).rejects.toThrow(/READONLY/i);
      expect(await readMaxmemory(primaryClient)).toBe(expectedMaxmemoryBytes(expectedSize.ramGb));
      expect(await readMaxmemory(readOnlyClient)).toBe(expectedMaxmemoryBytes(expectedSize.ramGb));
    } finally {
      await Promise.all([disconnectValkey(primaryClient), disconnectValkey(readOnlyClient)]);
    }
  }).toPass({ timeout: 5 * 60_000, intervals: [100, 250, 500, 1_000] });
}

export async function assertValkeySharedPrimary(
  primary: ValkeyEndpoint,
  readOnly: ValkeyEndpoint,
  keyPrefix: string,
  expectedSize: ValkeySize,
) {
  await expect(async () => {
    const primaryClient = await connectValkey(primary);
    const readOnlyClient = await connectValkey(readOnly);
    try {
      const key = `${keyPrefix}:${randomUUID()}`;
      const value = randomUUID();
      await primaryClient.set(key, value);
      await expect.poll(() => readOnlyClient.get(key), { timeout: 30_000 }).toBe(value);
      await readOnlyClient.set(`${key}:read-address`, value);
      expect(await primaryClient.get(`${key}:read-address`)).toBe(value);
      expect(await readMaxmemory(primaryClient)).toBe(expectedMaxmemoryBytes(expectedSize.ramGb));
      expect(await readMaxmemory(readOnlyClient)).toBe(expectedMaxmemoryBytes(expectedSize.ramGb));
    } finally {
      await Promise.all([disconnectValkey(primaryClient), disconnectValkey(readOnlyClient)]);
    }
  }).toPass({ timeout: 5 * 60_000, intervals: [100, 250, 500, 1_000] });
}

export async function waitForConnectionClose(client: Valkey, timeoutMs = 5 * 60_000) {
  if (client.status !== "ready") {
    return;
  }
  await new Promise<void>((resolve, reject) => {
    const timeout = setTimeout(
      () => reject(new Error("Прежнее соединение Valkey не закрылось")),
      timeoutMs,
    );
    client.once("close", () => {
      clearTimeout(timeout);
      resolve();
    });
  });
}

export async function waitForRejectedPassword(endpoint: ValkeyEndpoint, timeoutMs = 5 * 60_000) {
  await expect
    .poll(() => canConnect(endpoint), { timeout: timeoutMs, intervals: [100, 250, 500, 1_000] })
    .toBe(false);
}
