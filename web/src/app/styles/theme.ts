import {
  Container,
  createTheme,
  defaultVariantColorsResolver,
  rem,
  Slider,
  TextInput,
  type CSSVariablesResolver,
  type MantineColorsTuple,
  type VariantColorsResolver,
} from '@mantine/core';
import { buttonVariants } from '@/shared/config';

/** Кортеж из десяти одинаковых ступеней: семантический токен не имеет оттенков. */
const mono = (value: string): MantineColorsTuple =>
  Array.from({ length: 10 }, () => value) as unknown as MantineColorsTuple;

/** Семантический цвет, значение которого хранится в tokens.css и меняется вместе со схемой. */
const token = (name: string): MantineColorsTuple => mono(`var(--h3-${name})`);

const h3_gray: MantineColorsTuple = [
  '#F8F8F8',
  '#F2F2F2',
  '#E9E9E9',
  '#DAD9DA',
  '#B6B6B6',
  '#969696',
  '#6E6E6E',
  '#5A5A5A',
  '#3B3B3B',
  '#1B1B1B',
];

const h3_dark_gray: MantineColorsTuple = [
  '#F8F8F8',
  '#F3F3F3',
  '#E9E9E9',
  '#ADADAD',
  '#8F8F8F',
  '#787878',
  '#414141',
  '#373737',
  '#272727',
  '#1B1B1B',
];

const h3_mint: MantineColorsTuple = [
  '#E4FFEF',
  '#D5FDE7',
  '#B6FDD7',
  '#86FCBC',
  '#6CDB9F',
  '#51BA81',
  '#379A64',
  '#1C7946',
  '#025829',
  '#053C1E',
];

/**
 * Резолвер вариантов консоли. Цвета берутся из переменных tokens.css,
 * поэтому смена схемы не требует отдельной логики.
 */
export const variantColorResolver: VariantColorsResolver = (input) => {
  switch (input.variant) {
    case buttonVariants.accent:
      return {
        background: 'var(--h3-variant-accent)',
        hover: 'var(--h3-variant-accent-hover)',
        color: 'var(--h3-text-inverse)',
        border: `${rem(1)} solid transparent`,
      };
    case buttonVariants.secondary:
      return {
        background: 'var(--h3-bg-2)',
        hover: 'var(--h3-bg-3)',
        color: 'var(--h3-text)',
        border: `${rem(1)} solid transparent`,
      };
    case buttonVariants.stroke:
      return {
        background: 'transparent',
        hover: 'var(--h3-bg-2)',
        color: 'var(--h3-text)',
        border: `${rem(1)} solid var(--h3-border-control)`,
      };
    case buttonVariants.ghost:
      return {
        background: 'transparent',
        hover: 'var(--h3-bg-2)',
        color: 'var(--h3-text)',
        border: `${rem(1)} solid transparent`,
      };
    default:
      return defaultVariantColorsResolver(input);
  }
};

const schemeVariables = {
  '--mantine-color-body': 'var(--h3-bg)',
  '--mantine-color-text': 'var(--h3-text)',
  '--mantine-color-placeholder': 'var(--h3-placeholder)',
  '--mantine-color-dimmed': 'var(--h3-dimmed)',
  '--mantine-color-default-border': 'var(--h3-border)',
  '--mantine-color-anchor': 'var(--h3-text-accent)',
  '--mantine-color-error': 'var(--h3-error)',
};

/** Переменные Mantine, которых нет в createTheme. */
export const cssVariablesResolver: CSSVariablesResolver = () => ({
  // Начертания Mantine спрашивает у своих переменных: без них подпись поля
  // теряет 600 и набирается тем же весом, что и основной текст.
  variables: {
    '--mantine-font-weight-regular': 'var(--h3-fw-regular)',
    '--mantine-font-weight-medium': 'var(--h3-fw-medium)',
    '--mantine-font-weight-bold': 'var(--h3-fw-bold)',
  },
  // Mantine задаёт эти переменные отдельно для каждой схемы, поэтому
  // переопределять их нужно в обоих блоках, а не в общем.
  light: schemeVariables,
  dark: schemeVariables,
});

