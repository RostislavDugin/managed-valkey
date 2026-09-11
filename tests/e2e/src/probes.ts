export interface ProbeClock {
  now: () => number;
  sleep: (milliseconds: number) => Promise<void>;
}

export interface ProbeResult {
  writeFailures: number[];
  writeSuccesses: number[];
  readFailures: number[];
  readSuccesses: number[];
}

export interface FailureProbes {
  result: ProbeResult;
  stop: () => Promise<void>;
}

const systemClock: ProbeClock = {
  now: Date.now,
  sleep: (milliseconds) => new Promise((resolve) => setTimeout(resolve, milliseconds)),
};

export function startFailureProbes(
  write: () => Promise<unknown>,
  read: () => Promise<unknown>,
  options: { intervalMs?: number; clock?: ProbeClock } = {}
): FailureProbes {
  const intervalMs = options.intervalMs ?? 100;
  const clock = options.clock ?? systemClock;
  const result: ProbeResult = {
    writeFailures: [],
    writeSuccesses: [],
    readFailures: [],
    readSuccesses: [],
  };
  let stopped = false;

  const loop = async (
    operation: () => Promise<unknown>,
    successes: number[],
    failures: number[]
  ) => {
    while (!stopped) {
      try {
        await operation();
        successes.push(clock.now());
      } catch {
        failures.push(clock.now());
      }
      if (!stopped) {
        await clock.sleep(intervalMs);
      }
    }
  };

  const running = [
    loop(write, result.writeSuccesses, result.writeFailures),
    loop(read, result.readSuccesses, result.readFailures),
  ];

  return {
    result,
    stop: async () => {
      stopped = true;
      await Promise.all(running);
    },
  };
}

export function maximumSuccessGap(
  successes: readonly number[],
  startedAt: number,
  finishedAt: number
) {
  const within = successes
    .filter((value) => value >= startedAt && value <= finishedAt)
    .sort((left, right) => left - right);
  const points = [startedAt, ...within, finishedAt];
  let maximum = 0;
  for (let index = 1; index < points.length; index += 1) {
    maximum = Math.max(maximum, points[index] - points[index - 1]);
  }
  return maximum;
}

export async function waitForProbe(
  predicate: () => boolean,
  description: string,
  timeoutMs: number,
  intervalMs = 25
) {
  const deadline = Date.now() + timeoutMs;
  while (!predicate()) {
    if (Date.now() >= deadline) {
      throw new Error(`Не удалось дождаться: ${description}`);
    }
    await new Promise((resolve) => setTimeout(resolve, intervalMs));
  }
}
