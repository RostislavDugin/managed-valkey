import assert from 'node:assert/strict';
import test from 'node:test';
import { validateAgentTarget, type AgentContainer } from '../src/kubernetes.ts';

const project = 'managed-valkey-e2e-run';
const owned: AgentContainer = {
  id: 'container-agent-1',
  project,
  service: 'k3s-agent-1',
  running: true,
};

test('Kubernetes-помощник выбирает единственный работающий agent своего Compose project', () => {
  assert.equal(validateAgentTarget('k3s-agent-1', project, [owned]), owned);
});

test('Kubernetes-помощник не разрешает остановить server', () => {
  assert.throws(() => validateAgentTarget('k3s-server', project, [owned]), /нельзя останавливать/);
});

test('Kubernetes-помощник отклоняет неизвестный agent', () => {
  assert.throws(() => validateAgentTarget('worker-a', project, [owned]), /не является известным/);
});

test('Kubernetes-помощник отклоняет контейнер чужого Compose project', () => {
  assert.throws(
    () => validateAgentTarget('k3s-agent-1', project, [{ ...owned, project: 'foreign' }]),
    /найдено контейнеров.*0/
  );
});

test('Kubernetes-помощник отклоняет остановленный контейнер', () => {
  assert.throws(
    () => validateAgentTarget('k3s-agent-1', project, [{ ...owned, running: false }]),
    /уже остановлен/
  );
});
