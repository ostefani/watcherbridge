// Step 22 size gate (Designv9 §8.1): the committed bundle must stay
// at or under 200 KiB gzipped. Measures every emitted .js/.css file.
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { gzipSync } from 'node:zlib';

const LIMIT = 200 * 1024;

function walk(dir, out = []) {
  for (const name of readdirSync(dir)) {
    const path = join(dir, name);
    if (statSync(path).isDirectory()) {
      walk(path, out);
    } else if (name.endsWith('.js') || name.endsWith('.css')) {
      out.push(path);
    }
  }
  return out;
}

const dist = fileURLToPath(new URL('../../internal/viewer/web/dist/', import.meta.url));
let total = 0;
for (const file of walk(dist)) {
  total += gzipSync(readFileSync(file)).length;
}
console.log(`gzipped js+css: ${total} bytes (limit ${LIMIT})`);
if (total > LIMIT) {
  console.error('UI bundle exceeds the 200 KiB gzip budget');
  process.exit(1);
}
