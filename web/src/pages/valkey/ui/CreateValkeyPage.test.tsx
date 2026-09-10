import { act, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';
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

function installApi(catalogResponses: Response[]) {
  const fetchMock = vi.fn<typeof fetch>(async (input) => {
    const path = String(input);
    if (path === '/v1/managed/valkey/instances') {
      return jsonResponse({ items: [] });
    }
    if (path === '/v1/me') {
      return jsonResponse({
        user: { id: '01930000-0000-7000-8000-000000000001', email: 'user@example.com' },
        quota: { max_vcpu: 4, max_ram_gb: 16 },
        usage: { used_vcpu: 0, used_ram_gb: 0 },
      });
    }
    if (path === '/v1/managed/valkey/sizes') {
      return catalogResponses.shift() ?? jsonResponse(catalog);
    }
    return jsonResponse({ error: { code: 'NOT_FOUND', message: 'Не найдено', details: {} } }, 404);
  });
  window.fetch = fetchMock;
  globalThis.fetch = fetchMock;
}

describe('форма создания Valkey', () => {
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

    expect(screen.getByText(/Адрес базы: shop-xxxxxx,/)).toBeVisible();
    expect(screen.getByRole('button', { name: 'Создать базу' })).toBeDisabled();

    await user.click(screen.getByRole('button', { name: 'Повторить' }));

    await waitFor(() => expect(screen.queryByText('Каталог недоступен')).not.toBeInTheDocument());
    expect(name).toHaveValue('cache-prod');
    expect(prefix).toHaveValue('shop');
    expect(screen.getByText(/Адрес базы: shop-xxxxxx\.valkey\.test,/)).toBeVisible();
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
      await screen.findByText('В кластере сейчас не хватает свободных ресурсов.', {}, WAIT)
    ).toBeVisible();
    expect(name).toHaveValue('cache-prod');
    expect(prefix).toHaveValue('shop');
    expect(screen.queryByRole('dialog', { name: 'Сохраните пароль' })).not.toBeInTheDocument();
    await waitFor(() =>
      expect(api.requests.filter((request) => request.path === '/v1/me').length).toBe(2)
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
