import {
  assertReadOnlyContinuity,
  startValkeyFailureScenario,
  waitForWriteFailureAndRecovery,
} from '../src/fault-scenario.ts';
import { expect, test } from '../src/fixtures.ts';
import { assertValkeyReadOnly, minimumSize, scenarioIdentity } from '../src/scenario.ts';

const personalQuota = { vcpu: 4, ramGb: 12 };

test(
  'HA-инстанс восстанавливается после потери worker-ноды через прежний адрес',
  { tag: '@local-k8s' },
  async ({ console, kubernetes }) => {
    const account = await console.register('worker-node-loss');
    await console.openCreatePage();
    const sizes = (await console.readSizeCatalog()).filter(
      (size) => size.vcpu * 3 <= personalQuota.vcpu && size.ramGb * 3 <= personalQuota.ramGb
    );
    const size = minimumSize(sizes);
    const created = await console.submitCreation(account, {
      mode: 'ha',
      size,
      ...scenarioIdentity('worker-loss'),
    });
    await console.closePasswordWindow();
    await console.waitForRunningSize(size);
    const connection = await console.readConnection(created.password, true);
    if (!connection.readOnly) {
      throw new Error('Интерфейс HA не показал адрес только для чтения');
    }

    await kubernetes.ensurePrimaryOnAgent(created.slug);
    await kubernetes.waitForHealthyHA(created.slug);
    const failure = await startValkeyFailureScenario(connection);
    try {
      const faultStartedAt = Date.now();
      const fault = await kubernetes.stopAndDeletePrimaryWorker(created.slug);
      const recoveredAt = await waitForWriteFailureAndRecovery(failure, faultStartedAt);
      expect(failure.primaryCloseEvents()).toBeGreaterThan(0);
      expect(recoveredAt - faultStartedAt).toBeLessThanOrEqual(2 * 60_000);
      await kubernetes.waitForHealthyHAOnTwoNodes(created.slug);
      await fault.restore();
      await kubernetes.waitForHealthyHA(created.slug);
      assertReadOnlyContinuity(failure, faultStartedAt, Date.now());
    } finally {
      await failure.stop();
    }

    await console.waitForRunningSize(size);
    await assertValkeyReadOnly(
      connection.primary,
      connection.readOnly,
      'worker-loss-recovered',
      size
    );
    await console.deleteInstance(account, created);
    await console.waitForEmptyManagement();
  }
);
