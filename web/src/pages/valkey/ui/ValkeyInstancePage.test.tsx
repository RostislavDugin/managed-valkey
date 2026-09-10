import { act, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderValkeySection, seedSession } from '../../../../test/render';
import {
  installStatefulValkeyApi,
  jsonResponse,
  TEST_INSTANCE_ID,
  valkeyInstanceDto,
} from '../../../../test/valkey-api-fixture';

const WAIT = { timeout: 10_000 };
const instancePath = `/valkey/management/${TEST_INSTANCE_ID}`;

afterEach(() => {
  vi.useRealTimers();
});

describe('управление базой Valkey', () => {
  it('при сохранении имени и окна обслуживания отправляет один PATCH и повторно загружает карточку базы', async () => {
    const session = seedSession();
    const api = installStatefulValkeyApi();
    const user = userEvent.setup();

    renderValkeySection(instancePath, session);
    expect(await screen.findByRole('heading', { name: 'cache' }, WAIT)).toBeVisible();
    await user.click(screen.getByRole('button', { name: 'Изменить имя' }));

    const name = screen.getByRole('textbox', { name: 'Имя базы' });
    await user.clear(name);
    await user.type(name, 'cache-renamed');
    await user.click(screen.getByRole('switch', { name: 'Задать окно обслуживания' }));

    const day = screen.getByRole('textbox', { name: 'День недели, 0–6' });
    const hour = screen.getByRole('textbox', { name: 'Час UTC, 0–23' });
    const duration = screen.getByRole('textbox', { name: 'Длительность, минуты' });
    await user.clear(day);
    await user.type(day, '2');
    await user.clear(hour);
    await user.type(hour, '3');
    await user.clear(duration);
    await user.type(duration, '60');
    await user.click(screen.getByRole('button', { name: 'Сохранить' }));

    await waitFor(() =>
      expect(api.requests.find((request) => request.method === 'PATCH')?.body).toEqual({
        name: 'cache-renamed',
        maintenance: { dow: 2, hour_utc: 3, duration_min: 60 },
      })
    );
    expect(await screen.findByRole('heading', { name: 'cache-renamed' }, WAIT)).toBeVisible();
    await waitFor(() =>
      expect(
        api.requests.filter(
          (request) => request.method === 'GET' && request.path.endsWith(TEST_INSTANCE_ID)
        ).length
      ).toBeGreaterThan(1)
    );
    expect(api.requests.filter((request) => request.path === '/v1/me').length).toBeGreaterThan(1);
  });

  it('при сохранении списка доступа преобразует одиночный IP-адрес в канонический CIDR и отправляет его серверу', async () => {
    const session = seedSession();
    const api = installStatefulValkeyApi();
    const user = userEvent.setup();

    renderValkeySection(instancePath, session);
    await screen.findByRole('heading', { name: 'cache' }, WAIT);
    await user.click(screen.getByRole('button', { name: 'Изменить доступ по IP' }));
    await user.click(screen.getByRole('switch', { name: 'Ограничить доступ по IP-адресам' }));
    await user.type(
      screen.getByRole('textbox', { name: 'Разрешённые IPv4-адреса и CIDR' }),
      '203.0.113.10'
    );
    await user.click(screen.getByRole('button', { name: 'Сохранить' }));

    await waitFor(() =>
      expect(api.requests.find((request) => request.method === 'PUT')?.body).toEqual({
        is_whitelist_enabled: true,
        whitelist_cidrs: ['203.0.113.10/32'],
      })
    );
    expect(await screen.findByText('203.0.113.10/32', {}, WAIT)).toBeVisible();
  });

  it('при ротации создаёт пароль только после подтверждения и очищает его после события pagehide', async () => {
    const session = seedSession();
    const api = installStatefulValkeyApi();
    const user = userEvent.setup();

    renderValkeySection(instancePath, session);
    await screen.findByRole('heading', { name: 'cache' }, WAIT);
    const rotateAction = await screen.findByRole('button', { name: 'Сменить пароль' }, WAIT);
    await user.click(rotateAction);
    expect(api.requests.some((request) => request.path.endsWith('/credentials/rotate'))).toBe(
      false
    );

    await user.click(screen.getByRole('button', { name: 'Подтвердить смену пароля' }));

    const passwordInput = await screen.findByRole('textbox', { name: 'Новый пароль' }, WAIT);
    const rotateRequest = api.requests.find((request) =>
      request.path.endsWith('/credentials/rotate')
    );
    const password = String(rotateRequest?.body?.password);
    expect(rotateRequest?.body).toMatchObject({ expected_password_version: 1 });
    expect(rotateRequest?.headers.get('Idempotency-Key')).toMatch(
      /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/
    );
    expect(password).toMatch(/^[A-Za-z0-9_-]{32}$/);
    expect(passwordInput).toHaveValue(password);
    expect(screen.queryByText('Состояние: Применяется')).not.toBeInTheDocument();
    expect([...Array(localStorage.length).keys()].map((index) => localStorage.key(index))).toEqual([
      'mv_token',
    ]);
    expect(window.location.href).not.toContain(password);

    window.dispatchEvent(new Event('pagehide'));

    await waitFor(() =>
      expect(screen.queryByRole('textbox', { name: 'Новый пароль' })).not.toBeInTheDocument()
    );
  });

  it('пока сервер применяет конфигурацию, показывает статус базы и блокирует отправку следующего изменения', async () => {
    const session = seedSession();
    installStatefulValkeyApi([
      valkeyInstanceDto({
        status: 'provisioning',
        applied_vcpu: 0,
        applied_ram_gb: 0,
        desired_generation: 1,
        observed_generation: 0,
        observed_at: null,
        is_stale: true,
        is_updating: true,
      }),
    ]);

    renderValkeySection(instancePath, session);

    expect(await screen.findByText('Создаётся', {}, WAIT)).toBeVisible();
    expect(screen.queryByText('Настройки применяются')).not.toBeInTheDocument();
    expect(screen.queryByText('Состояние давно не обновлялось')).not.toBeInTheDocument();
    expect(screen.queryByText('Дождитесь завершения текущей операции.')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Изменить тариф' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Изменить доступ по IP' })).toBeDisabled();
    expect(await screen.findByRole('button', { name: 'Сменить пароль' }, WAIT)).toBeDisabled();
  });

  it('при открытой карточке обновляет статус каждые пять секунд и не запускает параллельные запросы', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const session = seedSession();
    installStatefulValkeyApi();
    const statefulFetch = window.fetch;
    let detailCalls = 0;
    let resolveSecond: (response: Response) => void = () => undefined;
    const secondResponse = new Promise<Response>((resolve) => {
      resolveSecond = resolve;
    });
    window.fetch = vi.fn(async (input, init) => {
      if (String(input).endsWith(TEST_INSTANCE_ID) && (init?.method ?? 'GET') === 'GET') {
        detailCalls += 1;
        if (detailCalls === 1) {
          return jsonResponse(valkeyInstanceDto({ status: 'provisioning' }));
        }
        if (detailCalls === 2) {
          return secondResponse;
        }
      }
      return statefulFetch(input, init);
    });
    globalThis.fetch = window.fetch;

    renderValkeySection(instancePath, session);
    expect(await screen.findByText('Создаётся', {}, WAIT)).toBeVisible();

    await vi.advanceTimersByTimeAsync(5_000);
    await vi.waitFor(() => expect(detailCalls).toBe(2));
    await vi.advanceTimersByTimeAsync(5_000);
    expect(detailCalls).toBe(2);

    resolveSecond(jsonResponse(valkeyInstanceDto({ status: 'running' })));
    await act(async () => {
      await secondResponse;
    });
    await vi.advanceTimersByTimeAsync(0);

    expect(screen.getByText('Работает')).toBeVisible();
    expect(detailCalls).toBe(3);
  });

  it('после запроса удаления удерживает квоту и доступ к карточке, пока сервер не подтвердит удаление базы', async () => {
    const session = seedSession();
    installStatefulValkeyApi([
      valkeyInstanceDto({
        status: 'deleting',
        deletion_requested_at: '2026-09-09T00:01:00Z',
      }),
    ]);
    const user = userEvent.setup();

    renderValkeySection(instancePath, session);

    expect(await screen.findByText('Удаляется', {}, WAIT)).toBeVisible();
    expect(screen.queryByText('База удаляется')).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Действия с базой' }));
    expect(
      await screen.findByRole('menuitem', { name: 'Удалить', hidden: true }, WAIT)
    ).toHaveAttribute('data-disabled', 'true');
  });

  it('после конфликта имени сохраняет введённое значение в форме и обновляет карточку данными сервера', async () => {
    const session = seedSession();
    installStatefulValkeyApi();
    const statefulFetch = window.fetch;
    let rejected = false;
    let returnedRemoteState = false;
    window.fetch = vi.fn(async (input, init) => {
      if (!rejected && init?.method === 'PATCH') {
        rejected = true;
        return jsonResponse(
          {
            error: {
              code: 'OPERATION_IN_PROGRESS',
              message: 'Предыдущее изменение ещё применяется',
              details: { desired_generation: 2, observed_generation: 1 },
            },
          },
          409
        );
      }
      if (
        rejected &&
        !returnedRemoteState &&
        String(input).endsWith(TEST_INSTANCE_ID) &&
        (init?.method ?? 'GET') === 'GET'
      ) {
        returnedRemoteState = true;
        return jsonResponse(valkeyInstanceDto({ name: 'remote-name' }));
      }
      return statefulFetch(input, init);
    });
    globalThis.fetch = window.fetch;
    const user = userEvent.setup();

    renderValkeySection(instancePath, session);
    await screen.findByRole('heading', { name: 'cache' }, WAIT);
    await user.click(screen.getByRole('button', { name: 'Изменить имя' }));
    const name = screen.getByRole('textbox', { name: 'Имя базы' });
    await user.clear(name);
    await user.type(name, 'kept-name');
    await user.click(screen.getByRole('button', { name: 'Сохранить' }));

    expect(
      await screen.findByText(
        'Предыдущее изменение ещё применяется: подтверждено поколение 1 из 2.',
        {},
        WAIT
      )
    ).toBeVisible();
    expect(screen.getByRole('textbox', { name: 'Имя базы' })).toHaveValue('kept-name');
    expect(await screen.findByRole('heading', { name: 'remote-name' }, WAIT)).toBeVisible();
    expect(returnedRemoteState).toBe(true);
  });

  it('после перехода к списку баз игнорирует поздний ответ запроса прежней карточки', async () => {
    const session = seedSession();
    installStatefulValkeyApi();
    const statefulFetch = window.fetch;
    let resolveDetail: (response: Response) => void = () => undefined;
    const lateDetail = new Promise<Response>((resolve) => {
      resolveDetail = resolve;
    });
    const detailSignals: AbortSignal[] = [];
    let detailCalls = 0;
    window.fetch = vi.fn(async (input, init) => {
      if (String(input).endsWith(TEST_INSTANCE_ID) && (init?.method ?? 'GET') === 'GET') {
        detailCalls += 1;
        if (detailCalls === 1) {
          if (init.signal instanceof AbortSignal) {
            detailSignals.push(init.signal);
          }
          return lateDetail;
        }
      }
      return statefulFetch(input, init);
    });
    globalThis.fetch = window.fetch;

    const { router } = renderValkeySection(instancePath, session);
    await vi.waitFor(() => expect(detailCalls).toBe(1));
    await router.navigate('/valkey/management');
    expect(await screen.findByRole('heading', { name: 'Базы данных' }, WAIT)).toBeVisible();
    expect(detailSignals[0]?.aborted).toBe(true);

    resolveDetail(jsonResponse(valkeyInstanceDto({ name: 'late-card' })));
    await act(async () => {
      await lateDetail;
    });

    expect(screen.queryByRole('heading', { name: 'late-card' })).not.toBeInTheDocument();
  });
});
