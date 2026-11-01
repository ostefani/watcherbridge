import js from '@eslint/js';
import svelte from 'eslint-plugin-svelte';
import noUnsanitized from 'eslint-plugin-no-unsanitized';
import globals from 'globals';

// Step 22 toolchain (Designv9 §8.1): no {@html} anywhere, all Svelte
// a11y warnings fail the build (--max-warnings 0 in package.json),
// DOM injection sinks stay banned through eslint-plugin-no-unsanitized.
export default [
  js.configs.recommended,
  ...svelte.configs['flat/recommended'],
  {
    plugins: {
      'no-unsanitized': noUnsanitized
    },
    rules: {
      'no-unsanitized/method': 'error',
      'no-unsanitized/property': 'error',
      'svelte/no-at-html-tags': 'error'
    },
    languageOptions: {
      globals: { ...globals.browser }
    }
  },
  {
    // The real outDir plus a local fallback: a stale or misconfigured
    // build must never be linted as a source.
    ignores: ['../internal/viewer/web/dist/**', 'dist/**']
  },
  {
    files: ['scripts/**/*.mjs'],
    languageOptions: {
      globals: { ...globals.node }
    }
  }
];
