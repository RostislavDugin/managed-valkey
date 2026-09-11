import assert from 'node:assert/strict';
import test from 'node:test';
import { maximumSuccessGap, startFailureProbes } from '../src/probes.ts';

test('фоновые пробы фиксируют ошибку записи и завершаются по stop', async () => {
  let writes = 0;
  let reads = 0;
  const probes = startFailureProbes(
    async () => {
      writes += 1;
      if (writes === 1) {
        throw new Error('write unavailable');
      }
    },
    async () => {
      reads += 1;
    },
    { intervalMs: 1 }
  );

  await new Promise((resolve) => setTimeout(resolve, 15));
  await probes.stop();
  const countsAfterStop = { writes, reads };
  await new Promise((resolve) => setTimeout(resolve, 5));

  assert.equal(probes.result.writeFailures.length, 1);
  assert.ok(probes.result.writeSuccesses.length > 0);
  assert.ok(probes.result.readSuccesses.length > 0);
  assert.deepEqual({ writes, reads }, countsAfterStop);
});

test('максимальный промежуток учитывает границы периода наблюдения', () => {
  assert.equal(maximumSuccessGap([110, 140, 170], 100, 200), 30);
  assert.equal(maximumSuccessGap([], 100, 200), 100);
});
