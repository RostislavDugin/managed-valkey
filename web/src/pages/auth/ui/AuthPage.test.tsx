import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { AUTH_TOKEN_KEY } from '@/shared/api';
import { renderRoutes } from '../../../../test/render';
import { AuthPage } from './AuthPage';

const TOKEN =
  'header.eyJzdWIiOiIwMTkzMDAwMC0wMDAwLTcwMDAtODAwMC0wMDAwMDAwMDAwMDEiLCJleHAiOjQxMDI0NDQ4MDB9.signature';

function jsonResponse(body: unknown, status = 200) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function renderAuth() {
  return renderRoutes(
    [
      {
        path: '/auth',
        loader: () => ({ reason: null, returnTo: '/done' }),
        Component: AuthPage,
      },
      { path: '/done', element: <h1>Готово</h1> },
    ],
    '/auth'
  );
}

beforeEach(() => {
  localStorage.clear();
});

describe('экран авторизации с API', () => {
  it('выполняет двухшаговый вход и оставляет подсказку менеджеру паролей', async () => {
    const user = userEvent.setup();
    vi.spyOn(window, 'fetch')
      .mockResolvedValueOnce(jsonResponse({ exists: true }))
      .mockResolvedValueOnce(jsonResponse({ token: TOKEN }));
    renderAuth();

    await user.type(await screen.findByLabelText('Почта'), 'User@Example.com');
    await user.click(screen.getByRole('button', { name: 'Продолжить' }));

    const password = await screen.findByLabelText('Пароль');
    expect(password).toHaveAttribute('autocomplete', 'current-password');
    await user.type(password, 'password1');
    await user.click(screen.getByRole('button', { name: 'Войти' }));

    expect(await screen.findByRole('heading', { name: 'Готово' })).toBeVisible();
    expect(localStorage.getItem(AUTH_TOKEN_KEY)).toBe(TOKEN);
  });

  it('показывает регистрацию и сообщает о конкурентно созданном аккаунте', async () => {
    const user = userEvent.setup();
    vi.spyOn(window, 'fetch')
      .mockResolvedValueOnce(jsonResponse({ exists: false }))
      .mockResolvedValueOnce(
        jsonResponse({ error: { code: 'CONFLICT', message: 'Аккаунт уже существует' } }, 409)
      );
    renderAuth();

    await user.type(await screen.findByLabelText('Почта'), 'user@example.com');
    await user.click(screen.getByRole('button', { name: 'Продолжить' }));

    const password = await screen.findByLabelText('Пароль');
    const confirmation = screen.getByLabelText('Повторите пароль');
    expect(password).toHaveAttribute('autocomplete', 'new-password');
    expect(confirmation).toHaveAttribute('autocomplete', 'new-password');
    await user.type(password, 'password1');
    await user.type(confirmation, 'password1');
    await user.click(screen.getByRole('button', { name: 'Создать аккаунт' }));

    expect(
      await screen.findByText('Аккаунт с этой почтой уже появился. Введите пароль для входа.')
    ).toBeVisible();
    expect(screen.getByRole('button', { name: 'Войти' })).toBeVisible();
  });

  it('показывает срок ожидания после RATE_LIMITED', async () => {
    const user = userEvent.setup();
    vi.spyOn(window, 'fetch').mockResolvedValueOnce(
      jsonResponse(
        {
          error: {
            code: 'RATE_LIMITED',
            message: 'Слишком много запросов',
            details: { retry_after: 42 },
          },
        },
        429
      )
    );
    renderAuth();

    await user.type(await screen.findByLabelText('Почта'), 'user@example.com');
    await user.click(screen.getByRole('button', { name: 'Продолжить' }));

    expect(await screen.findByText('Слишком много проверок. Повторите через 42 с.')).toBeVisible();
  });
});
