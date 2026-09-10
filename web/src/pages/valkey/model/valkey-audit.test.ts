import { describe, expect, it } from 'vitest';
import {
  AUDIT_ACTION_LABELS,
  getAuditGroupLabel,
  groupAuditLogs,
  type AuditLogEntry,
} from './valkey-audit';

function localIso(year: number, month: number, day: number, hour = 12) {
  return new Date(year, month - 1, day, hour).toISOString();
}

function event(id: string, createdAt: string): AuditLogEntry {
  return {
    id,
    action: 'instance.create',
    userEmail: 'user@example.com',
    createdAt,
  };
}

describe('модель аудита Valkey', () => {
  it('для каждого действия аудита возвращает понятную русскую подпись и подходящую пиктограмму', () => {
    expect(AUDIT_ACTION_LABELS).toEqual({
      'instance.create': 'Создана база',
      'instance.update': 'Изменены настройки',
      'instance.resize': 'Изменён тариф',
      'instance.whitelist.update': 'Изменён доступ по IP',
      'instance.password.rotate': 'Изменён пароль',
      'instance.delete': 'Удалена база',
    });
  });

  it('для событий по разные стороны полуночи создаёт группы по местной календарной дате', () => {
    const now = new Date(2026, 8, 8, 0, 5);
    const today = event('today', localIso(2026, 9, 8, 0));
    const yesterday = event('yesterday', localIso(2026, 9, 7, 23));

    expect(groupAuditLogs([today, yesterday], now).map((group) => group.label)).toEqual([
      'Сегодня',
      'Вчера',
    ]);
  });

  it('при форматировании группы добавляет год только для события не из текущего года', () => {
    const now = new Date(2026, 8, 8, 12);

    expect(getAuditGroupLabel(localIso(2026, 8, 20), now)).not.toContain('2026');
    expect(getAuditGroupLabel(localIso(2025, 8, 20), now)).toContain('2025');
  });
});
