export type AuditAction =
  | 'instance.create'
  | 'instance.update'
  | 'instance.resize'
  | 'instance.whitelist.update'
  | 'instance.password.rotate'
  | 'instance.delete';

export interface AuditLogEntry {
  id: string;
  action: AuditAction;
  userEmail: string;
  createdAt: string;
}

export interface AuditLogPage {
  items: AuditLogEntry[];
  nextCursor: string | null;
}

export interface AuditLogGroup {
  key: string;
  label: string;
  items: AuditLogEntry[];
}

export const AUDIT_ACTION_LABELS: Record<AuditAction, string> = {
  'instance.create': 'Создана база',
  'instance.update': 'Изменены настройки',
  'instance.resize': 'Изменён тариф',
  'instance.whitelist.update': 'Изменён доступ по IP',
  'instance.password.rotate': 'Изменён пароль',
  'instance.delete': 'Удалена база',
};

const DAY_FORMAT = new Intl.DateTimeFormat('ru-RU', { day: 'numeric', month: 'short' });
const DAY_WITH_YEAR_FORMAT = new Intl.DateTimeFormat('ru-RU', {
  day: 'numeric',
  month: 'short',
  year: 'numeric',
});

function localDateKey(value: string | Date) {
  const date = value instanceof Date ? value : new Date(value);
  const year = date.getFullYear();
  const month = String(date.getMonth() + 1).padStart(2, '0');
  const day = String(date.getDate()).padStart(2, '0');

  return `${year}-${month}-${day}`;
}

export function getAuditGroupLabel(value: string | Date, now = new Date()) {
  const date = value instanceof Date ? value : new Date(value);
  const yesterday = new Date(now);
  yesterday.setDate(yesterday.getDate() - 1);

  if (localDateKey(date) === localDateKey(now)) {
    return 'Сегодня';
  }

  if (localDateKey(date) === localDateKey(yesterday)) {
    return 'Вчера';
  }

  return date.getFullYear() === now.getFullYear()
    ? DAY_FORMAT.format(date)
    : DAY_WITH_YEAR_FORMAT.format(date);
}

export function groupAuditLogs(items: AuditLogEntry[], now = new Date()) {
  const groups = new Map<string, AuditLogGroup>();

  for (const item of items) {
    const key = localDateKey(item.createdAt);
    const group = groups.get(key);

    if (group) {
      group.items.push(item);
    } else {
      groups.set(key, { key, label: getAuditGroupLabel(item.createdAt, now), items: [item] });
    }
  }

  return [...groups.values()];
}
