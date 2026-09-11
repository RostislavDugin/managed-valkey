import { execFile } from 'node:child_process';
import { fileURLToPath } from 'node:url';

export interface PrimaryProcess {
  ordinal: number;
  podUID: string;
  nodeName: string;
  nodeUID: string;
}

interface ValkeyResource {
  status?: {
    phase?: string;
    primaryOrdinal?: number;
    primaryPodUID?: string;
    primaryNodeName?: string;
    primaryNodeUID?: string;
    nodes?: Array<{
      ordinal: number;
      podUID: string;
      nodeName: string;
      nodeUID: string;
      readiness?: boolean;
      role?: string;
      termination?: unknown;
    }>;
  };
}

interface KubernetesNode {
  metadata: { name: string; uid: string };
  status?: { conditions?: Array<{ type: string; status: string }> };
}

export interface AgentContainer {
  id: string;
  project: string;
  service: string;
  running: boolean;
}

export type CommandRunner = (command: string, args: readonly string[]) => Promise<string>;

const runCommand: CommandRunner = (command, args) =>
  new Promise((resolve, reject) => {
    execFile(command, [...args], { encoding: 'utf8' }, (error, stdout, stderr) => {
      if (error) {
        reject(new Error(`${command} завершился с ошибкой: ${stderr.trim()}`));
        return;
      }
      resolve(stdout);
    });
  });

const testEnvironmentScript = fileURLToPath(
  new URL('../../../scripts/test_environment.sh', import.meta.url)
);

function requiredEnvironment(name: string) {
  const value = process.env[name];
  if (!value) {
    throw new Error(`${name} не задан`);
  }
  return value;
}

export function validateAgentTarget(
  nodeName: string,
  composeProject: string,
  candidates: readonly AgentContainer[]
) {
  if (nodeName === 'k3s-server') {
    throw new Error('k3s-server нельзя останавливать в сценарии потери worker-ноды');
  }
  if (!/^k3s-agent-[1-3]$/.test(nodeName)) {
    throw new Error(`Node ${nodeName} не является известным agent e2e-стенда`);
  }
  const matches = candidates.filter(
    (candidate) => candidate.project === composeProject && candidate.service === nodeName
  );
  if (matches.length !== 1) {
    throw new Error(`Для Node ${nodeName} найдено контейнеров e2e-стенда: ${matches.length}`);
  }
  if (!matches[0].running) {
    throw new Error(`Контейнер ${nodeName} уже остановлен`);
  }
  return matches[0];
}

