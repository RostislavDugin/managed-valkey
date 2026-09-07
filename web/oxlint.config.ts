import { oxlint } from 'oxc-config-mantine';
import { defineConfig } from 'oxlint';

export default defineConfig({
  extends: [oxlint],
  ignorePatterns: ['dist', '**/*.{mjs,cjs,js,d.ts,d.mts}'],
  rules: {
    'react/rules-of-hooks': 'error',
  },
});
