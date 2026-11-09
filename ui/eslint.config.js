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
      'svelte/no-at-html-tags': 'error',
      // The recommended set leaves this off; without it a template
      // that does not compile is a lint pass and only fails the build.
      'svelte/valid-compile': 'error'
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
  },
  {
    // Folder-split boundaries (CONVENTIONS): components render props and
    // never fetch; containers orchestrate and never import each other
    // (App alone composes pages); lib stays free of UI layers.
    // These patterns match nothing until Phases 2-5 create the folders,
    // so they lock the contract before the first move.
    files: ['src/components/**/*.{js,svelte}'],
    rules: {
      'no-restricted-imports': ['error', {
        patterns: [
          { group: ['@/api*'], message: 'components render props; fetching lives in containers.' },
          { group: ['@/containers', '@/containers/*'], message: 'components never import containers.' },
          { group: ['@/lib/events/context*', '**/events/context*'], message: 'components read connection state from props, not context.' }
        ]
      }]
    }
  },
  {
    files: ['src/containers/**/*.{js,svelte}'],
    rules: {
      'no-restricted-imports': ['error', {
        patterns: [
          { group: ['@/containers', '@/containers/*'], message: 'containers never import each other; App alone composes pages.' }
        ]
      }]
    }
  },
  {
    // App is the single composer: it may import page containers.
    files: ['src/containers/App/**/*.{js,svelte}'],
    rules: {
      'no-restricted-imports': 'off'
    }
  },
  {
    files: ['src/lib/**/*.{js,svelte.js}'],
    rules: {
      'no-restricted-imports': ['error', {
        patterns: [
          { group: ['@/components', '@/components/*', '@/containers', '@/containers/*'], message: 'lib stays UI-free; containers compose lib, never the reverse.' }
        ]
      }]
    }
  }
];
