import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';
import { fileURLToPath } from 'node:url';

// Phase 0 folder split: stable import roots so components, containers,
// lib modules and the api folder land without rewriting specifiers.
// The target folders arrive in Phases 2-5; no source imports them yet.
const src = fileURLToPath(new URL('./src/', import.meta.url));

// Step 22 toolchain (Designv9 §8.1): JavaScript only, CSP-safe output.
// Separate JS/CSS assets, no sourcemaps, no inlined data URIs, no
// inline scripts — everything the Go server embeds must load under
// the viewer's Content-Security-Policy. Development proxies the API
// and the event stream to the loopback bridge.
//
// Accessibility is a build failure, not a warning: Svelte compiler
// a11y_* warnings never reach ESLint, so --max-warnings 0 cannot see
// them. The onwarn hook below rejects them at compile time instead.
export default defineConfig({
  plugins: [
    svelte({
      compilerOptions: { runes: true },
      onwarn(warning, handler) {
        // Svelte warning codes use underscores (a11y_missing_attribute).
        if (warning.code && warning.code.startsWith('a11y_')) {
          throw new Error(`svelte a11y violation treated as error: ${warning.code} ${warning.message}`);
        }
        handler(warning);
      }
    })
  ],
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
  resolve: {
    alias: {
      // Single src root: imports use `@/lib/x`, never `..` traversal.
      // `@/api.js` is the file today; it becomes `@/api/…` when the
      // Phase 2 folder split lands, with no alias change.
      '@': src
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
