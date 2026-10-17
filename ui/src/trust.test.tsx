// The trust boundary (docs/ui.md §2): the UI views a node-only
// artifact in memory and keeps no copy of it. It registers no service
// worker, writes nothing to the browser's storage, asks for the
// artifact uncached, and shows nothing of it when the node is offline.
import { readdirSync, readFileSync, existsSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { App } from './App';
import { FakeEventSource } from './test/setup';

const secret = 'SECRET-CODE-LINE';
const diff = `diff --git a/hello.txt b/hello.txt
index 1111111..2222222 100644
--- a/hello.txt
+++ b/hello.txt
@@ -1 +1 @@
-hello
+${secret}
`;

const task = {
  id: 't-1',
  team: 'infra',
  repo: 'bwsalmon/silo',
  title: 'Say something',
  prompt: 'Change **hello**.',
  submitter: 'alice@example.com',
  created_at: '2026-10-16T12:00:00Z',
  updated_at: '2026-10-16T12:01:00Z',
  state: 'running',
  node: 'ws-1',
  attempts: 1,
};

// stubHub answers the API the way a hub would, with the artifact
// answering artifactStatus.
function stubHub(artifactStatus: number) {
  const f = vi.fn(async (url: string, _init?: RequestInit) => {
    if (url === '/v1/me') return Response.json({ email: 'alice@example.com', hub_admin: false, teams: { infra: 'member' } });
    if (url === '/v1/tasks/t-1') return Response.json(task);
    if (url.startsWith('/v1/tasks/t-1/artifacts?ref=diff-1')) {
      return artifactStatus === 200
        ? new Response(diff, { status: 200, headers: { 'Cache-Control': 'no-store' } })
        : new Response('unavailable: node ws-1, which holds it, is offline\n', { status: artifactStatus });
    }
    return new Response('no such thing', { status: 404 });
  });
  vi.stubGlobal('fetch', f);
  return f;
}

// watchStorage spies on every way a page could keep something in the
// browser past this view.
function watchStorage() {
  const register = vi.fn();
  Object.defineProperty(navigator, 'serviceWorker', { value: { register }, configurable: true });
  const idbOpen = vi.fn();
  vi.stubGlobal('indexedDB', { open: idbOpen });
  const cachesOpen = vi.fn();
  vi.stubGlobal('caches', { open: cachesOpen });
  const setItem = vi.spyOn(Storage.prototype, 'setItem');
  return { register, idbOpen, cachesOpen, setItem };
}

async function openTask() {
  window.history.replaceState(null, '', '/tasks/t-1');
  render(<App />);
  await screen.findByText('Say something');
  await waitFor(() => expect(FakeEventSource.instances.length).toBe(1));
  const es = FakeEventSource.instances[0];
  expect(es.url).toBe('/v1/tasks/t-1/events');
  act(() => {
    es.open();
    es.emit('artifact', { v: 1, seq: 4, time: '2026-10-16T12:00:30Z', type: 'artifact', task: 't-1', artifact: { kind: 'diff', ref: 'diff-1', size: 120 } });
  });
  fireEvent.click(await screen.findByRole('button', { name: 'View diff' }));
}

describe('the task page’s view-through', () => {
  it('shows an artifact from memory and keeps no copy', async () => {
    const fetch = stubHub(200);
    const w = watchStorage();
    await openTask();
    await screen.findByText(secret, { exact: false });
    const call = fetch.mock.calls.find(([url]) => url.includes('/artifacts'));
    expect(call?.[1]?.cache).toBe('no-store');

    fireEvent.click(screen.getByRole('button', { name: 'Hide diff' }));
    expect(screen.queryByText(secret, { exact: false })).toBeNull();

    expect(w.register).not.toHaveBeenCalled();
    expect(w.idbOpen).not.toHaveBeenCalled();
    expect(w.cachesOpen).not.toHaveBeenCalled();
    expect(w.setItem).not.toHaveBeenCalled();
    expect(localStorage.length).toBe(0);
    expect(sessionStorage.length).toBe(0);
    expect(document.cookie).toBe('');
  });

  it('says an offline node’s artifact is unavailable and shows no copy', async () => {
    stubHub(503);
    const w = watchStorage();
    await openTask();
    await screen.findByText(/Unavailable: the node that holds this diff is offline/);
    expect(screen.queryByText(secret, { exact: false })).toBeNull();
    expect(w.setItem).not.toHaveBeenCalled();
  });
});

// The UI's own code, and the client SDK it builds in from source
// (sdk/ts/src, through vite.config.ts's alias), name no storage API at
// all, and the built bundle
// (when `npm run build` has made one) registers no service worker.
describe('the bundle', () => {
  const storage = /\b(localStorage|sessionStorage|indexedDB|serviceWorker|caches\.)/;

  it('has no storage in its source', () => {
    const walk = (dir: string): string[] =>
      readdirSync(dir, { withFileTypes: true }).flatMap((d) =>
        d.isDirectory() ? walk(join(dir, d.name)) : /\.tsx?$/.test(d.name) && !/\.test\.tsx?$/.test(d.name) && dir !== join(__dirname, 'test') ? [join(dir, d.name)] : [],
      );
    const files = walk(__dirname);
    expect(files.length).toBeGreaterThan(5);
    const sdk = walk(join(__dirname, '../../sdk/ts/src'));
    expect(sdk.map((f) => f.split('/').pop())).toContain('client.ts');
    files.push(...sdk);
    for (const f of files) expect([f, storage.test(readFileSync(f, 'utf8'))]).toEqual([f, false]);
  });

  it('registers no service worker', (ctx) => {
    const assets = join(__dirname, '../../hub/ui/static/app/assets');
    if (!existsSync(assets)) {
      if (process.env.SILO_UI_REQUIRE_BUNDLE) throw new Error(`no bundle at ${assets}`);
      ctx.skip();
    }
    const js = readdirSync(assets).filter((f) => f.endsWith('.js'));
    expect(js.length).toBeGreaterThan(0);
    for (const f of js) {
      const src = readFileSync(join(assets, f), 'utf8');
      expect([f, /serviceWorker/.test(src)]).toEqual([f, false]);
    }
  });
});
