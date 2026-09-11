import { act, renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { useValkeyCapacityPolling } from './valkey-capacity-polling';

function capacityResponse(usedVcpu: number) {
  return new Response(
    JSON.stringify({
      user: { limit: { vcpu: 4, ram_gb: 12 }, used: { vcpu: usedVcpu, ram_gb: 2 } },
      cluster: { limit: { vcpu: 12, ram_gb: 48 }, used: { vcpu: usedVcpu, ram_gb: 2 } },
      instances: { limit: 32, used: 1 },
    }),
    { headers: { 'Content-Type': 'application/json' } }
  );
}

afterEach(() => {
  vi.useRealTimers();
});

describe('опрос доступной ёмкости', () => {
  it('запускает следующий запрос через пять секунд от старта и не перекрывает медленный запрос', async () => {
    vi.useFakeTimers();
    const resolvers: Array<(response: Response) => void> = [];
    const fetchMock = vi.spyOn(window, 'fetch').mockImplementation(
      () =>
        new Promise<Response>((resolve) => {
          resolvers.push(resolve);
        })
    );
    const hook = renderHook(() => useValkeyCapacityPolling('first-account'));

    expect(fetchMock).toHaveBeenCalledTimes(1);
    void hook.result.current.refresh();
    await act(async () => vi.advanceTimersByTimeAsync(6_000));
    expect(fetchMock).toHaveBeenCalledTimes(1);

    await act(async () => {
      resolvers[0](capacityResponse(1));
      await Promise.resolve();
    });
    await act(async () => vi.advanceTimersByTimeAsync(0));
    expect(fetchMock).toHaveBeenCalledTimes(2);

    const secondSignal = fetchMock.mock.calls[1][1]?.signal;
    hook.unmount();
    expect(secondSignal?.aborted).toBe(true);
  });

  it('отменяет старый запрос при смене аккаунта и не принимает его поздний ответ', async () => {
    const resolvers: Array<(response: Response) => void> = [];
    const fetchMock = vi.spyOn(window, 'fetch').mockImplementation(
      () =>
        new Promise<Response>((resolve) => {
          resolvers.push(resolve);
        })
    );
    const hook = renderHook(
      ({ identity }: { identity: string }) => useValkeyCapacityPolling(identity),
      { initialProps: { identity: 'first-account' } }
    );
    const firstSignal = fetchMock.mock.calls[0][1]?.signal;

    hook.rerender({ identity: 'second-account' });
    expect(firstSignal?.aborted).toBe(true);
    expect(fetchMock).toHaveBeenCalledTimes(2);

    await act(async () => {
      resolvers[1](capacityResponse(2));
      await Promise.resolve();
    });
    expect(hook.result.current.capacity?.cluster.usage.vcpu).toBe(2);

    await act(async () => {
      resolvers[0](capacityResponse(9));
      await Promise.resolve();
    });
    expect(hook.result.current.capacity?.cluster.usage.vcpu).toBe(2);
    hook.unmount();
  });
});
