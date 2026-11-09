# UI conventions — folder split

How the Svelte viewer is organized, and the rules every change must hold.
Governing contracts: [Designv9 §8](../.innerdoc/Designv9.md#8-svelte-viewer-and-product-language)
(dependency allowlist, JS only, 200 KiB gzip, hash routing),
[CONTRIBUTING](../CONTRIBUTING.md) (committed `dist`, `make ui-check`).

## Roles

| | `containers/` | `components/` |
|---|---|---|
| Role | Smart: fetch, hold state, subscribe, post, wire everything together | Dumb: render props, emit callbacks |
| May import | `@/api`, `@/lib`, components, context | Other components, `@/lib/format` (pure helpers) |
| May not import | Other containers (`App` alone composes pages) | `@/api`, `@/containers`, context, `window` listeners |
| Local state | Only what orchestration needs | UI-only state (open/closed, a form draft, a copied flag) |

Enforced by `eslint.config.js` (`no-restricted-imports`); `App` has its own
override as the single composer. `src/lib` holds no markup or CSS and never
imports UI layers.

## Layout

Every unit lives in its own folder — components included — so styles,
tests and the entry point travel together:

```
components/<Name>/  Name.svelte  Name.module.css  index.js
containers/<Name>/  Name.svelte  (Name.css only when the container has its own markup)
```

Component props: data in, `on<something>` callbacks out (`onselect`,
`ondecided`). No `reload` callback props — containers own refetching.
Appearance variants ride on props (`variant`, `layout`), never on
parent selectors reaching into children. Shared derivation lives in
`@/lib/format/*` with plain-node unit coverage, not inside components.

## CSS

- CSS Modules (`*.module.css`), Vite-native. No `svelte-preprocess`:
  it is outside the Design §8.1 allowlist, and without it `<style src>`
  compiles to empty CSS (verified spike, Svelte 5.57.3 / Vite 8.3.5).
- The `.svelte` file has no `<style>` block; the CSS file next to it is
  the single owner of its classes. Scoping is enforced by hashed class
  names; hyphenated names use bracket access.
- No cross-component styling. Today's `.decision-card .trail` and
  `.dossier .briefing-heading` parent selectors become props or CSS
  custom properties during migration.
- Global CSS is only `styles/tokens.css` (custom properties, theming),
  `styles/base.css` (element defaults) and `styles/utilities.css`
  (`.sr-only`, `.mono`). Everything else gets an owner component.
- Server output must stay CSP-clean (separate CSS assets, no inline
  styles); `smoke.mjs` stubs CSS imports for SSR.

## JavaScript only

No TypeScript, no `svelte-check`, no test runner (all outside the
allowlist / forbidden automation stack). JSDoc wire typedefs with
`// wire: <GoType>` tags mirror Go views; `// @ts-check` is allowed.

## Testing

- `npm run smoke` — SSR render assertions (wire shapes, hostile text
  inertness) plus pure-unit asserts (guard, paging, undo, purge).
- `npm run characterize` — pins current behavior (20 checks) and three
  failing bug pins (BUG-1 shared reload/page guard, BUG-2 Detail id
  reset, BUG-3 Health snapshot reconnect). Pins flip green with their
  owning migrations; the harness is intentionally not in `make ui-check`
  until the pins clear (wiring is a step-38 gate decision).
- Release sign-off remains the human walkthrough in
  `.innerdoc/ui-manual-checklist.md`.

## Decisions log

- CSS Modules over `<style src>` (needs a new dep + Design exception)
  or global-prefix naming (leak-prone).
- One `@` alias for the src root (vite + jsconfig) so moves never
  rewrite specifiers; imports use `@/lib/x`, never `..` traversal.
  Node-run scripts keep relative paths; the harness resolves `@`
  through an inline `module.registerHooks()` resolver (own code, zero
  dependencies) where dynamic imports need it.
- Health migrates first, Briefing and Decisions last; one screen per
  step so the app stays shippable.
- `lib/async` is plain JS over `svelte/store`, not `.svelte.js` runes:
  runes need the compiler, which plain node lacks, and no test runner
  may be added — so unit coverage lives in `scripts/characterize.mjs`.
  Components consume the stores with `$data` as usual.
