import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { renderValkeySection, seedSession, TEST_USER_ID, wholeText } from '../../../../test/render';
import { createInstance, listInstances } from '../api/valkey-storage';
import type { ValkeyInstance, ValkeyMode, ValkeyRamGb, ValkeyVcpu } from '../model/valkey';

const WAIT = { timeout: 10_000 };

function seedInstance(name: string, mode: ValkeyMode, vcpu: ValkeyVcpu, ramGb: ValkeyRamGb) {
  return createInstance(TEST_USER_ID, { name, prefix: 'valkey', mode, vcpu, ramGb });
}

async function openCard(instance: ValkeyInstance) {
  const result = renderValkeySection(`/valkey/management/${instance.id}`);
  await screen.findByRole('heading', { name: instance.name }, WAIT);
  return result;
}

function dialog() {
  return screen.getByRole('dialog');
}

beforeEach(() => {
  seedSession();
});

describe('карточка базы', () => {
  it('показывает свойства базы по прямой ссылке', async () => {
    const instance = await seedInstance('valkey-1474', 'ha', 1, 2);

    await openCard(instance);

    expect(screen.getByText('Работает')).toBeVisible();
    expect(screen.getByText('Отказоустойчивый')).toBeVisible();
    expect(screen.getByText('1 vCPU / 2 ГБ')).toBeVisible();
    expect(screen.getByText('3 vCPU / 6 ГБ')).toBeVisible();
    expect(screen.getByText(instance.id)).toBeVisible();
    expect(screen.getByRole('button', { name: 'Изменить имя' })).toBeVisible();
    expect(screen.getByRole('button', { name: 'Скопировать ID' })).toBeVisible();
    expect(screen.getByRole('button', { name: 'Скопировать адрес' })).toBeVisible();
    expect(screen.getByText('Текущая стоимость')).toBeVisible();
    expect(screen.getByText(wholeText('4 860,00 ₽ в месяц'))).toBeVisible();
  });

  it('показывает и переключает четыре примера подключения', async () => {
    const instance = await seedInstance('valkey-1474', 'single', 1, 2);
    const user = userEvent.setup();

    await openCard(instance);

    const languageTabs = screen.getByRole('tablist', { name: 'Язык примера подключения' });
    expect(within(languageTabs).getAllByRole('tab')).toHaveLength(4);
    expect(screen.getByRole('tabpanel', { name: 'valkey.js' })).toHaveTextContent(
      "import { createClient } from 'redis'"
    );
    expect(screen.queryByText('Подключение по TLS')).toBeNull();
    expect(document.querySelector('.hljs-keyword')).toHaveTextContent('import');

    await user.click(within(languageTabs).getByRole('tab', { name: 'valkey.ts' }));
    expect(screen.getByRole('tabpanel', { name: 'valkey.ts' })).toHaveTextContent(
      'satisfies RedisClientOptions'
    );

    await user.click(within(languageTabs).getByRole('tab', { name: 'valkey.py' }));
    expect(screen.getByRole('tabpanel', { name: 'valkey.py' })).toHaveTextContent('ssl=True');

    await user.click(within(languageTabs).getByRole('tab', { name: 'valkey.go' }));
    const goExample = screen.getByRole('tabpanel', { name: 'valkey.go' });
    expect(goExample).toHaveTextContent('github.com/redis/go-redis/v9');
    expect(goExample).toHaveTextContent('Addr: "valkey-');
    expect(goExample).toHaveTextContent(':41379"');
    expect(goExample).not.toHaveTextContent('host +');
    expect(goExample).toHaveTextContent('<PASSWORD>');
    expect(screen.getByRole('button', { name: 'Скопировать код' })).toBeVisible();
  });

  it('копирует активный пример и сворачивает общий блок', async () => {
    const instance = await seedInstance('valkey-1474', 'single', 1, 2);
    const user = userEvent.setup();
    const writeText = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue();

    await openCard(instance);

    await user.click(screen.getByRole('tab', { name: 'valkey.py' }));
    await user.click(screen.getByRole('button', { name: 'Скопировать код' }));

    expect(writeText).toHaveBeenCalledWith(expect.stringContaining('client = redis.Redis('));
    expect(screen.getByRole('button', { name: 'Код скопирован' })).toBeVisible();

    const toggle = screen.getByRole('button', { name: 'Свернуть' });
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
    await user.click(toggle);

    expect(toggle).toHaveAccessibleName('Развернуть');
    expect(toggle).toHaveAttribute('aria-expanded', 'false');
    expect(screen.getByRole('button', { name: 'Развернуть код' })).toBeVisible();

    await user.click(screen.getByRole('tab', { name: 'valkey.go' }));
    expect(toggle).toHaveAttribute('aria-expanded', 'false');

    await user.click(screen.getByRole('button', { name: 'Развернуть код' }));
    expect(toggle).toHaveAccessibleName('Свернуть');
    expect(toggle).toHaveAttribute('aria-expanded', 'true');
  });

  it('сохраняет статус, удаление и стоимость при переходе по вкладкам', async () => {
    const instance = await seedInstance('valkey-1474', 'single', 1, 2);
    const user = userEvent.setup();
    const { router } = await openCard(instance);

    await user.click(screen.getByRole('tab', { name: 'Мониторинг' }));

    expect(await screen.findByRole('heading', { name: 'Мониторинг' }, WAIT)).toBeVisible();
    expect(screen.getByText('Работает')).toBeVisible();
    expect(screen.getByRole('button', { name: 'Действия с базой' })).toBeVisible();
    expect(screen.getByText('Текущая стоимость')).toBeVisible();
    expect(screen.getByTestId('console-aside')).not.toBeEmptyDOMElement();

    await router.navigate(-1);
    expect(await screen.findByRole('heading', { name: instance.name }, WAIT)).toBeVisible();
    expect(screen.getByText('Текущая стоимость')).toBeVisible();
  });

  it('показывает адрес базы с доменом и портом', async () => {
    const instance = await seedInstance('valkey-1474', 'single', 1, 2);

    await openCard(instance);

    expect(
      await screen.findByText(`${instance.slug}.valkey.h3llo-demo.com:41379`, {}, WAIT)
    ).toBeVisible();
  });

  it('сообщает, что база не найдена, и предлагает вернуться к списку', async () => {
    renderValkeySection('/valkey/management/00000000-0000-0000-0000-000000000000');

    expect(await screen.findByRole('heading', { name: 'База не найдена' }, WAIT)).toBeVisible();
    expect(screen.getByRole('link', { name: 'К списку баз' })).toHaveAttribute(
      'href',
      '/valkey/management'
    );
  });
});