export const theme = createTheme({
  primaryColor: 'h3_mint',
  primaryShade: { light: 3, dark: 3 },
  defaultRadius: 'h3_sm',
  fontFamily: 'var(--h3-font-family)',
  fontFamilyMonospace: 'var(--h3-font-family-mono)',
  headings: {
    fontFamily: 'var(--h3-font-family)',
    fontWeight: 'var(--h3-fw-bold)',
    textWrap: 'balance',
    sizes: {
      h1: { fontSize: 'var(--h3-headline-sm)', lineHeight: 'var(--h3-headline-lh-sm)' },
      h2: { fontSize: 'var(--h3-headline-xs)', lineHeight: 'var(--h3-headline-lh-xs)' },
      h3: { fontSize: 'var(--h3-fs-xl)', lineHeight: 'var(--h3-lh-xl)' },
      h4: { fontSize: 'var(--h3-fs-lg)', lineHeight: 'var(--h3-lh-lg)' },
      h5: { fontSize: 'var(--h3-fs-md)', lineHeight: 'var(--h3-lh-md)' },
      h6: { fontSize: 'var(--h3-fs-sm)', lineHeight: 'var(--h3-lh-sm)' },
    },
  },

  fontSizes: {
    h3_xs: 'var(--h3-fs-xs)',
    h3_sm: 'var(--h3-fs-sm)',
    h3_md: 'var(--h3-fs-md)',
    h3_lg: 'var(--h3-fs-lg)',
    h3_xl: 'var(--h3-fs-xl)',
    headline_xs: 'var(--h3-headline-xs)',
    headline_sm: 'var(--h3-headline-sm)',
    headline_md: 'var(--h3-headline-md)',
    headline_lg: 'var(--h3-headline-lg)',
    headline_xl: 'var(--h3-headline-xl)',
  },
  lineHeights: {
    h3_xs: 'var(--h3-lh-xs)',
    h3_sm: 'var(--h3-lh-sm)',
    h3_md: 'var(--h3-lh-md)',
    h3_lg: 'var(--h3-lh-lg)',
    h3_xl: 'var(--h3-lh-xl)',
    headline_xs: 'var(--h3-headline-lh-xs)',
    headline_sm: 'var(--h3-headline-lh-sm)',
    headline_md: 'var(--h3-headline-lh-md)',
    headline_lg: 'var(--h3-headline-lh-lg)',
    headline_xl: 'var(--h3-headline-lh-xl)',
  },
  spacing: {
    h3_xs: 'var(--h3-space-xs)',
    h3_sm: 'var(--h3-space-sm)',
    h3_md: 'var(--h3-space-md)',
    h3_lg: 'var(--h3-space-lg)',
    h3_xl: 'var(--h3-space-xl)',
  },
  radius: {
    h3_none: 'var(--h3-radius-none)',
    h3_xs: 'var(--h3-radius-xs)',
    h3_sm: 'var(--h3-radius-sm)',
    h3_md: 'var(--h3-radius-md)',
    h3_lg: 'var(--h3-radius-lg)',
    h3_xl: 'var(--h3-radius-xl)',
    h3_full: 'var(--h3-radius-full)',
  },
  shadows: {
    h3_default: 'var(--h3-shadow-default)',
    h3_primary: 'var(--h3-shadow-primary)',
  },
  breakpoints: {
    mobile: '48em',
    tablet: '64em',
    aside: '80em',
    wide: '88em',
  },
  other: {
    fontWeights: { regular: 450, medium: 600, bold: 700 },
  },

  components: {
    /** Боковой отступ страницы 24px: столько же между колонкой навигации и текстом в оригинале консоли. */
    Container: Container.extend({ defaultProps: { px: 'h3_lg' } }),

    /** Поле формы: высота 42px, поля 14px и кегль 16px из размера `md`. */
    TextInput: TextInput.extend({ defaultProps: { size: 'md' } }),

    /** Ползунок размера `xl` с радиусом 8px даёт квадратную ручку 24px. */
    Slider: Slider.extend({ defaultProps: { radius: 'h3_md', size: 'xl' } }),
  },

  colors: {
    h3_gray,
    h3_dark_gray,
    h3_mint,

    h3_bg: token('bg'),
    h3_bg_1: token('bg-1'),
    h3_bg_2: token('bg-2'),
    h3_bg_3: token('bg-3'),
    h3_bg_inverse: token('bg-inverse'),
    h3_bg_inverse_1: token('bg-inverse-1'),
    h3_bg_accent: token('bg-accent'),
    h3_bg_accent_1: token('bg-accent-1'),
    h3_bg_accent_2: token('bg-accent-2'),

    h3_text: token('text'),
    h3_text_1: token('text-1'),
    h3_text_2: token('text-2'),
    h3_text_inverse: token('text-inverse'),
    h3_text_accent: token('text-accent'),
    h3_text_accent_1: token('text-accent-1'),

    h3_border: token('border'),
    h3_border_1: token('border-1'),
    h3_border_accent: token('border-accent'),
    h3_border_control: token('border-control'),
    h3_border_focus: token('border-focus'),
  },

  variantColorResolver,
});
