/**
 * h3llo.cloud — фрагмент Mantine-темы.
 *
 * Реконструирован из собранного CSS app.h3llo.cloud. Это не копия исходника
 * продукта, а эквивалентное описание: те же имена токенов и те же значения,
 * которые видны в `<style data-mantine-styles="true">` на живой странице.
 *
 * Семантические токены (h3_bg, h3_text, h3_border и прочие) заведены как
 * virtualColor: Mantine разворачивает их в пары `<name>-light` / `<name>-dark`
 * и подставляет нужную по атрибуту data-mantine-color-scheme.
 *
 * Файл требует установленного @mantine/core. Вне проекта с этой зависимостью
 * tsc сообщит TS2307 на импорте: это ожидаемо, синтаксических ошибок в файле нет.
 */

import {
  createTheme,
  defaultVariantColorsResolver,
  rem,
  virtualColor,
  type MantineColorsTuple,
  type VariantColorsResolver,
} from '@mantine/core';

/** Токен одного цвета: Mantine требует кортеж из 10 ступеней. */
const mono = (hex: string): MantineColorsTuple =>
  Array.from({ length: 10 }, () => hex) as unknown as MantineColorsTuple;

const h3_gray: MantineColorsTuple = [
  '#F8F8F8', '#F2F2F2', '#E9E9E9', '#DAD9DA', '#B6B6B6',
  '#969696', '#6E6E6E', '#5A5A5A', '#3B3B3B', '#1B1B1B',
];

const h3_dark_gray: MantineColorsTuple = [
  '#F8F8F8', '#F3F3F3', '#E9E9E9', '#ADADAD', '#8F8F8F',
  '#787878', '#414141', '#373737', '#272727', '#1B1B1B',
];

const h3_mint: MantineColorsTuple = [
  '#E4FFEF', '#D5FDE7', '#B6FDD7', '#86FCBC', '#6CDB9F',
  '#51BA81', '#379A64', '#1C7946', '#025829', '#053C1E',
];

/**
 * Резолвер вариантов. Четыре варианта, которые реально встречаются в консоли:
 *
 *   h3_accent    — главное действие страницы. В светлой схеме чёрная кнопка
 *                  с белым текстом, в тёмной мятная с тёмным. Разница спрятана
 *                  в переменной --mantine-color-variant-h3_accent.
 *   h3_secondary — второстепенное действие на плоской поверхности.
 *   h3_stroke    — действие, которому нужна видимая граница (рядом с полем).
 *   h3_ghost     — действие без веса: иконки в панелях, пункты меню.
 */
export const variantColorResolver: VariantColorsResolver = (input) => {
  switch (input.variant) {
    case 'h3_accent':
      return {
        background: 'var(--mantine-color-variant-h3_accent)',
        hover: 'var(--mantine-color-variant-h3_accent-hover)',
        color: 'var(--mantine-color-h3_text_inverse)',
        border: `${rem(1)} solid transparent`,
      };
    case 'h3_secondary':
      return {
        background: 'var(--mantine-color-h3_bg_2)',
        hover: 'var(--mantine-color-h3_bg_3)',
        color: 'var(--mantine-color-text)',
        border: `${rem(1)} solid transparent`,
      };
    case 'h3_stroke':
      return {
        background: 'transparent',
        hover: 'var(--mantine-color-h3_bg_2)',
        color: 'var(--mantine-color-text)',
        border: `${rem(1)} solid var(--mantine-color-h3_border)`,
      };
    case 'h3_ghost':
      return {
        background: 'transparent',
        hover: 'var(--mantine-color-h3_bg_2)',
        color: 'var(--mantine-color-text)',
        border: `${rem(1)} solid transparent`,
      };
    default:
      return defaultVariantColorsResolver(input);
  }
};

