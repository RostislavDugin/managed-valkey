import { afterEach, describe, expect, it, vi } from 'vitest';
import { setRybbitUser } from './rybbit';

const USER = {
  email: 'user@example.com',
  userId: '01930000-0000-7000-8000-000000000001',
};

function addRybbitScript() {
  const script = document.createElement('script');
  script.dataset.siteId = 'test-site';
  script.dataset.testRybbit = 'true';
  script.src = 'https://rybbit.databasus.com/api/script.js';
  script.type = 'application/json';
  document.head.append(script);
}

function installRybbitClient() {
  const client = {
    clearUserId: vi.fn(),
    identify: vi.fn(),
  };
  window.rybbit = client;
  return client;
}

afterEach(() => {
  delete window.rybbit;
  document.querySelectorAll('[data-test-rybbit]').forEach((element) => element.remove());
  vi.useRealTimers();
});

describe('синхронизация пользователя Rybbit', () => {
  it('при загруженном Rybbit передаёт UUID аккаунта и почту', () => {
    addRybbitScript();
    const client = installRybbitClient();

    setRybbitUser(USER);

    expect(client.identify).toHaveBeenCalledOnce();
    expect(client.identify).toHaveBeenCalledWith(USER.userId, { email: USER.email });
    expect(client.clearUserId).not.toHaveBeenCalled();
  });

  it('при отсутствии пользовательской сессии очищает идентификатор Rybbit', () => {
    addRybbitScript();
    const client = installRybbitClient();

    setRybbitUser(null);

    expect(client.clearUserId).toHaveBeenCalledOnce();
    expect(client.identify).not.toHaveBeenCalled();
  });

  it('при ошибке Rybbit не прерывает работу приложения', () => {
    addRybbitScript();
    const client = installRybbitClient();
    client.identify.mockImplementation(() => {
      throw new Error('Rybbit unavailable');
    });

    expect(() => setRybbitUser(USER)).not.toThrow();
  });

  it('при локальной сборке без скрипта Rybbit не начинает ожидание', () => {
    vi.useFakeTimers();

    setRybbitUser(USER);

    expect(vi.getTimerCount()).toBe(0);
  });

  it('после поздней загрузки Rybbit применяет только последнее состояние сессии', async () => {
    vi.useFakeTimers();
    addRybbitScript();

    setRybbitUser(USER);
    setRybbitUser(null);
    const client = installRybbitClient();
    await vi.advanceTimersByTimeAsync(100);

    expect(client.identify).not.toHaveBeenCalled();
    expect(client.clearUserId).toHaveBeenCalledOnce();
  });

  it('если Rybbit не загрузился за десять секунд, прекращает ожидание', async () => {
    vi.useFakeTimers();
    addRybbitScript();

    setRybbitUser(USER);
    await vi.advanceTimersByTimeAsync(10_000);

    expect(vi.getTimerCount()).toBe(0);
  });
});
