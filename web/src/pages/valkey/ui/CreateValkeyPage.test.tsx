import { act, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { renderValkeySection, seedSession } from '../../../../test/render';
import {
  installStatefulValkeyApi,
  jsonResponse as statefulJsonResponse,
} from '../../../../test/valkey-api-fixture';

const WAIT = { timeout: 10_000 };
const catalog = {
  items: [
    { vcpu: 1, ram_gb: 1 },
    { vcpu: 2, ram_gb: 8 },
  ],
  pricing: {
    vcpu_coins_per_hour: 125,
    ram_gb_coins_per_hour: 50,
    hours_per_month: 720,
  },
  connection: { domain: 'valkey.test', port: 41379 },
};

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

const availableCapacity = {
  user: { limit: { vcpu: 4, ram_gb: 12 }, used: { vcpu: 0, ram_gb: 0 } },
  cluster: { limit: { vcpu: 12, ram_gb: 48 }, used: { vcpu: 0, ram_gb: 0 } },
  instances: { limit: 32, used: 0 },
};

function installApi(catalogResponses: Response[], capacityResponses = [availableCapacity]) {
  const fetchMock = vi.fn<typeof fetch>(async (input) => {
    const path = String(input);
    if (path === '/v1/managed/valkey/instances') {
      return jsonResponse({ items: [] });
    }
    if (path === '/v1/me') {
      return jsonResponse({
        user: { id: '01930000-0000-7000-8000-000000000001', email: 'user@example.com' },
        quota: { max_vcpu: 4, max_ram_gb: 12 },
        usage: { used_vcpu: 0, used_ram_gb: 0 },
      });
    }
    if (path === '/v1/managed/valkey/capacity') {
      return jsonResponse(capacityResponses.shift() ?? availableCapacity);
    }
    if (path === '/v1/managed/valkey/sizes') {
      return catalogResponses.shift() ?? jsonResponse(catalog);
    }
    return jsonResponse({ error: { code: 'NOT_FOUND', message: 'Не найдено', details: {} } }, 404);
  });
  window.fetch = fetchMock;
  globalThis.fetch = fetchMock;
  return fetchMock;
}

afterEach(() => {
  vi.useRealTimers();
});

describe('форма создания Valkey', () => {
  it.each([
    {
      name: 'общий бюджет',
      capacity: {
        ...availableCapacity,
        cluster: { limit: { vcpu: 12, ram_gb: 48 }, used: { vcpu: 12, ram_gb: 48 } },
      },
      message: 'Недостаточно ресурсов Managed Kubernetes',
    },
    {
      name: 'предел баз',
      capacity: { ...availableCapacity, instances: { limit: 32, used: 32 } },
      message: 'Достигнут предел числа баз',
    },
  ])('при ограничении "$name" показывает точную причину и запрещает создание', async (testCase) => {
    const session = seedSession();
    installApi([jsonResponse(catalog)], [testCase.capacity]);

    renderValkeySection('/valkey/management/new', session);

    expect(await screen.findByText(testCase.message, {}, WAIT)).toBeVisible();
    expect(screen.getByRole('button', { name: 'Создать базу' })).toBeDisabled();
    expect(screen.getAllByText(/\/ мес\./).length).toBeGreaterThan(0);
  });

  it('при лимите кластера 3 vCPU и 14 ГБ показывает размер 4 vCPU и 16 ГБ, но запрещает создание', async () => {
    const session = seedSession();
    const fullCatalog = {
      ...catalog,
      items: [...catalog.items, { vcpu: 4, ram_gb: 16 }],
    };
    installApi(
      [jsonResponse(fullCatalog)],
      [
        {
          user: { limit: { vcpu: 8, ram_gb: 32 }, used: { vcpu: 0, ram_gb: 0 } },
          cluster: { limit: { vcpu: 3, ram_gb: 14 }, used: { vcpu: 0, ram_gb: 0 } },
          instances: { limit: 32, used: 0 },
        },
      ]
    );
    const user = userEvent.setup();

    renderValkeySection('/valkey/management/new', session);

    const size = await screen.findByRole('radio', { name: /4\s*vCPU.*16\s*ГБ/i }, WAIT);
    expect(size).toBeVisible();
    await user.click(size);

    expect(screen.getByText('Недостаточно ресурсов Managed Kubernetes')).toBeVisible();
    expect(screen.getByRole('button', { name: 'Создать базу' })).toBeDisabled();
  });

  it('при одновременном исчерпании личной квоты и общего бюджета показывает личную квоту', async () => {
    const session = seedSession();
    installApi(
      [jsonResponse(catalog)],
      [
        {
          user: { limit: { vcpu: 4, ram_gb: 12 }, used: { vcpu: 4, ram_gb: 12 } },
          cluster: { limit: { vcpu: 12, ram_gb: 48 }, used: { vcpu: 12, ram_gb: 48 } },
          instances: { limit: 32, used: 5 },
        },
      ]
    );

    renderValkeySection('/valkey/management/new', session);

    expect(await screen.findByText('Недостаточно квоты', {}, WAIT)).toBeVisible();
    expect(screen.queryByText('Недостаточно ресурсов Managed Kubernetes')).not.toBeInTheDocument();
    expect(
      screen.getByRole('link', { name: 'Напишите в поддержку для увеличения квоты' })
    ).toHaveAttribute('href', 'https://t.me/rostislav_dugin');
  });

  it('после следующего опроса запрещает создание, если другой аккаунт занял общий ресурс', async () => {
    vi.useFakeTimers();
    const session = seedSession();
    installApi(
      [jsonResponse(catalog)],
      [
        availableCapacity,
        {
          ...availableCapacity,
          cluster: { limit: { vcpu: 12, ram_gb: 48 }, used: { vcpu: 12, ram_gb: 48 } },
        },
      ]
    );

    renderValkeySection('/valkey/management/new', session);
    await act(async () => vi.advanceTimersByTimeAsync(0));
    expect(screen.getByRole('button', { name: 'Создать базу' })).toBeEnabled();

    await act(async () => vi.advanceTimersByTimeAsync(5_000));
    expect(screen.getByText('Недостаточно ресурсов Managed Kubernetes')).toBeVisible();
    expect(screen.getByRole('button', { name: 'Создать базу' })).toBeDisabled();
  });

  it('после ошибки каталога сохраняет введённые значения и запрещает создание до успешной повторной загрузки', async () => {
    const session = seedSession();
    installApi([
      jsonResponse(
        { error: { code: 'REQUEST_FAILED', message: 'Каталог недоступен', details: {} } },
        400
      ),
      jsonResponse(catalog),
    ]);
    const user = userEvent.setup();

    renderValkeySection('/valkey/management/new', session);

    expect(await screen.findByText('Каталог недоступен', {}, WAIT)).toBeVisible();
    const name = screen.getByRole('textbox', { name: 'Имя' });
    const prefix = screen.getByRole('textbox', { name: 'Префикс' });
    await user.clear(name);
    await user.type(name, 'cache-prod');
    await user.clear(prefix);
    await user.type(prefix, 'shop');

    expect(screen.getByText(/Primary: redis:\/\/shop-xxxxxx/)).toBeVisible();
    expect(screen.getByRole('button', { name: 'Создать базу' })).toBeDisabled();

    await user.click(screen.getByRole('button', { name: 'Повторить' }));

    await waitFor(() => expect(screen.queryByText('Каталог недоступен')).not.toBeInTheDocument());
    expect(name).toHaveValue('cache-prod');
    expect(prefix).toHaveValue('shop');
    expect(screen.getByText(/Primary: redis:\/\/shop-xxxxxx\.valkey\.test:41379/)).toBeVisible();
    expect(
      screen.getByText(/Для чтения: redis:\/\/shop-xxxxxx-ro\.valkey\.test:41379/)
    ).toBeVisible();
    expect(screen.getByRole('button', { name: 'Создать базу' })).toBeEnabled();
  });

  it('при включённом пустом списке адресов требует явного подтверждения недоступности базы', async () => {
    const session = seedSession();
    installApi([jsonResponse(catalog)]);
    const user = userEvent.setup();

    renderValkeySection('/valkey/management/new', session);

    const create = await screen.findByRole('button', { name: 'Создать базу' }, WAIT);
    expect(create).toBeEnabled();

    await user.click(screen.getByRole('switch', { name: 'Ограничить доступ по IP-адресам' }));

    const confirmation = screen.getByRole('checkbox', {
      name: 'Запретить все подключения к базе',
    });
    expect(create).toBeDisabled();

    await user.click(confirmation);

    expect(create).toBeEnabled();
  });

  it('проверяет окно обслуживания и отправляет его в одном POST создания', async () => {
    const session = seedSession();
    const api = installStatefulValkeyApi([]);
    const user = userEvent.setup();

    renderValkeySection('/valkey/management/new', session);

    const maintenance = await screen.findByRole(
      'switch',
      { name: 'Задать окно обслуживания' },
      WAIT
    );
    expect(maintenance).not.toBeChecked();
    expect(screen.getByText('Белый список')).toBeVisible();
    expect(screen.queryByText('Белый список адресов')).not.toBeInTheDocument();
    await user.click(maintenance);

    const day = screen.getByRole('textbox', { name: 'День недели, 0–6' });
    const hour = screen.getByRole('textbox', { name: 'Час UTC, 0–23' });
    const duration = screen.getByRole('textbox', { name: 'Длительность, минуты' });
    await user.clear(day);
    await user.clear(hour);
    await user.clear(duration);
    await user.click(screen.getByRole('button', { name: 'Создать базу' }));

    expect(await screen.findByText('Укажите число от 0 до 6')).toBeVisible();
    expect(screen.getByText('Укажите час от 0 до 23')).toBeVisible();
    expect(screen.getByText('Укажите длительность от 1 до 1440 минут')).toBeVisible();
    expect(api.requests.some((request) => request.method === 'POST')).toBe(false);

    await user.clear(day);
    await user.type(day, '2');
    await user.clear(hour);
    await user.type(hour, '3');
    await user.clear(duration);
    await user.type(duration, '60');
    await user.click(screen.getByRole('button', { name: 'Создать базу' }));

    await waitFor(() =>
      expect(api.requests.find((request) => request.method === 'POST')?.body).toMatchObject({
        maintenance: { dow: 2, hour_utc: 3, duration_min: 60 },
      })
    );
    expect(api.requests.some((request) => request.method === 'PATCH')).toBe(false);
  });

  it('после ответа 422 сохраняет поля формы, а при следующей отправке создаёт новый пароль', async () => {
    const session = seedSession();
    const api = installStatefulValkeyApi([]);
    const statefulFetch = window.fetch;
    const createRequests: RequestInit[] = [];
    window.fetch = vi.fn(async (input, init) => {
      if (String(input) === '/v1/managed/valkey/instances' && init?.method === 'POST') {
        createRequests.push(init);
        if (createRequests.length === 1) {
          return statefulJsonResponse(
            {
              error: {
                code: 'NOT_ENOUGH_RESOURCES',
                message: 'Недостаточно свободных ресурсов кластера',
                details: { reason: 'cluster_quota' },
              },
            },
            422
          );
        }
      }
      return statefulFetch(input, init);
    });
    globalThis.fetch = window.fetch;
    const user = userEvent.setup();

    const view = renderValkeySection('/valkey/management/new', session);

    const name = await screen.findByRole('textbox', { name: 'Имя' }, WAIT);
    const prefix = screen.getByRole('textbox', { name: 'Префикс' });
    await user.clear(name);
    await user.type(name, 'cache-prod');
    await user.clear(prefix);
    await user.type(prefix, 'shop');
    await user.click(screen.getByRole('button', { name: 'Создать базу' }));

    expect(
      await screen.findByText('Недостаточно свободных ресурсов кластера', {}, WAIT)
    ).toBeVisible();
    expect(name).toHaveValue('cache-prod');
    expect(prefix).toHaveValue('shop');
    expect(screen.queryByRole('dialog', { name: 'Сохраните пароль' })).not.toBeInTheDocument();
    await waitFor(() =>
      expect(
        api.requests.filter((request) => request.path === '/v1/managed/valkey/capacity').length
      ).toBe(2)
    );

    await user.click(screen.getByRole('button', { name: 'Создать базу' }));

    expect(await screen.findByRole('dialog', { name: 'Сохраните пароль' }, WAIT)).toBeVisible();
    expect(screen.queryByText('База создаётся')).not.toBeInTheDocument();
    expect(screen.queryByText('Создание базы cache-prod принято.')).not.toBeInTheDocument();
    const firstBody = JSON.parse(String(createRequests[0].body)) as { password: string };
    const secondBody = JSON.parse(String(createRequests[1].body)) as { password: string };
    expect(firstBody.password).not.toBe(secondBody.password);
    expect(new Headers(createRequests[0].headers).get('Idempotency-Key')).not.toBe(
      new Headers(createRequests[1].headers).get('Idempotency-Key')
    );
    act(() => {
      view.setSession({
        ...session,
        userId: '01930000-0000-7000-8000-000000000099',
        email: 'second@example.com',
      });
    });
    await waitFor(() =>
      expect(screen.queryByRole('dialog', { name: 'Сохраните пароль' })).not.toBeInTheDocument()
    );
  });

  it('после потерянного ответа повторяет создание с теми же ключом и паролем, а полученный пароль показывает один раз', async () => {
    const session = seedSession();
    installStatefulValkeyApi([]);
    const statefulFetch = window.fetch;
    const createRequests: RequestInit[] = [];
    vi.spyOn(Math, 'random').mockReturnValue(0);
    window.fetch = vi.fn(async (input, init) => {
      if (String(input) === '/v1/managed/valkey/instances' && init?.method === 'POST') {
        createRequests.push(init);
        if (createRequests.length <= 3) {
          throw new TypeError('connection lost');
        }
      }
      return statefulFetch(input, init);
    });
    globalThis.fetch = window.fetch;
    const user = userEvent.setup();

    renderValkeySection('/valkey/management/new', session);

    const create = await screen.findByRole('button', { name: 'Создать базу' }, WAIT);
    await user.click(create);
    expect(
      await screen.findByText('Не удалось выполнить запрос. Попробуйте ещё раз.', {}, WAIT)
    ).toBeVisible();

    await user.click(create);

    const passwordInput = await screen.findByRole('textbox', { name: 'Пароль базы' }, WAIT);
    expect((passwordInput as HTMLInputElement).value).toMatch(/^[A-Za-z0-9_-]{32}$/);
    const keys = createRequests.map((request) =>
      new Headers(request.headers).get('Idempotency-Key')
    );
    const passwords = createRequests.map(
      (request) => (JSON.parse(String(request.body)) as { password: string }).password
    );
    expect(new Set(keys).size).toBe(1);
    expect(new Set(passwords).size).toBe(1);
    expect(localStorage.getItem('mv_valkey_instances')).toBeNull();
    expect(window.location.href).not.toContain(passwords[0]);

    await user.click(screen.getByRole('button', { name: 'Закрыть' }));

    expect(screen.queryByRole('textbox', { name: 'Пароль базы' })).not.toBeInTheDocument();
  });
});
