import type { ReactNode } from 'react';
import { Select, SimpleGrid } from '@mantine/core';
import { getMaintenanceTimeOptions, MAINTENANCE_WEEKDAYS } from '../model/valkey-maintenance';

interface MaintenanceWindowFieldsProps {
  day: string;
  dayError?: ReactNode;
  onDayChange: (value: string | null) => void;
  onTimeChange: (value: string | null) => void;
  time: string;
  timeError?: ReactNode;
  timezoneOffsetMinutes: number;
}

export function MaintenanceWindowFields({
  day,
  dayError,
  onDayChange,
  onTimeChange,
  time,
  timeError,
  timezoneOffsetMinutes,
}: MaintenanceWindowFieldsProps) {
  return (
    <SimpleGrid cols={{ base: 1, mobile: 2 }} spacing="h3_sm">
      <Select
        allowDeselect={false}
        data={MAINTENANCE_WEEKDAYS}
        error={dayError}
        label="День недели"
        onChange={onDayChange}
        value={day}
      />
      <Select
        allowDeselect={false}
        data={getMaintenanceTimeOptions(timezoneOffsetMinutes)}
        error={timeError}
        label="Время начала, местное"
        onChange={onTimeChange}
        value={time}
      />
    </SimpleGrid>
  );
}
