import { defineConfig, type Plugin } from 'vite';
import { resolve } from 'path';
import { createHash } from 'crypto';
import { readFileSync, mkdirSync, copyFileSync } from 'fs';

// Background effects [SL-P1-7]: MediaPipe's segmentation runtime is served
// from our own origin (CSP default-src 'self'; no CDN). The WASM comes from
// the pinned @mediapipe/tasks-vision package (pnpm-lock integrity) and is
// checked against these SHA-256s before it is copied into the build. The
// ~250 KB selfie model is committed in web/public/mediapipe/.
const MEDIAPIPE_VERSION = '1.0.1';
const MEDIAPIPE_FILES: Record<string, string> = {
  'vision_wasm_internal.js': 'e170ee67dd4e16c1a6fcd8840a206687e5a59b22c20e4a902bc445b095454d73',
  'vision_wasm_internal.wasm': '8da277a733926eacd0474b8704b36742d6ec3231c57a860c5b889dff8f1df886',
};

function mediapipeRuntime(): Plugin {
  const src = resolve(__dirname, 'node_modules/@mediapipe/tasks-vision/wasm');
  const verified = (name: string) => {
    const file = resolve(src, name);
    const data = readFileSync(file);
    const sum = createHash('sha256').update(data).digest('hex');
    if (sum !== MEDIAPIPE_FILES[name]) throw new Error(`@mediapipe/tasks-vision@${MEDIAPIPE_VERSION} ${name}: SHA-256 ${sum} does not match the pinned ${MEDIAPIPE_FILES[name]}`);
    return { file, data };
  };
  let outDir = '';
  return {
    name: 'studiolink-mediapipe-runtime',
    configResolved(config) { outDir = resolve(config.root, config.build.outDir); },
    // vite dev: serve the verified files at /mediapipe/.
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        const name = req.url?.match(/^\/mediapipe\/(vision_wasm_internal\.(?:js|wasm))$/)?.[1];
        if (!name) return next();
        res.setHeader('Content-Type', name.endsWith('.wasm') ? 'application/wasm' : 'text/javascript');
        res.end(verified(name).data);
      });
    },
    closeBundle() {
      mkdirSync(resolve(outDir, 'mediapipe'), { recursive: true });
      for (const name of Object.keys(MEDIAPIPE_FILES)) copyFileSync(verified(name).file, resolve(outDir, 'mediapipe', name));
    },
  };
}

export default defineConfig({
  plugins: [mediapipeRuntime()],
  build: {
    outDir: resolve(__dirname, '../host/internal/webassets/dist'),
    emptyOutDir: true,
    // The call app, the phone camera page [SL-P1-9] and the record-link page [SL-P5-3].
    rollupOptions: { input: { main: resolve(__dirname, 'index.html'), phone: resolve(__dirname, 'phone.html'), record: resolve(__dirname, 'record.html') } },
    // Emit JS assets (the AudioWorklet modules) as files instead of data:
    // URLs, so the daemon's CSP can keep script-src 'self' [SL-SEC-1].
    assetsInlineLimit: (file: string) => (file.endsWith('.js') ? false : undefined),
  },
  server: {
    port: 3000,
    proxy: {
      '/api': {target:'http://127.0.0.1:7443',ws:true},
      '/health': 'http://127.0.0.1:7443',
      // Dashboard sign-in: the daemon consumes ?launch=<token> and sets the
      // admin cookie for this dev origin (`p2pvideo dashboard --port 3000`).
      '^/\\?(.*&)?launch=': 'http://127.0.0.1:7443',
    },
  },
});
