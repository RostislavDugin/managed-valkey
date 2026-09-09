import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { seedSession, withProviders } from '../../../../test/render';
import type { ValkeyInstance } from '../model/valkey';
import { DeleteInstanceModal } from './DeleteInstanceModal';

const instance = {
  id: 'instance-1',
  name: 'valkey-1474',
  slug: 'valkey-instance-1',
} as ValkeyInstance;

beforeEach(() => {
  seedSession();
});

describe('удаление базы', () => {
  it('отправляет DELETE только после ввода точного slug', async () => {
    const fetchMock = vi
      .spyOn(window, 'fetch')
      .mockResolvedValue(new Response(null, { status: 202 }));
    const onClose = vi.fn();
    const onDeleted = vi.fn();
    const user = userEvent.setup();

    render(
      withProviders(
        <DeleteInstanceModal instance={instance} onClose={onClose} onDeleted={onDeleted} />
      )
    );

    const confirmation = screen.getByRole('textbox', {
      name: /Введите valkey-instance-1 для подтверждения/,
    });
    const remove = screen.getByRole('button', { name: 'Удалить базу' });

    await user.type(confirmation, 'valkey-instance');
    expect(remove).toBeDisabled();
    expect(fetchMock).not.toHaveBeenCalled();

    await user.type(confirmation, '-1');
    expect(remove).toBeEnabled();
    await user.click(remove);

    await waitFor(() =>
      expect(fetchMock).toHaveBeenCalledWith(
        '/v1/managed/valkey/instances/instance-1',
        expect.objectContaining({ method: 'DELETE' })
      )
    );
    expect(onDeleted).toHaveBeenCalledOnce();
    expect(onClose).toHaveBeenCalledOnce();
    expect(screen.queryByText('База удаляется')).not.toBeInTheDocument();
  });
});
