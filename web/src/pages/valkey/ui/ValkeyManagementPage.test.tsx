import { act, screen, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderValkeySection, seedSession } from '../../../../test/render';
import {
  installStatefulValkeyApi,
  jsonResponse,
  valkeyInstanceDto,
} from '../../../../test/valkey-api-fixture';

const WAIT = { timeout: 10_000 };

afterEach(() => {
  vi.useRealTimers();
});

describe('список баз Valkey', () => {
  it('при отсутствии баз показывает нулевое использование квоты в правой колонке', async () => {
    const session = seedSession();
    installStatefulValkeyApi([]);

    renderValkeySection('/valkey/management', session);

    expect(
      await screen.findByRole('heading', { name: 'Управляемые базы Valkey на DDR5' }, WAIT)
    ).toBeVisible();
    const aside = screen.getByTestId('console-aside');
    expect(within(aside).getByRole('heading', { name: 'Квота' })).toBeVisible();
    expect(within(aside).getByRole('progressbar', { name: 'vCPU: занято 0 / 8' })).toBeVisible();
    expect(
      within(aside).getByRole('progressbar', { name: /RAM: занято 0\sГБ \/ 32\sГБ/ })
    ).toBeVisible();
    expect(within(aside).getByRole('link', { name: 'Увеличить через поддержку' })).toBeVisible();
  });

  it('при повторном открытии страницы загружает сохранённые базы с сервера и показывает их в таблице', async () => {
    const session = seedSession();
    installStatefulValkeyApi([valkeyInstanceDto({ name: 'persistent-cache' })]);

    const first = renderValkeySection('/valkey/management', session);
    expect(await screen.findByText('persistent-cache', {}, WAIT)).toBeVisible();
    first.unmount();

    renderValkeySection('/valkey/management', session);

    expect(await screen.findByText('persistent-cache', {}, WAIT)).toBeVisible();
  });

  it('при периодическом обновлении не перекрывает запросы и отсчитывает следующий период от начала предыдущего', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const session = seedSession();
    installStatefulValkeyApi();
    const statefulFetch = window.fetch;
    let listCalls = 0;
    let resolveSecond: (response: Response) => void = () => undefined;
    const secondResponse = new Promise<Response>((resolve) => {
      resolveSecond = resolve;
    });
    window.fetch = vi.fn(async (input, init) => {
      if (String(input) === '/v1/managed/valkey/instances') {
        listCalls += 1;
        if (listCalls === 2) {
          return secondResponse;
        }
      }
      return statefulFetch(input, init);
    });
    globalThis.fetch = window.fetch;

    renderValkeySection('/valkey/management', session);
    await vi.waitFor(() => expect(screen.getByRole('table')).toBeVisible());

    await vi.advanceTimersByTimeAsync(5_000);
    await vi.waitFor(() => expect(listCalls).toBe(2));
    await vi.advanceTimersByTimeAsync(5_000);
    expect(listCalls).toBe(2);

    resolveSecond(jsonResponse({ items: [valkeyInstanceDto()] }));
    await act(async () => {
      await secondResponse;
    });
    await vi.advanceTimersByTimeAsync(0);

    expect(listCalls).toBe(3);
  });

  it('после ухода со страницы отменяет запрос списка и игнорирует его поздний ответ', async () => {
    const session = seedSession();
    installStatefulValkeyApi([]);
    const statefulFetch = window.fetch;
    let resolveList: (response: Response) => void = () => undefined;
    const lateResponse = new Promise<Response>((resolve) => {
      resolveList = resolve;
    });
    const firstSignals: AbortSignal[] = [];
    let listCalls = 0;
    window.fetch = vi.fn(async (input, init) => {
      if (String(input) === '/v1/managed/valkey/instances') {
        listCalls += 1;
        if (listCalls === 1) {
          if (init?.signal instanceof AbortSignal) {
            firstSignals.push(init.signal);
          }
          return lateResponse;
        }
      }
      return statefulFetch(input, init);
    });
    globalThis.fetch = window.fetch;

    const { router } = renderValkeySection('/valkey/management', session);
    await vi.waitFor(() => expect(listCalls).toBe(1));
    await router.navigate('/valkey/management/new');
    expect(await screen.findByRole('heading', { name: 'Новая Valkey база' }, WAIT)).toBeVisible();
    expect(firstSignals[0]?.aborted).toBe(true);

    resolveList(jsonResponse({ items: [valkeyInstanceDto({ name: 'late-foreign-cache' })] }));
    await act(async () => {
      await lateResponse;
    });

    expect(screen.queryByText('late-foreign-cache')).not.toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Новая Valkey база' })).toBeVisible();
  });

  it('после смены аккаунта отменяет прежний запрос списка и не показывает базы предыдущего пользователя', async () => {
    const session = seedSession();
    installStatefulValkeyApi([]);
    const statefulFetch = window.fetch;
    let resolveFirstList: (response: Response) => void = () => undefined;
    const firstResponse = new Promise<Response>((resolve) => {
      resolveFirstList = resolve;
    });
    const firstSignals: AbortSignal[] = [];
    let listCalls = 0;
    window.fetch = vi.fn(async (input, init) => {
      if (String(input) === '/v1/managed/valkey/instances') {
        listCalls += 1;
        if (listCalls === 1) {
          if (init?.signal instanceof AbortSignal) {
            firstSignals.push(init.signal);
          }
          return firstResponse;
        }
        return jsonResponse({ items: [valkeyInstanceDto({ name: 'second-account-cache' })] });
      }
      return statefulFetch(input, init);
    });
    globalThis.fetch = window.fetch;

    const view = renderValkeySection('/valkey/management', session);
    await vi.waitFor(() => expect(listCalls).toBe(1));
    act(() => {
      view.setSession({
        ...session,
        userId: '01930000-0000-7000-8000-000000000099',
        email: 'second@example.com',
      });
    });

    expect(await screen.findByText('second-account-cache', {}, WAIT)).toBeVisible();
    expect(firstSignals[0]?.aborted).toBe(true);
    resolveFirstList(jsonResponse({ items: [valkeyInstanceDto({ name: 'first-account-cache' })] }));
    await act(async () => {
      await firstResponse;
    });

    expect(screen.queryByText('first-account-cache')).not.toBeInTheDocument();
    expect(screen.getByText('second-account-cache')).toBeVisible();
  });
});
