import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  renderValkeySection,
  seedSession,
  TEST_AUTHOR,
  TEST_USER_ID,
  TEST_VALKEY_PASSWORD,
  wholeText,
} from '../../../../test/render';
import { rotateValkeyPassword } from '../api/valkey-credentials';
import {
  createInstance,
  listInstances,
  readAuditSnapshot,
  VALKEY_INSTANCES_KEY,
} from '../api/valkey-storage';
import type { ValkeyInstance, ValkeyMode, ValkeyRamGb, ValkeyVcpu } from '../model/valkey';
import * as credentialsModel from '../model/valkey-credentials';

const WAIT = { timeout: 10_000 };

function seedInstance(name: string, mode: ValkeyMode, vcpu: ValkeyVcpu, ramGb: ValkeyRamGb) {
  return createInstance(TEST_AUTHOR, {
    name,
    prefix: 'valkey',
    mode,
    vcpu,
    ramGb,
    password: TEST_VALKEY_PASSWORD,
  });
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

afterEach(() => {
  vi.restoreAllMocks();
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

describe('учётные данные', () => {
  it('показывает пользователя, маску и сменяет пароль с одноразовой выдачей', async () => {
    const nextPassword = 'wxyzEFGHijklMNOPqrstUVWXyz01_234';
    const generate = vi
      .spyOn(credentialsModel, 'generateValkeyPassword')
      .mockReturnValue(nextPassword);
    const instance = await seedInstance('valkey-1474', 'single', 1, 2);
    const user = userEvent.setup();
    const copy = vi.spyOn(navigator.clipboard, 'writeText').mockResolvedValue();
    await openCard(instance);

    expect(await screen.findByText('app', {}, WAIT)).toBeVisible();
    expect(screen.getByText('abcd*****')).toBeVisible();
    expect(screen.queryByText('Готов')).toBeNull();

    await user.click(screen.getByRole('button', { name: 'Сменить пароль' }));

    const confirmation = screen.getByRole('dialog');
    expect(confirmation).toHaveTextContent(
      'Все подключения Valkey будут закрыты. Обновите пароль в приложениях.'
    );
    expect(generate).not.toHaveBeenCalled();

    await user.click(
      within(confirmation).getByRole('button', { name: 'Подтвердить смену пароля' })
    );

    const passwordField = await screen.findByRole('textbox', { name: 'Новый пароль' }, WAIT);
    expect(passwordField).toHaveValue(nextPassword);
    expect(screen.getByRole('dialog')).toHaveTextContent('Состояние: Применяется');
    expect(localStorage.getItem(VALKEY_INSTANCES_KEY)).not.toContain(nextPassword);

    await user.click(screen.getByRole('button', { name: 'Скопировать пароль' }));
    expect(copy).toHaveBeenCalledWith(nextPassword);
    await user.click(screen.getByRole('button', { name: 'Закрыть' }));

    const rotateButton = screen.getByRole('button', { name: 'Сменить пароль' });
    expect(rotateButton).toBeDisabled();
    expect(screen.getByText('Применяется')).toBeVisible();
    await waitFor(() => expect(rotateButton).toBeEnabled(), WAIT);
    expect(screen.queryByText('Применяется')).toBeNull();
    expect(screen.queryByDisplayValue(nextPassword)).toBeNull();
    expect(readAuditSnapshot(TEST_USER_ID, instance.id).map((item) => item.action)).toContain(
      'instance.password.rotate'
    );

    await user.click(rotateButton);
    expect(screen.getByRole('dialog')).toHaveTextContent('Подключения будут разорваны');
    expect(screen.queryByDisplayValue(nextPassword)).toBeNull();
  });

  it('оставляет остальную карточку доступной при ошибке учётных данных', async () => {
    const instance = await seedInstance('valkey-1474', 'single', 1, 2);
    await openCard(instance);
    const saved = localStorage.getItem(VALKEY_INSTANCES_KEY);

    localStorage.setItem(VALKEY_INSTANCES_KEY, '{"version":5,"instances":[{}]}');

    expect(await screen.findByText('Не удалось загрузить пароль', {}, WAIT)).toBeVisible();
    expect(screen.getByText('Текущий тариф')).toBeVisible();
    expect(screen.getByRole('button', { name: 'Скопировать адрес' })).toBeVisible();

    localStorage.setItem(VALKEY_INSTANCES_KEY, saved ?? '');
    await userEvent.click(screen.getByRole('button', { name: 'Повторить' }));

    expect(await screen.findByText('app', {}, WAIT)).toBeVisible();
  });

  it('предупреждает о задержке для базы с ограничениями', async () => {
    const instance = await seedInstance('valkey-1474', 'ha', 1, 2);
    const state = JSON.parse(localStorage.getItem(VALKEY_INSTANCES_KEY) ?? '{}') as {
      instances: Array<{ status: string }>;
    };
    state.instances[0].status = 'degraded';
    localStorage.setItem(VALKEY_INSTANCES_KEY, JSON.stringify(state));
    const user = userEvent.setup();

    await openCard(instance);
    await user.click(await screen.findByRole('button', { name: 'Сменить пароль' }, WAIT));

    expect(screen.getByRole('dialog')).toHaveTextContent(
      'Недоступный прежний процесс может задержать применение нового пароля.'
    );
  });

  it('обновляет метаданные после конфликта версии и не показывает непринятый пароль', async () => {
    const externalPassword = '2222EFGHijklMNOPqrstUVWXyz01_234';
    const rejectedPassword = '3333EFGHijklMNOPqrstUVWXyz01_234';
    vi.spyOn(credentialsModel, 'generateValkeyPassword').mockReturnValue(rejectedPassword);
    const instance = await seedInstance('valkey-1474', 'single', 1, 2);
    const user = userEvent.setup();
    await openCard(instance);
    await screen.findByRole('button', { name: 'Сменить пароль' }, WAIT);

    await rotateValkeyPassword(TEST_AUTHOR, instance.id, {
      password: externalPassword,
      expectedPasswordVersion: 1,
    });
    await user.click(screen.getByRole('button', { name: 'Сменить пароль' }));
    await user.click(
      within(screen.getByRole('dialog')).getByRole('button', {
        name: 'Подтвердить смену пароля',
      })
    );

    expect(await screen.findByText('Не удалось сменить пароль', {}, WAIT)).toBeVisible();
    expect(screen.queryByDisplayValue(rejectedPassword)).toBeNull();
    expect(await screen.findByText('Применяется', {}, WAIT)).toBeVisible();
  });

  it('не показывает пароль и не пишет аудит при ошибке запроса', async () => {
    const rejectedPassword = '6666EFGHijklMNOPqrstUVWXyz01_234';
    vi.spyOn(credentialsModel, 'generateValkeyPassword').mockReturnValue(rejectedPassword);
    const instance = await seedInstance('valkey-1474', 'single', 1, 2);
    const user = userEvent.setup();
    await openCard(instance);
    await screen.findByRole('button', { name: 'Сменить пароль' }, WAIT);
    const saved = localStorage.getItem(VALKEY_INSTANCES_KEY) ?? '';

    await user.click(screen.getByRole('button', { name: 'Сменить пароль' }));
    localStorage.setItem(VALKEY_INSTANCES_KEY, '{"version":5,"instances":[{}]}');
    await user.click(
      within(screen.getByRole('dialog')).getByRole('button', {
        name: 'Подтвердить смену пароля',
      })
    );

    expect(await screen.findByText('Не удалось сменить пароль', {}, WAIT)).toBeVisible();
    expect(screen.queryByDisplayValue(rejectedPassword)).toBeNull();

    localStorage.setItem(VALKEY_INSTANCES_KEY, saved);
    expect(readAuditSnapshot(TEST_USER_ID, instance.id).map((item) => item.action)).toEqual([
      'instance.create',
    ]);
  });

  it('не открывает пароль после закрытия ожидающего окна', async () => {
    const nextPassword = '4444EFGHijklMNOPqrstUVWXyz01_234';
    vi.spyOn(credentialsModel, 'generateValkeyPassword').mockReturnValue(nextPassword);
    const instance = await seedInstance('valkey-1474', 'single', 1, 2);
    const user = userEvent.setup();
    await openCard(instance);

    await user.click(await screen.findByRole('button', { name: 'Сменить пароль' }, WAIT));
    await user.click(
      within(screen.getByRole('dialog')).getByRole('button', {
        name: 'Подтвердить смену пароля',
      })
    );
    await user.keyboard('{Escape}');

    await waitFor(() => expect(readAuditSnapshot(TEST_USER_ID, instance.id)).toHaveLength(2), WAIT);
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(screen.queryByDisplayValue(nextPassword)).toBeNull();
  });

  it('очищает показанный пароль при pagehide и возврате из bfcache', async () => {
    const nextPassword = '5555EFGHijklMNOPqrstUVWXyz01_234';
    vi.spyOn(credentialsModel, 'generateValkeyPassword').mockReturnValue(nextPassword);
    const instance = await seedInstance('valkey-1474', 'single', 1, 2);
    const user = userEvent.setup();
    await openCard(instance);

    await user.click(await screen.findByRole('button', { name: 'Сменить пароль' }, WAIT));
    await user.click(
      within(screen.getByRole('dialog')).getByRole('button', {
        name: 'Подтвердить смену пароля',
      })
    );
    expect(await screen.findByDisplayValue(nextPassword, {}, WAIT)).toBeVisible();

    const pagehide = new Event('pagehide');
    Object.defineProperty(pagehide, 'persisted', { value: true });
    fireEvent(window, pagehide);

    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull(), WAIT);
    expect(screen.queryByDisplayValue(nextPassword)).toBeNull();
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
