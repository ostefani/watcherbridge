// Plan step 38: no inline `<style>` blocks in Svelte sources. Scoped
// component styles live in sibling CSS Modules; page and shell
// composition lives in styles/*.css. Fails the file:line of the first
// offender so CI points at the violation.
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const src = join(fileURLToPath(new URL('..', import.meta.url)), 'src');

function collect(dir) {
  const found = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = join(dir, entry.name);
    if (entry.isDirectory()) {
      found.push(...collect(full));
    } else if (entry.name.endsWith('.svelte')) {
      found.push(full);
    }
  }
  return found;
}

let offenders = 0;
for (const full of collect(src)) {
  const lines = readFileSync(full, 'utf8').split('\n');
  lines.forEach((line, i) => {
    if (/<style[\s>]/.test(line)) {
      console.error(`${full}:${i + 1}: inline <style> block; use the sibling CSS module or styles/*.css`);
      offenders += 1;
    }
  });
}
if (offenders > 0) {
  process.exit(1);
}
console.log('styleguard: no inline <style> blocks');
