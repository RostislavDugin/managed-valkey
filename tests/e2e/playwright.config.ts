import { defineConfig, devices } from '@playwright/test';

const runId = (process.env.MV_RUN_ID ?? `manual-${process.pid}`).replace(/[^a-zA-Z0-9-]/g, '-');
const artifactRoot = `../../tmp/playwright/e2e/${runId}`;

export default defineConfig({
  expect: {
    timeout: 10_000,
  },
  forbidOnly: Boolean(process.env.CI),
  fullyParallel: false,
  outputDir: `${artifactRoot}/test-results`,
  projects: [
    {
      name: 'chromium',
      use: {
        ...devices['Desktop Chrome'],
        viewport: { width: 1440, height: 800 },
      },
    },
  ],
  reporter: [['line'], ['html', { open: 'never', outputFolder: `${artifactRoot}/report` }]],
  retries: 0,
  testDir: './specs',
  timeout: 35 * 60_000,
  use: {
    actionTimeout: 30_000,
    baseURL: process.env.PLAYWRIGHT_BASE_URL ?? 'http://127.0.0.1:5173',
    navigationTimeout: 30_000,
    screenshot: 'off',
    trace: 'off',
  },
  workers: 1,
});
