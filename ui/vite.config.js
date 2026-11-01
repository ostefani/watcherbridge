import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';

// Step 22 toolchain (Designv9 §8.1): JavaScript only, CSP-safe output.
// Separate JS/CSS assets, no sourcemaps, no inlined data URIs, no
// inline scripts — everything the Go server embeds must load under
// the viewer's Content-Security-Policy. Development proxies the API
// and the event stream to the loopback bridge.
export default defineConfig({
  plugins: [svelte({ compilerOptions: { runes: true } })],
  build: {
    // The bundle is committed at internal/viewer/web/dist and embedded
    // by the Go server, so asset names stay fixed: deterministic output
    // keeps the drift check meaningful, and no-store caching needs no
    // content hashes.
    outDir: '../internal/viewer/web/dist',
    emptyOutDir: true,
    sourcemap: false,
    assetsInlineLimit: 0,
    cssCodeSplit: true,
    rollupOptions: {
      output: {
        entryFileNames: 'assets/[name].js',
        chunkFileNames: 'assets/[name].js',
        assetFileNames: 'assets/[name].[ext]'
      }
    }
  },
  server: {
    port: 5173,
    strictPort: true,
    proxy: {
      '/api': 'http://127.0.0.1:8081',
      '/events': 'http://127.0.0.1:8081'
    }
  }
});
