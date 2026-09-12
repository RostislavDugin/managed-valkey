import { describe, expect, it } from 'vitest';
import {
  formatLocalMaintenance,
  getDefaultLocalMaintenance,
  getMaintenanceTimeOptions,
  localMaintenanceToUtc,
  utcMaintenanceToLocal,
} from './valkey-maintenance';

describe('окно обслуживания в часовом поясе пользователя', () => {
  it('преобразует воскресенье 04:00 UTC+3 в воскресенье 01:00 UTC и обратно', () => {
    const local = { dow: 0, time: '04:00', durationMin: 30 };

    const utc = localMaintenanceToUtc(local, -180);

    expect(utc).toEqual({ dow: 0, hourUtc: 1, durationMin: 30 });
    expect(utcMaintenanceToLocal(utc, -180)).toEqual(local);
  });

  it('переносит день недели через границу UTC', () => {
    expect(localMaintenanceToUtc({ dow: 0, time: '00:00', durationMin: 15 }, -180)).toEqual({
      dow: 6,
      hourUtc: 21,
      durationMin: 15,
    });
    expect(utcMaintenanceToLocal({ dow: 1, hourUtc: 23, durationMin: 60 }, -180)).toEqual({
      dow: 2,
      time: '02:00',
      durationMin: 60,
    });
  });

  it('предлагает только времена, совместимые с целым часом UTC', () => {
    const options = getMaintenanceTimeOptions(-330);

    expect(options).toHaveLength(24);
    expect(options[0]).toEqual({ value: '00:30', label: '00:30' });
    expect(options.at(-1)).toEqual({ value: '23:30', label: '23:30' });
    expect(localMaintenanceToUtc({ dow: 1, time: '04:30', durationMin: 30 }, -330)).toEqual({
      dow: 0,
      hourUtc: 23,
      durationMin: 30,
    });
  });

  it('выбирает ближайшее к четырём утра доступное местное время', () => {
    expect(getDefaultLocalMaintenance(-180)).toEqual({ dow: 0, time: '04:00', durationMin: 30 });
    expect(getDefaultLocalMaintenance(-330)).toEqual({ dow: 0, time: '04:30', durationMin: 30 });
  });

  it('форматирует ответ API в местном времени', () => {
    expect(formatLocalMaintenance({ dow: 0, hourUtc: 1, durationMin: 30 }, -180)).toBe(
      'Воскресенье, 04:00 по местному времени'
    );
  });
});
