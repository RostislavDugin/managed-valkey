import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';

function luminance(hex: string) {
  const channels = hex
    .slice(1)
    .match(/.{2}/g)!
    .map((value) => Number.parseInt(value, 16) / 255)
    .map((value) => (value <= 0.03928 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4));
  return channels[0] * 0.2126 + channels[1] * 0.7152 + channels[2] * 0.0722;
}

function contrast(left: string, right: string) {
  const lighter = Math.max(luminance(left), luminance(right));
  const darker = Math.min(luminance(left), luminance(right));
  return (lighter + 0.05) / (darker + 0.05);
}

describe('токены графиков', () => {
  it('для каждого ряда графика обеспечивает контраст не ниже 4,5 в светлой и тёмной схемах', () => {
    const css = readFileSync('src/app/styles/tokens.css', 'utf8');
    const colors = [...css.matchAll(/--h3-chart-[123]:\s*(#[0-9a-f]{6})/gi)].map(
      (match) => match[1]
    );

    expect(colors).toHaveLength(6);
    for (const color of colors.slice(0, 3)) {
      expect(contrast(color, '#ffffff')).toBeGreaterThanOrEqual(4.5);
    }
    for (const color of colors.slice(3)) {
      expect(contrast(color, '#1b1b1b')).toBeGreaterThanOrEqual(4.5);
    }
  });
});
