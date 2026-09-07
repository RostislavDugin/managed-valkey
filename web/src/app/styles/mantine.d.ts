import type { DefaultMantineColor, MantineColorsTuple } from '@mantine/core';

type H3Size = 'h3_xs' | 'h3_sm' | 'h3_md' | 'h3_lg' | 'h3_xl';
type H3Headline = 'headline_xs' | 'headline_sm' | 'headline_md' | 'headline_lg' | 'headline_xl';
type MantineSize = 'xs' | 'sm' | 'md' | 'lg' | 'xl';

type H3Colors =
  | 'h3_gray'
  | 'h3_dark_gray'
  | 'h3_mint'
  | 'h3_bg'
  | 'h3_bg_1'
  | 'h3_bg_2'
  | 'h3_bg_3'
  | 'h3_bg_inverse'
  | 'h3_bg_inverse_1'
  | 'h3_bg_accent'
  | 'h3_bg_accent_1'
  | 'h3_bg_accent_2'
  | 'h3_text'
  | 'h3_text_1'
  | 'h3_text_2'
  | 'h3_text_inverse'
  | 'h3_text_accent'
  | 'h3_text_accent_1'
  | 'h3_border'
  | 'h3_border_1'
  | 'h3_border_accent'
  | 'h3_border_control'
  | 'h3_border_focus';

declare module '@mantine/core' {
  export interface MantineThemeColorsOverride {
    colors: Record<DefaultMantineColor | H3Colors, MantineColorsTuple>;
  }

  export interface MantineThemeSizesOverride {
    fontSizes: Record<MantineSize | H3Size | H3Headline, string>;
    lineHeights: Record<MantineSize | H3Size | H3Headline, string>;
    spacing: Record<MantineSize | H3Size, string>;
    radius: Record<MantineSize | 'h3_none' | H3Size | 'h3_full', string>;
    shadows: Record<MantineSize | 'h3_default' | 'h3_primary', string>;
    breakpoints: Record<MantineSize | 'mobile' | 'tablet' | 'aside' | 'wide', string>;
  }
}
