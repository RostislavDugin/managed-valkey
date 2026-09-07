import { oxfmt } from 'oxc-config-mantine';
import { defineConfig } from 'oxfmt';

export default defineConfig({
  ...oxfmt,
  ignorePatterns: [...oxfmt.ignorePatterns, 'dist', 'pnpm-lock.yaml'],
});
