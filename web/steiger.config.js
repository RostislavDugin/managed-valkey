// @ts-check
// Обычный JS, а не TS: загрузчик конфигурации Steiger (cosmiconfig) не умеет
// читать `.ts` через TypeScript 7.
import fsd from '@feature-sliced/steiger-plugin';
import { defineConfig } from 'steiger';

export default defineConfig([...fsd.configs.recommended]);