describe('переименование', () => {
  it('меняет имя в карточке, крошках и списке', async () => {
    const instance = await seedInstance('valkey-1474', 'single', 1, 2);
    const user = userEvent.setup();
    const { router } = await openCard(instance);

    await user.click(screen.getByRole('button', { name: 'Изменить имя' }));

    const field = within(dialog()).getByRole('textbox', { name: 'Имя базы' });
    await user.clear(field);
    await user.type(field, 'valkey-2222');
    await user.click(within(dialog()).getByRole('button', { name: 'Сохранить имя' }));

    expect(await screen.findByRole('heading', { name: 'valkey-2222' }, WAIT)).toBeVisible();
    expect(screen.getByRole('navigation', { name: 'Хлебные крошки' })).toHaveTextContent(
      'valkey-2222'
    );

    await router.navigate('/valkey/management');
    expect(await screen.findByRole('link', { name: 'valkey-2222' }, WAIT)).toBeVisible();
  });

  it('ставит ошибку занятого имени у поля', async () => {
    const instance = await seedInstance('valkey-1474', 'single', 1, 1);
    await seedInstance('valkey-2222', 'single', 1, 1);
    const user = userEvent.setup();
    await openCard(instance);

    await user.click(screen.getByRole('button', { name: 'Изменить имя' }));

    const field = within(dialog()).getByRole('textbox', { name: 'Имя базы' });
    await user.clear(field);
    await user.type(field, 'valkey-2222');
    await user.click(within(dialog()).getByRole('button', { name: 'Сохранить имя' }));

    expect(
      await within(dialog()).findByText('База с таким именем уже существует', {}, WAIT)
    ).toBeVisible();
    expect(screen.getByRole('heading', { name: 'valkey-1474' })).toBeVisible();
  });
});

