/** Варианты кнопок и иконочных кнопок консоли. Выбираются по весу действия. */
export const buttonVariants = {
  /** Главное действие страницы, одно на экран. Чёрная в светлой схеме, мятная в тёмной. */
  accent: 'h3_accent',
  /** Частое второстепенное действие на плоской поверхности. */
  secondary: 'h3_secondary',
  /** Действие рядом с полем, которому нужна видимая граница. */
  stroke: 'h3_stroke',
  /** Действие без веса: иконки в панелях, пункты меню. */
  ghost: 'h3_ghost',
} as const;

export type ButtonVariantName = (typeof buttonVariants)[keyof typeof buttonVariants];
