# AGENTS.md (web)

Инструкции для работы в каталоге `web`. Читать в таком порядке:

1. [../AGENTS.md](../AGENTS.md) — общие правила проекта, включая обязательный
   навык `humanizer` и правило хранения навыков в `../.skills`.
2. [DESIGN.md](DESIGN.md) — дизайн-гайд консоли: токены, раскладка, компоненты,
   текст, контраст, чеклист перед сдачей экрана.
3. [../.skills/feature-sliced-design/SKILL.md](../.skills/feature-sliced-design/SKILL.md)
   — методология FSD v2.1, по которой организован `src`.
4. Навыки Mantine по задаче:
   - [../.skills/mantine-form/SKILL.md](../.skills/mantine-form/SKILL.md) — формы
     на `@mantine/form`;
   - [../.skills/mantine-combobox/SKILL.md](../.skills/mantine-combobox/SKILL.md)
     — выпадающие списки и выбор на примитивах `Combobox`;
   - [../.skills/mantine-custom-components/SKILL.md](../.skills/mantine-custom-components/SKILL.md)
     — собственные компоненты с поддержкой темы и Styles API.

Требования к frontend в целом описаны в разделе «Frontend» файла
[../SYSTEM.md](../SYSTEM.md).

## Стек

React 19, TypeScript, Vite, React Router, Mantine 9 (`core`, `hooks`, `form`,
`charts`, `notifications`), Tailwind CSS 4 без Preflight, иконки `lucide-react`.
Пакеты ставит `pnpm` версии из поля `packageManager` в `package.json`.
Мы часто работаем в новом worktree, поэтому перед началом работы запускай
`pnpm install` в каталоге `web`.

## Команды

| Команда | Что делает |
|---|---|
| `pnpm dev` | сервер разработки, запросы на `/v1` проксируются в api |
| `pnpm build` | производственная сборка в `dist` |
| `pnpm preview` | раздача собранного `dist` |
| `pnpm typecheck` | проверка типов без генерации файлов |
| `pnpm lint:code` | Oxlint с конфигурацией `oxc-config-mantine` |
| `pnpm lint:fsd` | Steiger: границы и структура FSD |
| `pnpm lint` | обе проверки |
| `pnpm test` | Vitest: модель, временный клиент и экраны раздела |
| `just test` | тот же полный набор Vitest для корневого запуска и CI |
| `pnpm format` / `pnpm format:check` | Oxfmt |
| `pnpm check` | форматирование, обе проверки, типы, тесты и сборка подряд |

Адрес api для прокси задаётся переменной окружения `API_PROXY_TARGET`
(в оболочке или в `.env.local`), по умолчанию `http://127.0.0.1:8080`.

Перед завершением задачи `pnpm check` должен проходить с нулевым кодом.

## Структура src

FSD v2.1 с подходом pages-first: код сначала пишется в `pages`, а в `features`,
`entities` и `widgets` выносится только то, что стабильно используется больше
чем одной страницей. Пустые слои и срезы на будущее не создаются.

```
src/
  main.tsx          тонкая точка входа
  app/              App.tsx с провайдерами, routes/, styles/
  pages/<page>/     index.ts + ui/
  shared/config/    пути маршрутов, имена вариантов кнопок
```

Внешний импорт из среза идёт через его `index.ts`; внутри среза относительные
импорты. Псевдоним `@/*` указывает на `src/*`. Клиент api, когда появится,
живёт в `src/shared/api`, а не в `src/api`.

## Стили

Значения токенов есть только в `src/app/styles/tokens.css`. Тема Mantine и
слой Tailwind ссылаются на них, поэтому цвет в компоненте задаётся семантическим
токеном (`c="h3_text_2"` или класс `text-h3-text-2`), а не hex. Кнопки
используют варианты из `src/shared/config/variants.ts`. Оба способа оформления
проверяются в светлой и тёмной схеме.

## Оформление кода

Разбивай код пустыми строками на логические блоки. Отделяй импорты разных
источников, объявления, подготовку данных, обработчики и крупные части JSX.
Если форматтер допускает несколько вариантов, выбирай тот, в котором этапы
работы кода видны при беглом чтении.