describe('изменение тарифа', () => {
  it('оставляет режим неизменяемым и предупреждает о простое для single', async () => {
    const instance = await seedInstance('valkey-1474', 'single', 4, 16);
    const user = userEvent.setup();
    await openCard(instance);

    await user.click(screen.getByRole('button', { name: 'Изменить тариф' }));

    expect(within(dialog()).getByText('Одна нода, изменить нельзя')).toBeVisible();

    await user.click(within(dialog()).getByRole('radio', { name: /2\s*vCPU, 8\s*ГБ RAM/ }));

    expect(
      await within(dialog()).findByText(
        'База будет недоступна во время ресайза, кеш очистится полностью.',
        {},
        WAIT
      )
    ).toBeVisible();
    expect(
      within(dialog()).getByText(
        'Ресайз нельзя отменить, новый тариф действует с момента принятия запроса.'
      )
    ).toBeVisible();
    expect(within(dialog()).getByText('4 680,00 ₽ в месяц')).toBeVisible();

    await user.click(within(dialog()).getByRole('button', { name: 'Изменить тариф' }));

    // Тариф на ноду и суммарные ресурсы у `single` совпадают, поэтому строк две.
    expect(await screen.findAllByText('2 vCPU / 8 ГБ', {}, WAIT)).toHaveLength(2);
  });

  it('предупреждает об очистке кеша при уменьшении RAM в ha', async () => {
    const instance = await seedInstance('valkey-1474', 'ha', 1, 2);
    const user = userEvent.setup();
    await openCard(instance);

    await user.click(screen.getByRole('button', { name: 'Изменить тариф' }));

    await user.click(within(dialog()).getByRole('radio', { name: /1\s*vCPU, 1\s*ГБ RAM/ }));

    expect(
      await within(dialog()).findByText(
        'Уменьшение RAM в отказоустойчивом режиме очищает кеш полностью.',
        {},
        WAIT
      )
    ).toBeVisible();
  });

  it('предупреждает о потере последних записей при остальных изменениях ha', async () => {
    const instance = await seedInstance('valkey-1474', 'ha', 1, 1);
    const user = userEvent.setup();
    await openCard(instance);

    await user.click(screen.getByRole('button', { name: 'Изменить тариф' }));

    await user.click(within(dialog()).getByRole('radio', { name: /1\s*vCPU, 2\s*ГБ RAM/ }));

    expect(
      await within(dialog()).findByText(
        'При смене primary можно потерять последние записи.',
        {},
        WAIT
      )
    ).toBeVisible();
  });

  it('не даёт выйти за остаток квоты', async () => {
    const instance = await seedInstance('valkey-1474', 'single', 1, 1);
    await seedInstance('valkey-2222', 'single', 2, 8);
    const user = userEvent.setup();
    await openCard(instance);

    await user.click(screen.getByRole('button', { name: 'Изменить тариф' }));

    await user.click(within(dialog()).getByRole('radio', { name: /4\s*vCPU, 16\s*ГБ RAM/ }));

    expect(
      await within(dialog()).findByText(/Не хватает 2\s*vCPU и 8\s*ГБ/, {}, WAIT)
    ).toBeVisible();
    expect(within(dialog()).getByRole('button', { name: 'Изменить тариф' })).toBeDisabled();
  });

  it('оставляет текущую нестандартную конфигурацию среди карточек', async () => {
    const instance = await seedInstance('valkey-1474', 'single', 4, 8);
    const user = userEvent.setup();
    await openCard(instance);

    await user.click(screen.getByRole('button', { name: 'Изменить тариф' }));

    expect(within(dialog()).getByRole('radio', { name: /4\s*vCPU, 8\s*ГБ RAM/ })).toBeChecked();
    expect(within(dialog()).queryAllByRole('slider')).toHaveLength(0);
    expect(dialog()).not.toHaveTextContent('На ноду');
    expect(within(dialog()).getByRole('button', { name: 'Изменить тариф' })).toBeDisabled();
  });
});

describe('удаление', () => {
  it('называет базу в подтверждении и отменяется без потерь', async () => {
    const instance = await seedInstance('valkey-1474', 'single', 1, 1);
    const user = userEvent.setup();
    await openCard(instance);

    await user.click(screen.getByRole('button', { name: 'Действия с базой' }));
    await user.click(await screen.findByText('Удалить'));

    expect(dialog()).toHaveTextContent('Удалить базу valkey-1474?');

    await user.click(within(dialog()).getByRole('button', { name: 'Отмена' }));

    await expect(listInstances(TEST_USER_ID)).resolves.toHaveLength(1);
  });

  it('удаляет базу, освобождает квоту и приводит к пустому состоянию', async () => {
    const instance = await seedInstance('valkey-1474', 'single', 4, 16);
    const user = userEvent.setup();
    const { router } = await openCard(instance);

    await user.click(screen.getByRole('button', { name: 'Действия с базой' }));
    await user.click(await screen.findByText('Удалить'));
    await user.click(within(dialog()).getByRole('button', { name: 'Удалить базу' }));

    await waitFor(() => expect(router.state.location.pathname).toBe('/valkey/management'), WAIT);
    expect(
      await screen.findByRole('heading', { name: 'Управляемые базы Valkey на DDR5' }, WAIT)
    ).toBeVisible();
    await expect(listInstances(TEST_USER_ID)).resolves.toHaveLength(0);
  });
});
