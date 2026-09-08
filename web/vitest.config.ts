import { fileURLToPath, URL } from 'node:url';
import { defineConfig } from 'vitest/config';

export default defineConfig({
  resolve: {
    alias: {
      '@': fileURLToPath(new URL('./src', import.meta.url)),
    },
  },
  test: {
    // Модель, временный клиент и auth работают с localStorage и Web Crypto,
    // поэтому тестам нужна среда браузера, а не голый Node.
    environment: 'happy-dom',
    // Ширина окна по умолчанию: на ней правая панель раздела стоит колонкой,
    // а не выдвижным блоком.
    environmentOptions: { happyDOM: { width: 1440, height: 900 } },
    include: ['src/**/*.test.{ts,tsx}'],
    setupFiles: ['./vitest.setup.ts'],
    // Каждая операция временного клиента ждёт 400 мс, поэтому сценарий из
    // нескольких шагов не укладывается в стандартные 5 секунд.
    testTimeout: 20_000,
  },
});
