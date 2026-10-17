// The UI reaches the hub's API only through the client SDK (sdk/ts,
// imported as @silo/hub-sdk; docs/ui.md §11): no file of the app makes
// a request of its own, names an API path, or opens a stream. A test
// may stub fetch; the app may not call it.
import { readdirSync, readFileSync } from 'node:fs';
import { join, relative } from 'node:path';
import { describe, expect, it } from 'vitest';

const src = __dirname;

function appFiles(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((d) => {
    const p = join(dir, d.name);
    if (d.isDirectory()) return d.name === 'test' ? [] : appFiles(p);
    return /\.tsx?$/.test(d.name) && !/\.test\.tsx?$/.test(d.name) ? [p] : [];
  });
}

// code is a file without its comments, which may name what the code
// may not.
const code = (text: string) => text.replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:])\/\/.*$/gm, '$1');

const forbidden: [string, RegExp][] = [
  ['calls fetch', /\bfetch\s*\(/],
  ['names an API path', /['"`]\/v1\//],
  ['opens an EventSource', /\bEventSource\b/],
  ['opens a WebSocket', /\bWebSocket\b/],
  ['uses XMLHttpRequest', /\bXMLHttpRequest\b/],
  ['sends a beacon', /\bsendBeacon\b/],
];

describe('the UI’s API access', () => {
  const files = appFiles(src);

  it('finds the app’s files', () => {
    expect(files.map((f) => relative(src, f))).toContain('api.ts');
  });

  it.each(forbidden)('never %s outside the SDK', (_what, re) => {
    const offenders = files.filter((f) => re.test(code(readFileSync(f, 'utf8')))).map((f) => relative(src, f));
    expect(offenders).toEqual([]);
  });

  it('catches a direct call', () => {
    expect(forbidden.some(([, re]) => re.test(code("const r = await fetch('/v1/tasks');")))).toBe(true);
    expect(forbidden.some(([, re]) => re.test(code('// fetch(\'/v1/tasks\') is the SDK’s')))).toBe(false);
  });
});
