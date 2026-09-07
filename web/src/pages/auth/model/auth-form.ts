import { ApiError } from '@/shared/api';

export function getEmailRequestError(error: unknown) {
  if (error instanceof ApiError && error.code === 'RATE_LIMITED') {
    const retryAfter = error.details?.retry_after;
    const suffix = typeof retryAfter === 'number' ? ` Повторите через ${retryAfter} с.` : '';
    return `Слишком много проверок.${suffix}`;
  }

  return error instanceof ApiError
    ? error.message
    : 'Не удалось выполнить запрос. Попробуйте ещё раз.';
}
