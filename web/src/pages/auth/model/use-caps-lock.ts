import { useEffect, useState } from 'react';

/**
 * Сообщает, включён ли CapsLock.
 *
 * Браузер отдаёт состояние модификатора только вместе с событием клавиатуры или
 * указателя, отдельного способа спросить его нет. Поэтому подписка слушает всю
 * страницу: если слушать только само поле пароля, состояние станет известно лишь
 * после нажатия клавиши внутри него, а до этого подсказка не появится. До первого
 * события на странице значение равно false.
 */
export function useCapsLock() {
  const [enabled, setEnabled] = useState(false);

  useEffect(() => {
    const read = (event: KeyboardEvent | PointerEvent) => {
      setEnabled(event.getModifierState('CapsLock'));
    };

    window.addEventListener('keydown', read);
    window.addEventListener('keyup', read);
    window.addEventListener('pointerdown', read);

    return () => {
      window.removeEventListener('keydown', read);
      window.removeEventListener('keyup', read);
      window.removeEventListener('pointerdown', read);
    };
  }, []);

  return enabled;
}
