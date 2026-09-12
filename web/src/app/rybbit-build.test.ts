import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { build } from 'vite';
import { expect, test } from 'vitest';

const WEB_ROOT = process.cwd();
const PROJECT_ROOT = join(WEB_ROOT, '..');
const RYBBIT_SCRIPT_URL = 'https://rybbit.databasus.com/api/script.js';

async function buildIndexHtml(siteId?: string) {
  const outputDirectory = await mkdtemp(join(tmpdir(), 'managed-valkey-web-build-'));
  const previousSiteId = process.env.RYBBIT_SITE_ID;

  if (siteId) {
    process.env.RYBBIT_SITE_ID = siteId;
  } else {
    delete process.env.RYBBIT_SITE_ID;
  }

  try {
    await build({
      root: WEB_ROOT,
      configFile: join(WEB_ROOT, 'vite.config.ts'),
      logLevel: 'silent',
      build: {
        emptyOutDir: true,
        outDir: outputDirectory,
      },
    });

    return await readFile(join(outputDirectory, 'index.html'), 'utf8');
  } finally {
    if (previousSiteId === undefined) {
      delete process.env.RYBBIT_SITE_ID;
    } else {
      process.env.RYBBIT_SITE_ID = previousSiteId;
    }

    await rm(outputDirectory, { recursive: true, force: true });
  }
}

test('локальная сборка не подключает Rybbit', async () => {
  const indexHtml = await buildIndexHtml();

  expect(indexHtml).not.toContain(RYBBIT_SCRIPT_URL);
});

test('рабочая сборка Caddy подключает Rybbit с заданным идентификатором сайта', async () => {
  const dockerfile = await readFile(join(PROJECT_ROOT, 'caddy/Dockerfile'), 'utf8');
  const siteId = dockerfile.match(/^RUN RYBBIT_SITE_ID=(\S+) pnpm build$/m)?.[1];

  expect(siteId).toBe('5e1f44c518f5');

  const indexHtml = await buildIndexHtml(siteId);

  expect(indexHtml).toContain(`src="${RYBBIT_SCRIPT_URL}"`);
  expect(indexHtml).toContain(`data-site-id="${siteId}"`);
  expect(indexHtml).toMatch(/<script[^>]+defer/);
});