export const theme = createTheme({
  primaryColor: 'h3_mint',
  primaryShade: { light: 3, dark: 3 },
  defaultRadius: 'h3_sm',
  fontFamily:
    "'manrope', -apple-system, BlinkMacSystemFont, Segoe UI, Roboto, Helvetica, Arial, sans-serif, Apple Color Emoji, Segoe UI Emoji",
  fontFamilyMonospace:
    "'robotoMono', ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, Liberation Mono, Courier New, monospace",
  headings: {
    fontFamily: 'var(--mantine-font-family, sans-serif)',
    fontWeight: '700',
    textWrap: 'balance',
    sizes: {
      h1: { fontSize: '1.875rem', lineHeight: '1.0666666666666667' },  // headline_sm 30px
      h2: { fontSize: '1.5625rem', lineHeight: '1.2' },                // headline_xs 25px
      h3: { fontSize: '1.3125rem', lineHeight: '1.8125rem' },          // h3_xl 21/29
      h4: { fontSize: '1.1875rem', lineHeight: '1.5625rem' },          // h3_lg 19/25
      h5: { fontSize: '1.0625rem', lineHeight: '1.4375rem' },          // h3_md 17/23
      h6: { fontSize: '0.9375rem', lineHeight: '1.1875rem' },          // h3_sm 15/19
    },
  },

  fontSizes: {
    h3_xs: '0.8125rem',
    h3_sm: '0.9375rem',
    h3_md: '1.0625rem',
    h3_lg: '1.1875rem',
    h3_xl: '1.3125rem',
    headline_xs: '1.5625rem',
    headline_sm: '1.875rem',
    headline_md: '2.1875rem',
    headline_lg: '2.5rem',
    headline_xl: '3.125rem',
  },
  lineHeights: {
    h3_xs: '0.9375rem',
    h3_sm: '1.1875rem',
    h3_md: '1.4375rem',
    h3_lg: '1.5625rem',
    h3_xl: '1.8125rem',
    headline_xs: '1.2',
    headline_sm: '1.0666666666666667',
    headline_md: '1.0285714285714285',
    headline_lg: '1',
    headline_xl: '1.0416666666666667',
  },
  spacing: {
    h3_xs: '0.5rem',
    h3_sm: '0.75rem',
    h3_md: '1rem',
    h3_lg: '1.5rem',
    h3_xl: '2rem',
  },
  radius: {
    h3_none: '0rem',
    h3_xs: '0.25rem',
    h3_sm: '0.375rem',
    h3_md: '0.5rem',
    h3_lg: '0.75rem',
    h3_xl: '1rem',
    h3_full: '624.9375rem',
  },
  shadows: {
    h3_default: '0px 1px 3px 0px rgb(from #1B1B1B r g b / 50%)',
    h3_primary:
      '0 2px 4px 0 rgba(0, 0, 0, 0.10), 0 0 8px 0 rgba(134, 252, 188, 0.5)',
  },
  breakpoints: {
    mobile: '48em',   // 768px
    tablet: '64em',   // 1024px
    wide: '88em',     // 1408px
    '1280': '80em',   // 1280px, порог мобильной правой панели
  },
  other: {
    fontWeights: { regular: 450, medium: 600, bold: 700 },
  },

  colors: {
    h3_gray,
    h3_dark_gray,
    h3_mint,

    // --- Поверхности ---
    'h3_bg-light': mono('#ffffff'),
    'h3_bg-dark': mono('#1B1B1B'),
    h3_bg: virtualColor({ name: 'h3_bg', light: 'h3_bg-light', dark: 'h3_bg-dark' }),

    'h3_bg_1-light': mono('#F8F8F8'),
    'h3_bg_1-dark': mono('#272727'),
    h3_bg_1: virtualColor({ name: 'h3_bg_1', light: 'h3_bg_1-light', dark: 'h3_bg_1-dark' }),

    'h3_bg_2-light': mono('#F2F2F2'),
    'h3_bg_2-dark': mono('#373737'),
    h3_bg_2: virtualColor({ name: 'h3_bg_2', light: 'h3_bg_2-light', dark: 'h3_bg_2-dark' }),

    'h3_bg_3-light': mono('#E9E9E9'),
    'h3_bg_3-dark': mono('#414141'),
    h3_bg_3: virtualColor({ name: 'h3_bg_3', light: 'h3_bg_3-light', dark: 'h3_bg_3-dark' }),

    'h3_bg_inverse-light': mono('#1B1B1B'),
    'h3_bg_inverse-dark': mono('#E9E9E9'),
    h3_bg_inverse: virtualColor({
      name: 'h3_bg_inverse',
      light: 'h3_bg_inverse-light',
      dark: 'h3_bg_inverse-dark',
    }),

    'h3_bg_inverse_1-light': mono('#5A5A5A'),
    'h3_bg_inverse_1-dark': mono('#ADADAD'),
    h3_bg_inverse_1: virtualColor({
      name: 'h3_bg_inverse_1',
      light: 'h3_bg_inverse_1-light',
      dark: 'h3_bg_inverse_1-dark',
    }),

    // --- Акцентные поверхности (мята одинакова в обеих схемах,
    //     кроме приглушённой bg_accent_1) ---
    'h3_bg_accent-light': mono('#86FCBC'),
    'h3_bg_accent-dark': mono('#86FCBC'),
    h3_bg_accent: virtualColor({
      name: 'h3_bg_accent',
      light: 'h3_bg_accent-light',
      dark: 'h3_bg_accent-dark',
    }),

    'h3_bg_accent_1-light': mono('#E4FFEF'),
    'h3_bg_accent_1-dark': mono('rgba(108, 219, 159, 0.4)'),
    h3_bg_accent_1: virtualColor({
      name: 'h3_bg_accent_1',
      light: 'h3_bg_accent_1-light',
      dark: 'h3_bg_accent_1-dark',
    }),

    'h3_bg_accent_2-light': mono('#B6FDD7'),
    'h3_bg_accent_2-dark': mono('#B6FDD7'),
    h3_bg_accent_2: virtualColor({
      name: 'h3_bg_accent_2',
      light: 'h3_bg_accent_2-light',
      dark: 'h3_bg_accent_2-dark',
    }),

    // --- Текст ---
    'h3_text-light': mono('#1B1B1B'),
    'h3_text-dark': mono('#ffffff'),
    h3_text: virtualColor({ name: 'h3_text', light: 'h3_text-light', dark: 'h3_text-dark' }),

    'h3_text_1-light': mono('#969696'),
    'h3_text_1-dark': mono('#787878'),
    h3_text_1: virtualColor({ name: 'h3_text_1', light: 'h3_text_1-light', dark: 'h3_text_1-dark' }),

    'h3_text_2-light': mono('#6E6E6E'),
    'h3_text_2-dark': mono('#8F8F8F'),
    h3_text_2: virtualColor({ name: 'h3_text_2', light: 'h3_text_2-light', dark: 'h3_text_2-dark' }),

    'h3_text_inverse-light': mono('#ffffff'),
    'h3_text_inverse-dark': mono('#1B1B1B'),
    h3_text_inverse: virtualColor({
      name: 'h3_text_inverse',
      light: 'h3_text_inverse-light',
      dark: 'h3_text_inverse-dark',
    }),

    'h3_text_accent-light': mono('#51BA81'),
    'h3_text_accent-dark': mono('#51BA81'),
    h3_text_accent: virtualColor({
      name: 'h3_text_accent',
      light: 'h3_text_accent-light',
      dark: 'h3_text_accent-dark',
    }),

    'h3_text_accent_1-light': mono('#86FCBC'),
    'h3_text_accent_1-dark': mono('#86FCBC'),
    h3_text_accent_1: virtualColor({
      name: 'h3_text_accent_1',
      light: 'h3_text_accent_1-light',
      dark: 'h3_text_accent_1-dark',
    }),

    // --- Границы ---
    'h3_border-light': mono('#E9E9E9'),
    'h3_border-dark': mono('#373737'),
    h3_border: virtualColor({ name: 'h3_border', light: 'h3_border-light', dark: 'h3_border-dark' }),

    'h3_border_1-light': mono('#B6B6B6'),
    'h3_border_1-dark': mono('#787878'),
    h3_border_1: virtualColor({
      name: 'h3_border_1',
      light: 'h3_border_1-light',
      dark: 'h3_border_1-dark',
    }),

    'h3_border_accent-light': mono('#6CDB9F'),
    'h3_border_accent-dark': mono('#6CDB9F'),
    h3_border_accent: virtualColor({
      name: 'h3_border_accent',
      light: 'h3_border_accent-light',
      dark: 'h3_border_accent-dark',
    }),
  },

  variantColorResolver,
});

/**
 * Переменные, которых нет в createTheme и которые задаются в глобальном CSS:
 *
 *   --mantine-color-variant-h3_accent
 *     light: var(--mantine-color-h3_bg_inverse)      // чёрная кнопка
 *     dark:  var(--mantine-primary-color-filled)     // мятная кнопка
 *
 *   --mantine-color-variant-h3_accent-hover
 *     light: var(--mantine-color-h3_bg_inverse_1)
 *     dark:  var(--mantine-primary-color-filled-hover)
 *
 *   --mantine-color-placeholder: var(--mantine-color-h3_text_1)
 *   --mantine-color-default-border: var(--mantine-color-h3_border)
 *   --mantine-color-anchor: var(--mantine-color-h3_mint-4)
 */