async function waitUntil(
  predicate: () => Promise<boolean>,
  description: string,
  timeoutMs: number
) {
  const deadline = Date.now() + timeoutMs;
  while (!(await predicate())) {
    if (Date.now() >= deadline) {
      throw new Error(`Не удалось дождаться: ${description}`);
    }
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
}

export class WorkerFault {
  private restored = false;
  readonly container: AgentContainer;
  readonly node: KubernetesNode;
  private readonly controller: KubernetesController;

  constructor(container: AgentContainer, node: KubernetesNode, controller: KubernetesController) {
    this.container = container;
    this.node = node;
    this.controller = controller;
  }

  async restore() {
    if (this.restored) {
      return;
    }
    await this.controller.restoreWorker(this.container, this.node.metadata.name);
    this.restored = true;
  }
}

export class FaultRegistry {
  private readonly faults: WorkerFault[] = [];

  track(fault: WorkerFault) {
    this.faults.push(fault);
  }

  async restoreAll() {
    const failures: string[] = [];
    for (const fault of [...this.faults].reverse()) {
      try {
        await fault.restore();
      } catch {
        failures.push(fault.node.metadata.name);
      }
    }
    if (failures.length > 0) {
      throw new Error(`Не удалось восстановить agent e2e-стенда: ${failures.join(', ')}`);
    }
  }
}

export class KubernetesController {
  private readonly kubeconfig: string;
  private readonly composeProject: string;
  private readonly stateDir: string;
  private readonly faults: FaultRegistry;
  private readonly command: CommandRunner;

  constructor(faults: FaultRegistry, command: CommandRunner = runCommand) {
    this.faults = faults;
    this.command = command;
    this.kubeconfig = requiredEnvironment('MANAGED_VALKEY_E2E_ADMIN_KUBECONFIG');
    this.composeProject = requiredEnvironment('MANAGED_VALKEY_E2E_COMPOSE_PROJECT');
    this.stateDir = requiredEnvironment('MANAGED_VALKEY_E2E_STATE_DIR');
  }

  async readPrimary(slug: string) {
    const resource = await this.readResource(slug);
    const status = resource.status;
    if (
      status?.primaryOrdinal === undefined ||
      !status.primaryPodUID ||
      !status.primaryNodeName ||
      !status.primaryNodeUID
    ) {
      throw new Error(`ValkeyInstance ${slug} не содержит свежую identity primary`);
    }
    const process = status.nodes?.find(
      (candidate) =>
        candidate.ordinal === status.primaryOrdinal && candidate.podUID === status.primaryPodUID
    );
    if (!process || process.role !== 'primary') {
      throw new Error(`ValkeyInstance ${slug} не подтверждает primary в status.nodes`);
    }
    return {
      ordinal: status.primaryOrdinal,
      podUID: status.primaryPodUID,
      nodeName: status.primaryNodeName,
      nodeUID: status.primaryNodeUID,
    } satisfies PrimaryProcess;
  }

  async deletePrimaryPod(slug: string) {
    const primary = await this.readPrimary(slug);
    const namespace = `valkey-${slug}`;
    const podName = `${slug}-${primary.ordinal}`;
    const pod = JSON.parse(
      await this.kubectl(['-n', namespace, 'get', 'pod', podName, '-o', 'json'])
    ) as { metadata?: { uid?: string } };
    if (pod.metadata?.uid !== primary.podUID) {
      throw new Error(`Pod ${podName} больше не совпадает со свежим primary`);
    }
    await this.kubectl(['-n', namespace, 'delete', 'pod', podName, '--wait=false']);
    return primary;
  }

  async ensurePrimaryOnAgent(slug: string) {
    let primary = await this.readPrimary(slug);
    if (primary.nodeName === 'k3s-server') {
      const previousUID = primary.podUID;
      await this.deletePrimaryPod(slug);
      await waitUntil(
        async () => {
          try {
            primary = await this.readPrimary(slug);
            return primary.podUID !== previousUID && primary.nodeName !== 'k3s-server';
          } catch {
            return false;
          }
        },
        `primary ${slug} на agent`,
        2 * 60_000
      );
    }
    if (primary.nodeName === 'k3s-server') {
      throw new Error(`Primary ${slug} остался на k3s-server`);
    }
    return primary;
  }

  async stopAndDeletePrimaryWorker(slug: string) {
    const primary = await this.ensurePrimaryOnAgent(slug);
    const node = await this.readNode(primary.nodeName);
    if (node.metadata.uid !== primary.nodeUID) {
      throw new Error(`UID Node ${primary.nodeName} изменился после наблюдения primary`);
    }
    const candidates = await this.agentContainers(primary.nodeName);
    const container = validateAgentTarget(primary.nodeName, this.composeProject, candidates);
    const fault = new WorkerFault(container, node, this);
    this.faults.track(fault);

    await this.command('docker', ['stop', '--time', '20', container.id]);
    const stopped = await this.inspectContainer(container.id);
    if (stopped.running) {
      throw new Error(`Контейнер ${primary.nodeName} не остановился`);
    }
    const currentNode = await this.readNode(primary.nodeName);
    if (currentNode.metadata.uid !== node.metadata.uid) {
      throw new Error(`UID Node ${primary.nodeName} изменился перед удалением`);
    }
    await this.kubectl(['delete', 'node', primary.nodeName, '--wait=false']);
    return fault;
  }

  async waitForHealthyHA(slug: string) {
    await waitUntil(
      async () => {
        try {
          const status = (await this.readResource(slug)).status;
          return Boolean(
            status?.phase === 'running' &&
            status.nodes?.length === 3 &&
            status.nodes.every(
              (process) => process.readiness && !process.termination && process.role
            )
          );
        } catch {
          return false;
        }
      },
      `HA ${slug} в running с тремя готовыми процессами`,
      5 * 60_000
    );
  }

  async waitForDegradedHAOnTwoNodes(slug: string) {
    await waitUntil(
      async () => {
        try {
          const status = (await this.readResource(slug)).status;
          const ready =
            status?.nodes?.filter((process) => process.readiness && !process.termination).length ??
            0;
          return status?.phase === 'degraded' && ready === 2;
        } catch {
          return false;
        }
      },
      `HA ${slug} в degraded с двумя готовыми процессами`,
      2 * 60_000
    );
  }

  async restoreWorker(container: AgentContainer, nodeName: string) {
    const current = await this.inspectContainer(container.id);
    if (current.project !== this.composeProject || current.service !== nodeName) {
      throw new Error(`Контейнер ${container.id} больше не принадлежит ${this.composeProject}`);
    }
    if (!current.running) {
      await this.command('docker', ['start', container.id]);
    }
    await waitUntil(
      async () => {
        try {
          const node = await this.readNode(nodeName);
          return Boolean(
            node.status?.conditions?.some(
              (condition) => condition.type === 'Ready' && condition.status === 'True'
            )
          );
        } catch {
          return false;
        }
      },
      `Node ${nodeName} в Ready после восстановления`,
      3 * 60_000
    );
    await this.command(testEnvironmentScript, ['refresh-node-route', this.stateDir, nodeName]);
  }

  private async readResource(slug: string) {
    return JSON.parse(
      await this.kubectl([
        '-n',
        `valkey-${slug}`,
        'get',
        'valkeyinstances.valkey.h3llo-demo.com',
        slug,
        '-o',
        'json',
      ])
    ) as ValkeyResource;
  }

  private async readNode(nodeName: string) {
    return JSON.parse(
      await this.kubectl(['get', 'node', nodeName, '-o', 'json'])
    ) as KubernetesNode;
  }

  private kubectl(args: readonly string[]) {
    return this.command('kubectl', ['--kubeconfig', this.kubeconfig, ...args]);
  }

  private async agentContainers(nodeName: string) {
    const identifiers = (
      await this.command('docker', [
        'ps',
        '--filter',
        `label=com.docker.compose.project=${this.composeProject}`,
        '--filter',
        `label=com.docker.compose.service=${nodeName}`,
        '--format',
        '{{.ID}}',
      ])
    )
      .split('\n')
      .map((value) => value.trim())
      .filter(Boolean);
    return Promise.all(identifiers.map((identifier) => this.inspectContainer(identifier)));
  }

  private async inspectContainer(identifier: string) {
    const inspected = JSON.parse(await this.command('docker', ['inspect', identifier])) as Array<{
      Id: string;
      Config?: { Labels?: Record<string, string> };
      State?: { Running?: boolean };
    }>;
    if (inspected.length !== 1) {
      throw new Error(`docker inspect вернул ${inspected.length} контейнеров для ${identifier}`);
    }
    const item = inspected[0];
    return {
      id: item.Id,
      project: item.Config?.Labels?.['com.docker.compose.project'] ?? '',
      service: item.Config?.Labels?.['com.docker.compose.service'] ?? '',
      running: item.State?.Running === true,
    } satisfies AgentContainer;
  }
}
