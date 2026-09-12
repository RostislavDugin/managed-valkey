import type { ValkeyMaintenance } from './valkey';

const MINUTES_PER_HOUR = 60;
const MINUTES_PER_DAY = 24 * MINUTES_PER_HOUR;
const MINUTES_PER_WEEK = 7 * MINUTES_PER_DAY;
const DEFAULT_LOCAL_START_MINUTES = 4 * MINUTES_PER_HOUR;

export const MAINTENANCE_HINT =
  'В это время мы обновляем кластер. Обновления обычно проходят примерно раз в месяц. Мы постараемся провести их без перерыва в работе, но краткая недоступность возможна. Время указано в вашем часовом поясе.';

export interface LocalMaintenance {
  dow: number;
  time: string;
  durationMin: number;
}

export const MAINTENANCE_WEEKDAYS = [
  { value: '0', label: 'Воскресенье' },
  { value: '1', label: 'Понедельник' },
  { value: '2', label: 'Вторник' },
  { value: '3', label: 'Среда' },
  { value: '4', label: 'Четверг' },
  { value: '5', label: 'Пятница' },
  { value: '6', label: 'Суббота' },
] as const;

function normalizeMinutes(value: number, period: number) {
  return ((value % period) + period) % period;
}

function formatTime(minutes: number) {
  const hours = Math.floor(minutes / MINUTES_PER_HOUR);
  const minute = minutes % MINUTES_PER_HOUR;
  return `${String(hours).padStart(2, '0')}:${String(minute).padStart(2, '0')}`;
}

function parseTime(value: string) {
  const match = /^(\d{2}):(\d{2})$/.exec(value);
  if (!match) {
    return null;
  }

  const hours = Number(match[1]);
  const minutes = Number(match[2]);
  return hours <= 23 && minutes <= 59 ? hours * MINUTES_PER_HOUR + minutes : null;
}

export function getMaintenanceTimeOptions(timezoneOffsetMinutes: number) {
  const values = Array.from({ length: 24 }, (_, hourUtc) =>
    normalizeMinutes(hourUtc * MINUTES_PER_HOUR - timezoneOffsetMinutes, MINUTES_PER_DAY)
  );

  return [...new Set(values)]
    .sort((left, right) => left - right)
    .map((minutes) => ({ value: formatTime(minutes), label: formatTime(minutes) }));
}

export function getDefaultLocalMaintenance(timezoneOffsetMinutes: number): LocalMaintenance {
  const options = getMaintenanceTimeOptions(timezoneOffsetMinutes);
  const time = options.reduce((nearest, option) => {
    const nearestMinutes = parseTime(nearest.value) ?? 0;
    const optionMinutes = parseTime(option.value) ?? 0;
    const nearestDistance = Math.abs(nearestMinutes - DEFAULT_LOCAL_START_MINUTES);
    const optionDistance = Math.abs(optionMinutes - DEFAULT_LOCAL_START_MINUTES);

    return optionDistance < nearestDistance ||
      (optionDistance === nearestDistance && optionMinutes > nearestMinutes)
      ? option
      : nearest;
  }).value;

  return { dow: 0, time, durationMin: 30 };
}

export function localMaintenanceToUtc(
  value: LocalMaintenance,
  timezoneOffsetMinutes: number
): ValkeyMaintenance {
  const localTimeMinutes = parseTime(value.time);
  if (localTimeMinutes === null) {
    throw new Error('Некорректное местное время окна обслуживания');
  }

  const utcMinutes = normalizeMinutes(
    value.dow * MINUTES_PER_DAY + localTimeMinutes + timezoneOffsetMinutes,
    MINUTES_PER_WEEK
  );
  if (utcMinutes % MINUTES_PER_HOUR !== 0) {
    throw new Error('Местное время нельзя представить целым часом UTC');
  }

  return {
    dow: Math.floor(utcMinutes / MINUTES_PER_DAY),
    hourUtc: (utcMinutes % MINUTES_PER_DAY) / MINUTES_PER_HOUR,
    durationMin: value.durationMin,
  };
}

export function utcMaintenanceToLocal(
  value: ValkeyMaintenance,
  timezoneOffsetMinutes: number
): LocalMaintenance {
  const localMinutes = normalizeMinutes(
    value.dow * MINUTES_PER_DAY + value.hourUtc * MINUTES_PER_HOUR - timezoneOffsetMinutes,
    MINUTES_PER_WEEK
  );

  return {
    dow: Math.floor(localMinutes / MINUTES_PER_DAY),
    time: formatTime(localMinutes % MINUTES_PER_DAY),
    durationMin: value.durationMin,
  };
}

export function validateLocalMaintenanceTime(value: string, timezoneOffsetMinutes: number) {
  return getMaintenanceTimeOptions(timezoneOffsetMinutes).some((option) => option.value === value)
    ? null
    : 'Выберите время из списка';
}

export function formatLocalMaintenance(value: ValkeyMaintenance, timezoneOffsetMinutes: number) {
  const local = utcMaintenanceToLocal(value, timezoneOffsetMinutes);
  const weekday = MAINTENANCE_WEEKDAYS[local.dow]?.label ?? `День ${local.dow}`;
  return `${weekday}, ${local.time} по местному времени`;
}
