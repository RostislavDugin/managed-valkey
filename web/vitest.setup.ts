import '@testing-library/jest-dom/vitest';
import { cleanup } from '@testing-library/react';
import { afterEach, beforeEach, vi } from 'vitest';

class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}

// Mantine измеряет элементы через ResizeObserver, а happy-dom его не даёт.
globalThis.ResizeObserver ??= ResizeObserverStub as unknown as typeof ResizeObserver;
Element.prototype.scrollIntoView ??= () => {};

beforeEach(() => {
  localStorage.clear();
  window.fetch = vi.fn(async (input) => {
    const body = String(input).endsWith('/managed/valkey/capacity')
      ? {
          user: { limit: { vcpu: 4, ram_gb: 12 }, used: { vcpu: 0, ram_gb: 0 } },
          cluster: { limit: { vcpu: 12, ram_gb: 48 }, used: { vcpu: 0, ram_gb: 0 } },
          instances: { limit: 32, used: 0 },
        }
      : {
          user: { id: '01930000-0000-7000-8000-000000000001', email: 'user@example.com' },
          quota: { max_vcpu: 4, max_ram_gb: 12 },
          usage: { used_vcpu: 0, used_ram_gb: 0 },
        };
    return new Response(JSON.stringify(body), {
      status: 200,
      headers: { 'Content-Type': 'application/json' },
    });
  });
  globalThis.fetch = window.fetch;
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});
