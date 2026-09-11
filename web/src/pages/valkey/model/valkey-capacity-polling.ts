import { useCallback, useEffect, useRef, useState } from 'react';
import { getValkeyCapacity } from '../api/valkey-api';
import type { ValkeyCapacity } from './quota';

const CAPACITY_POLL_INTERVAL_MS = 5_000;

export function useValkeyCapacityPolling(identity: string | null) {
  const [capacity, setCapacity] = useState<ValkeyCapacity | null>(null);
  const [error, setError] = useState<unknown>(null);
  const refreshRef = useRef<() => Promise<ValkeyCapacity | undefined>>(async () => undefined);

  useEffect(() => {
    let stopped = false;
    let timer: number | undefined;
    let controller: AbortController | null = null;
    let active: Promise<ValkeyCapacity | undefined> | null = null;

    setCapacity(null);
    setError(null);

    if (!identity) {
      refreshRef.current = async () => undefined;
      return;
    }

    const schedule = (startedAt: number) => {
      if (stopped) {
        return;
      }
      const delay = Math.max(0, CAPACITY_POLL_INTERVAL_MS - (Date.now() - startedAt));
      timer = window.setTimeout(() => void run(), delay);
    };

    const run = () => {
      if (stopped) {
        return Promise.resolve(undefined);
      }
      if (active) {
        return active;
      }

      if (timer !== undefined) {
        window.clearTimeout(timer);
        timer = undefined;
      }
      const startedAt = Date.now();
      controller = new AbortController();
      const currentController = controller;
      const request = (async () => {
        try {
          const next = await getValkeyCapacity(currentController.signal);
          if (!stopped && controller === currentController) {
            setCapacity(next);
            setError(null);
          }
          return next;
        } catch (requestError) {
          if (
            !stopped &&
            controller === currentController &&
            !(requestError instanceof DOMException && requestError.name === 'AbortError')
          ) {
            setError(requestError);
          }
          return undefined;
        } finally {
          if (controller === currentController) {
            controller = null;
          }
          active = null;
          schedule(startedAt);
        }
      })();
      active = request;
      return request;
    };

    refreshRef.current = run;
    void run();

    return () => {
      stopped = true;
      if (timer !== undefined) {
        window.clearTimeout(timer);
      }
      controller?.abort();
      refreshRef.current = async () => undefined;
    };
  }, [identity]);

  const refresh = useCallback(() => refreshRef.current(), []);
  return { capacity, error, refresh };
}
