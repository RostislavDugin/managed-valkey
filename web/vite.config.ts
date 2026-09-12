import { fileURLToPath, URL } from 'node:url';
import tailwindcss from '@tailwindcss/vite';
import react from '@vitejs/plugin-react';
import { defineConfig, loadEnv, type Plugin, type PluginOption } from 'vite';

// Адрес api для прокси `/v1` в режиме разработки. Переопределяется переменной
// окружения API_PROXY_TARGET (в оболочке или в `.env.local`).
const DEFAULT_API_PROXY_TARGET = 'http://127.0.0.1:8080';

function rybbitAnalytics(siteId: string): Plugin {
  return {
    name: 'rybbit-analytics',
    transformIndexHtml: {
      order: 'post',
      handler: () => [
        {
          tag: 'script',
          attrs: {
            src: 'https://rybbit.databasus.com/api/script.js',
            'data-site-id': siteId,
            defer: true,
          },
          injectTo: 'head',
        },
      ],
    },
  };
}

export default defineConfig(({ command, mode }) => {
  const env = loadEnv(mode, process.cwd(), '');
  const apiProxyTarget = env.API_PROXY_TARGET || DEFAULT_API_PROXY_TARGET;
  const rybbitSiteId = env.RYBBIT_SITE_ID;
  const plugins: PluginOption[] = [react(), tailwindcss()];

  if (command === 'build' && mode === 'production' && rybbitSiteId) {
    plugins.push(rybbitAnalytics(rybbitSiteId));
  }

  return {
    plugins,
    resolve: {
      alias: {
        '@': fileURLToPath(new URL('./src', import.meta.url)),
      },
    },
    server: {
      proxy: {
        '/v1': {
          target: apiProxyTarget,
          changeOrigin: true,
        },
      },
    },
  };
});
