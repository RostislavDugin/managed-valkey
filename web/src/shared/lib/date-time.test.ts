import { afterEach, describe, expect, it, vi } from 'vitest';
import { formatDateTime, formatRelativeTime } from './date-time';

afterEach(() => {
  vi.useRealTimers();
});

describe('форматирование времени', () => {
  it('показывает точную дату и время в русской локали', () => {
    const value = '2026-09-08T12:00:00.000Z';
    const expected = new Intl.DateTimeFormat('ru-RU', {
      dateStyle: 'medium',
      timeStyle: 'short',
    }).format(new Date(value));

    expect(formatDateTime(value)).toBe(expected);
  });

  it('считает относительное время от текущих часов', () => {
    vi.useFakeTimers();
    vi.setSystemTime('2026-09-08T12:05:00.000Z');

    expect(formatRelativeTime('2026-09-08T12:00:00.000Z')).toBe('5 минут назад');
  });
});
