// The SDK keeps nothing: it names no browser storage API and registers
// no service worker, so an artifact it reads through the hub (docs/hub.md
// §7.2) or a token it is handed stays in its caller's memory. The hub
// UI's trust test (ui/src/trust.test.tsx) checks the same of the UI,
// which builds this source in.
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

const src = fileURLToPath(new URL('../src', import.meta.url));

describe('the SDK’s source', () => {
  it('has no storage', () => {
    const files = readdirSync(src).filter((f) => f.endsWith('.ts'));
    expect(files).toContain('client.ts');
    for (const f of files) {
      const text = readFileSync(join(src, f), 'utf8');
      expect([f, /\b(localStorage|sessionStorage|indexedDB|serviceWorker|caches\.)/.test(text)]).toEqual([f, false]);
    }
  });
});
