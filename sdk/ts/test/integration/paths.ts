import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));

// repoRoot is the checkout; binDir is where setup.ts builds `silo` and
// the fake Grain (ignored by git).
export const repoRoot = resolve(here, '../../../..');
export const binDir = resolve(here, '../../.bin');
