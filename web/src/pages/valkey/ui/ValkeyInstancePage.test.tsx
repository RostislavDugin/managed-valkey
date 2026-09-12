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
import {
  formatLocalMaintenance,
  MAINTENANCE_WEEKDAYS,
  utcMaintenanceToLocal,
} from '../model/valkey-maintenance';

const WAIT = { timeout: 10_000 };
const instancePath = `/valkey/management/${TEST_INSTANCE_ID}`;

afterEach(() => {
  vi.useRealTimers();
});

describe('управление базой Valkey', () => {
  it('показывает и копирует оба redis-адреса HA-базы и включает обе роли в пример', async () => {
    const session = seedSession();
    installStatefulValkeyApi([
      valkeyInstanceDto({
        mode: 'ha',
        vcpu: 2,
        ram_gb: 8,
        applied_vcpu: 2,
        applied_ram_gb: 8,
        host_ro: 'shop-abc123-ro.valkey.test',
      }),
    ]);
    const user = userEvent.setup();

    renderValkeySection(instancePath, session);

    expect(await screen.findByRole('heading', { name: 'cache' }, WAIT)).toBeVisible();
    expect(screen.getByRole('group', { name: 'Текущая конфигурация' })).toHaveTextContent(
      '2 vCPU / 8 ГБ'
    );
    expect(screen.queryByText('Применённая конфигурация')).not.toBeInTheDocument();
    expect(screen.queryByText('Суммарные ресурсы')).not.toBeInTheDocument();
    expect(screen.getByRole('group', { name: 'Адрес для записи и чтения' })).toHaveTextContent(
      'redis://shop-abc123.valkey.test:41379'
    );
    expect(screen.getByRole('group', { name: 'Адрес только для чтения' })).toHaveTextContent(
      'redis://shop-abc123-ro.valkey.test:41379'
    );
    expect(screen.getByText('Белый список')).toBeVisible();
    expect(screen.getByText('Создано')).toBeVisible();
    expect(screen.queryByText('Создана')).not.toBeInTheDocument();
    expect(document.body).toHaveTextContent("const primaryHost = 'shop-abc123.valkey.test'");
    expect(document.body).toHaveTextContent("const readHost = 'shop-abc123-ro.valkey.test'");
    expect(document.body).not.toHaveTextContent('tls: true');
    await user.hover(screen.getByRole('img', { name: 'Подсказка: Адрес для записи и чтения' }));
    expect(
      await screen.findByText(/Если primary недоступен, одна из реплик принимает его роль/)
    ).toBeVisible();

    await user.click(screen.getByRole('button', { name: 'Скопировать адрес для записи и чтения' }));
    await expect(navigator.clipboard.readText()).resolves.toBe(
      'redis://shop-abc123.valkey.test:41379'
    );
    await user.click(screen.getByRole('button', { name: 'Скопировать адрес для чтения' }));
    await expect(navigator.clipboard.readText()).resolves.toBe(
      'redis://shop-abc123-ro.valkey.test:41379'
    );
  });

  it('показывает два разных redis-адреса у single-базы', async () => {
    const session = seedSession();
    installStatefulValkeyApi();

    renderValkeySection(instancePath, session);

    expect(await screen.findByRole('heading', { name: 'cache' }, WAIT)).toBeVisible();
    expect(screen.getByRole('group', { name: 'Адрес для записи и чтения' })).toHaveTextContent(
      'redis://shop-abc123.valkey.test:41379'
    );
    expect(screen.getByRole('group', { name: 'Адрес только для чтения' })).toHaveTextContent(
      'redis://shop-abc123-ro.valkey.test:41379'
    );
  });

  it('при переименовании отправляет PATCH только с именем и повторно загружает карточку базы', async () => {
    const session = seedSession();
    const api = installStatefulValkeyApi();
    const user = userEvent.setup();

    renderValkeySection(instancePath, session);
    expect(await screen.findByRole('heading', { name: 'cache' }, WAIT)).toBeVisible();
    await user.click(screen.getByRole('button', { name: 'Изменить имя' }));

    const name = screen.getByRole('textbox', { name: 'Имя базы' });
    await user.clear(name);
    await user.type(name, 'cache-renamed');
    await user.click(screen.getByRole('button', { name: 'Сохранить' }));

    await waitFor(() =>
      expect(api.requests.find((request) => request.method === 'PATCH')?.body).toEqual({
        name: 'cache-renamed',
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

  it('показывает и заменяет обязательное окно обслуживания в местном времени', async () => {
    const session = seedSession();
    const api = installStatefulValkeyApi([
      valkeyInstanceDto({
        status: 'updating',
        is_updating: true,
        maintenance: { dow: 2, hour_utc: 3, duration_min: 60 },
      }),
    ]);
    const user = userEvent.setup();
    const timezoneOffsetMinutes = new Date().getTimezoneOffset();
    const current = { dow: 2, hourUtc: 3, durationMin: 60 };
    const currentLocal = utcMaintenanceToLocal(current, timezoneOffsetMinutes);
    const next = { dow: 4, hourUtc: 5, durationMin: 60 };
    const nextLocal = utcMaintenanceToLocal(next, timezoneOffsetMinutes);

    renderValkeySection(instancePath, session);
    await screen.findByRole('heading', { name: 'cache' }, WAIT);
    const action = screen.getByRole('button', { name: 'Изменить окно обслуживания' });
    expect(action).toBeEnabled();
    await user.click(action);

    const day = screen.getByRole('combobox', { name: 'День недели' });
    const time = screen.getByRole('combobox', { name: 'Время начала, местное' });
    expect(day).toHaveValue(MAINTENANCE_WEEKDAYS[currentLocal.dow].label);
    expect(time).toHaveValue(currentLocal.time);
    expect(screen.queryByRole('textbox', { name: 'Длительность, минуты' })).not.toBeInTheDocument();
    await user.click(day);
    await user.keyboard('{ArrowDown}{ArrowDown}{Enter}');
    await user.click(time);
    await user.keyboard('{ArrowDown}{ArrowDown}{Enter}');
    expect(day).toHaveValue(MAINTENANCE_WEEKDAYS[nextLocal.dow].label);
    expect(time).toHaveValue(nextLocal.time);
    await user.click(screen.getByRole('button', { name: 'Сохранить' }));

    await waitFor(() =>
      expect(api.requests.filter((request) => request.method === 'PATCH').at(-1)?.body).toEqual({
        maintenance: { dow: 4, hour_utc: 5, duration_min: 60 },
      })
    );
    expect(
      await screen.findByText(formatLocalMaintenance(next, timezoneOffsetMinutes), {}, WAIT)
    ).toBeVisible();

    await user.click(screen.getByRole('button', { name: 'Изменить окно обслуживания' }));
    expect(
      screen.queryByRole('switch', { name: 'Задать окно обслуживания' })
    ).not.toBeInTheDocument();
  });

  it('при сохранении списка доступа преобразует одиночный IP-адрес в канонический CIDR и отправляет его серверу', async () => {
    const session = seedSession();
    const api = installStatefulValkeyApi();
    const user = userEvent.setup();

    renderValkeySection(instancePath, session);
    await screen.findByRole('heading', { name: 'cache' }, WAIT);
    await user.click(screen.getByRole('button', { name: 'Изменить белый список' }));
    expect(await screen.findByRole('dialog', { name: 'Белый список' }, WAIT)).toBeVisible();
    await user.click(screen.getByRole('switch', { name: 'Ограничить доступ по IP-адресам' }));
    await user.click(screen.getByRole('button', { name: 'Сохранить' }));
    expect(
      await screen.findByText('Добавьте хотя бы один IPv4-адрес или диапазон CIDR')
    ).toBeVisible();
    expect(api.requests.some((request) => request.method === 'PUT')).toBe(false);
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
    const rotateAction = await screen.findByRole('button', { name: 'Изменить пароль' }, WAIT);
    await user.click(rotateAction);
    expect(api.requests.some((request) => request.path.endsWith('/credentials/rotate'))).toBe(
      false
    );

    await user.click(screen.getByRole('button', { name: 'Подтвердить изменение пароля' }));

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
    expect(screen.getByRole('button', { name: 'Изменить белый список' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Изменить окно обслуживания' })).toBeEnabled();
    expect(await screen.findByRole('button', { name: 'Изменить пароль' }, WAIT)).toBeDisabled();
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
    expect(screen.getByRole('button', { name: 'Изменить окно обслуживания' })).toBeDisabled();
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
