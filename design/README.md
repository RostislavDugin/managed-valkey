# Стайл-гайд h3llo.cloud

Описание дизайн-системы h3llo.cloud: консоль `app.h3llo.cloud` на Mantine и лендинг
`h3llo.cloud` на Tailwind. Гайд собран по разметке и собранным стилям живых страниц,
значения перенесены как есть.

## Начать здесь

Открой [preview/index.html](preview/index.html) двойным кликом. Это витрина всех
токенов и компонентов с переключателем светлой и тёмной схемы. Дальше читай разделы
по мере надобности.

## Разделы

**Основы**

- [foundations/color.md](foundations/color.md) — палитры, семантические токены, правила выбора цвета
- [foundations/typography.md](foundations/typography.md) — шрифты, две шкалы, карта заголовков
- [foundations/space-radius-elevation.md](foundations/space-radius-elevation.md) — отступы, радиусы, тени, границы
- [foundations/layout.md](foundations/layout.md) — каркас консоли, брейкпоинты, сетка формы
- [foundations/motion.md](foundations/motion.md) — длительности, кривые, уменьшенное движение

**Компоненты**

- [components/buttons.md](components/buttons.md) — четыре варианта кнопок и когда какой
- [components/forms.md](components/forms.md) — поля, списки, радио, слайдер, переключатель
- [components/navigation.md](components/navigation.md) — навигация, крошки, сегментированный контрол
- [components/feedback.md](components/feedback.md) — уведомления, модальные окна, состояния загрузки и ошибок

**Паттерны**

- [patterns/console-page.md](patterns/console-page.md) — каркас страницы консоли и её четыре состояния
- [patterns/resource-form.md](patterns/resource-form.md) — форма создания ресурса с живым расчётом
- [patterns/marketing-site.md](patterns/marketing-site.md) — секции лендинга и его приёмы

**Текст**

- [content/voice-and-tone.md](content/voice-and-tone.md) — регистр, обращение, термины, формат чисел

**Качество**

- [quality/contrast-audit.md](quality/contrast-audit.md) — посчитанный контраст и что чинить
- [quality/checklist.md](quality/checklist.md) — что проверить перед сдачей экрана

**Токены**

- [tokens/tokens.css](tokens/tokens.css) — CSS-переменные для обеих схем
- [tokens/tokens.json](tokens/tokens.json) — тот же набор для Figma и Style Dictionary
- [tokens/mantine-theme.ts](tokens/mantine-theme.ts) — фрагмент `createTheme()`
- [tokens/tailwind-brand.css](tokens/tailwind-brand.css) — слой лендинга

## Три правила, если читать некогда

**Цвет берётся из семантики, не из палитры.** `h3_text_2`, а не `#6E6E6E`. Тогда
тёмная тема получится сама.

**Главная кнопка — вариант `h3_accent`, и красить её вручную не надо.** В светлой
схеме она чёрная, в тёмной мятная, и это разные цвета за одной переменной.

**Мята означает состояние и деньги, а не действие.** Выбрано, включено, столько стоит.
Кнопку «сделать» она заливает только в тёмной схеме.

## Что здесь считается фактом, а что предположением

Значения токенов консоли, имена вариантов компонентов, размеры каркаса и шкалы взяты
из собранного CSS и разметки живых страниц. Это факт.

Значения брендовых переменных лендинга (`--color-brand-*`) в доступной разметке видны
только по именам: сам файл стилей внешний. В [tokens/tailwind-brand.css](tokens/tailwind-brand.css)
они помечены как гипотеза, там же написано, как проверить. То же касается таблицы
соответствия классов лендинга типографической шкале консоли.

Рекомендации по контрасту, доступности и обратной связи — это рекомендации, а не
описание текущего состояния. Там, где продукт им не соответствует, это сказано прямо.

## Как поддерживать

Токены меняются в продукте, а не здесь. Если тема консоли изменилась, обнови
`tokens/tokens.css` и `tokens/tokens.json`, пересчитай таблицы в
`quality/contrast-audit.md` (скрипт приведён в конце файла) и проверь витрину.

Раздел, описывающий то, чего в продукте ещё нет, помечай явно. Гайд, который
расходится с продуктом молча, хуже отсутствующего.
