import { randomUUID } from 'node:crypto';
import { expect } from '@playwright/test';
import { maximumSuccessGap, startFailureProbes, waitForProbe } from './probes.ts';
import { connectValkey, disconnectValkey, type ValkeyInstanceConnection } from './valkey.ts';

export async function startValkeyFailureScenario(connection: ValkeyInstanceConnection) {
  if (!connection.readOnly) {
    throw new Error('Для аварийного HA-сценария нужен адрес только для чтения');
  }
  const primary = await connectValkey(connection.primary, true);
  let pendingReadOnly: Awaited<ReturnType<typeof connectValkey>> | undefined;
  let pendingProbes: ReturnType<typeof startFailureProbes> | undefined;
  try {
    const readOnly = await connectValkey(connection.readOnly, true);
    pendingReadOnly = readOnly;
    const controlKey = `e2e:fault:control:${randomUUID()}`;
    const controlValue = randomUUID();
    let primaryCloseEvents = 0;
    primary.on('close', () => {
      primaryCloseEvents += 1;
    });
    await primary.set(controlKey, controlValue);
    await expect.poll(() => readOnly.get(controlKey), { timeout: 30_000 }).toBe(controlValue);

    let writeSequence = 0;
    const probes = startFailureProbes(
      async () => {
        writeSequence += 1;
        await primary.set(`e2e:fault:write:${writeSequence}`, String(writeSequence));
      },
      async () => {
        const value = await readOnly.get(controlKey);
        if (value !== controlValue) {
          throw new Error('RO-маршрут не вернул контрольный ключ');
        }
      }
    );
    pendingProbes = probes;
    await waitForProbe(
      () => probes.result.writeSuccesses.length > 0 && probes.result.readSuccesses.length > 0,
      'первые успешные пробы записи и RO-чтения',
      30_000
    );

    return {
      controlKey,
      controlValue,
      primary,
      readOnly,
      probes,
      primaryCloseEvents: () => primaryCloseEvents,
      stop: async () => {
        await probes.stop();
        await Promise.all([disconnectValkey(primary), disconnectValkey(readOnly)]);
      },
    };
  } catch (error) {
    await pendingProbes?.stop();
    await Promise.all([
      disconnectValkey(primary),
      ...(pendingReadOnly ? [disconnectValkey(pendingReadOnly)] : []),
    ]);
    throw error;
  }
}

export async function waitForWriteFailureAndRecovery(
  scenario: Awaited<ReturnType<typeof startValkeyFailureScenario>>,
  faultStartedAt: number
) {
  await waitForProbe(
    () => scenario.probes.result.writeFailures.some((value) => value >= faultStartedAt),
    'хотя бы один временный отказ записи',
    2 * 60_000
  );
  const firstFailure = scenario.probes.result.writeFailures.find(
    (value) => value >= faultStartedAt
  );
  if (firstFailure === undefined) {
    throw new Error('Проба не сохранила время отказа записи');
  }
  await waitForProbe(
    () => scenario.probes.result.writeSuccesses.some((value) => value > firstFailure),
    'восстановление записи через прежний адрес',
    2 * 60_000
  );
  const recoveredAt = scenario.probes.result.writeSuccesses.find((value) => value > firstFailure);
  if (recoveredAt === undefined || recoveredAt - faultStartedAt > 2 * 60_000) {
    throw new Error('Запись не восстановилась за две минуты после внесения отказа');
  }
  return recoveredAt;
}

export function assertReadOnlyContinuity(
  scenario: Awaited<ReturnType<typeof startValkeyFailureScenario>>,
  faultStartedAt: number,
  recoveredAt: number
) {
  expect(
    maximumSuccessGap(scenario.probes.result.readSuccesses, faultStartedAt, recoveredAt)
  ).toBeLessThanOrEqual(5_000);
}
