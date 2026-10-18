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

describe('the grainfiles and nodes pages’ storage boundary', () => {
  it('writes nothing to storage or service workers when browsing /grainfiles, inspecting details, and viewing /nodes', async () => {
    const overview = {
      grainfiles: [
        {
          repo: 'https://github.com/acme/app',
          path: 'tasks/01-deploy.task.md',
          commit_sha: '1111111111111111111111111111111111111111',
          ref: 'main',
          url: 'https://github.com/acme/app/blob/main/tasks/01-deploy.task.md',
          kind: 'task',
          title: 'Deploy service to staging',
          recipient: 'https://github.com/acme/grain-eu',
          requestor: 'Alice <alice@example.com>',
          draft: false,
          not_before: '2026-10-19T00:00:00Z',
          references: ['https://github.com/acme/app/blob/main/docs/arch.md', 'https://github.com/other/repo/blob/main/ext.md'],
          authorized_references: ['https://github.com/acme/app/blob/main/docs/arch.md'],
          depends_on: ['https://github.com/acme/app/blob/main/tasks/00-prep.task.md'],
          updated_at: '2026-10-18T10:05:00Z',
        },
        {
          repo: 'https://github.com/acme/infra',
          path: '.grain/processes/ci.process.md',
          commit_sha: '2222222222222222222222222222222222222222',
          ref: 'main',
          url: 'https://github.com/acme/infra/blob/main/.grain/processes/ci.process.md',
          kind: 'process',
          title: 'Continuous verification gate',
          recipient: 'https://github.com/acme/grain-us',
          draft: true,
          gate: true,
          trigger_with: ['https://github.com/acme/infra/blob/main/.grain/processes/smoke.process.md'],
          on: {
            push: { branches: ['main'] },
            schedule: { cron: '0 * * * *', timezone: 'UTC' },
            scenario: { description: 'Verify staging' },
          },
          updated_at: '2026-10-18T10:06:00Z',
        },
      ],
      subscriptions: [
        {
          node_id: 'ws-eu-1',
          owner: 'alice@example.com',
          recipients: ['https://github.com/acme/grain-eu'],
          repos: ['https://github.com/acme/app'],
          folders: ['https://github.com/acme/app/tree/main/tasks'],
          grainfiles: ['https://github.com/acme/app/blob/main/tasks/01-deploy.task.md'],
          cursor: 3,
          revision: 1,
          updated_at: '2026-10-18T10:05:00Z',
        },
      ],
      events: [
        {
          seq: 1,
          type: 'task_ready',
          repo: 'https://github.com/acme/app',
          path: 'tasks/01-deploy.task.md',
          grainfile_url: 'https://github.com/acme/app/blob/main/tasks/01-deploy.task.md',
          kind: 'task',
          title: 'Deploy service to staging',
          recipient: 'https://github.com/acme/grain-eu',
          created_at: '2026-10-18T10:05:00Z',
        },
        {
          seq: 2,
          type: 'process_triggered',
          repo: 'https://github.com/acme/infra',
          path: '.grain/processes/ci.process.md',
          grainfile_url: 'https://github.com/acme/infra/blob/main/.grain/processes/ci.process.md',
          kind: 'process',
          title: 'Continuous verification gate',
          recipient: 'https://github.com/acme/grain-us',
          trigger_source: 'push',
          gate: true,
          reason: 'commit 2222222 on main',
          created_at: '2026-10-18T10:06:00Z',
        },
      ],
    };

    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string) => {
        if (url === '/v1/me') return Response.json({ email: 'alice@example.com', hub_admin: false, teams: { infra: 'admin' } });
        if (url.startsWith('/v1/grainfiles')) return Response.json(overview);
        if (url === '/v1/nodes') {
          return Response.json({
            nodes: [
              {
                id: 'ws-eu-1',
                repos: ['acme/app'],
                slots: 4,
                owner: 'alice@example.com',
                status: 'online',
                free: 3,
                claimed: 0,
                connected_at: '2026-10-18T10:00:00Z',
                last_seen: '2026-10-18T10:05:00Z',
                features: ['grainfiles', 'nodeui'],
                recipients: ['https://github.com/acme/grain-eu'],
                folders: ['https://github.com/acme/app/tree/main/tasks'],
                grainfiles: ['https://github.com/acme/app/blob/main/tasks/01-deploy.task.md'],
                ui: { offered: true, url: 'https://ws-eu-1.nodes.hub.example.com/' },
                ui_url: 'https://ws-eu-1.nodes.hub.example.com/',
              },
            ],
          });
        }
        if (url === '/v1/node-tokens') return Response.json({ node_tokens: [] });
        return new Response('no', { status: 404 });
      }),
    );

    const w = watchStorage();
    window.history.replaceState(null, '', '/grainfiles');
    render(<App />);

    await screen.findAllByText('Deploy service to staging');
    expect(screen.getByText('task_ready')).toBeTruthy();

    // Inspect the second Grainfile and filter the list.
    fireEvent.click(screen.getByText('Continuous verification gate'));
    await screen.findByText('process_triggered');
    fireEvent.change(screen.getByLabelText('Repository'), { target: { value: 'acme/app' } });

    // Navigate to /nodes and inspect connected deployment subscriptions.
    fireEvent.click(screen.getByRole('button', { name: 'Nodes' }));
    await screen.findByText('ws-eu-1');
    expect(screen.getByText('https://github.com/acme/grain-eu')).toBeTruthy();

    expect(w.register).not.toHaveBeenCalled();
    expect(w.idbOpen).not.toHaveBeenCalled();
    expect(w.cachesOpen).not.toHaveBeenCalled();
    expect(w.setItem).not.toHaveBeenCalled();
    expect(localStorage.length).toBe(0);
    expect(sessionStorage.length).toBe(0);
    expect(document.cookie).toBe('');
  });

  it('refuses javascript:, data:, vbscript:, and protocol-relative // hrefs on /grainfiles and /nodes', async () => {
    const hostileOverview = {
      grainfiles: [
        {
          repo: 'https://github.com/acme/app',
          path: 'tasks/evil.task.md',
          commit_sha: '1111111111111111111111111111111111111111',
          ref: 'main',
          url: 'javascript:alert(1)',
          kind: 'task',
          title: 'Hostile Grainfile Links',
          recipient: '//evil.example.com/phish',
          requestor: 'data:text/html,<script>alert(1)</script>',
          draft: false,
          references: ['javascript:alert(2)', '//evil.example.com/ref', 'https://github.com/acme/app/blob/main/docs/safe.md'],
          authorized_references: ['vbscript:msgbox(1)'],
          depends_on: ['/\\evil.example.com/dep'],
          trigger_with: ['java\tscript:alert(3)'],
          updated_at: '2026-10-18T10:05:00Z',
        },
      ],
      subscriptions: [],
      events: [],
    };

    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string) => {
        if (url === '/v1/me') return Response.json({ email: 'alice@example.com', hub_admin: false, teams: { infra: 'admin' } });
        if (url.startsWith('/v1/grainfiles')) return Response.json(hostileOverview);
        if (url === '/v1/nodes') {
          return Response.json({
            nodes: [
              {
                id: 'ws-evil',
                repos: ['acme/app'],
                slots: 2,
                owner: 'alice@example.com',
                status: 'online',
                free: 2,
                claimed: 0,
                connected_at: '2026-10-18T10:00:00Z',
                last_seen: '2026-10-18T10:05:00Z',
                features: ['grainfiles', 'nodeui'],
                recipients: ['javascript:alert(4)', '//evil.example.com/rec'],
                folders: ['data:text/html,evil'],
                grainfiles: ['/\\evil.example.com/gf'],
                ui: { offered: true, url: '//evil.example.com/ui' },
                ui_url: '//evil.example.com/ui',
              },
            ],
          });
        }
        if (url === '/v1/node-tokens') return Response.json({ node_tokens: [] });
        return new Response('no', { status: 404 });
      }),
    );

    window.history.replaceState(null, '', '/grainfiles');
    const { container } = render(<App />);
    await screen.findAllByText('Hostile Grainfile Links');

    const assertNoUnsafeAnchors = () => {
      const anchors = Array.from(container.querySelectorAll('a'));
      for (const a of anchors) {
        const href = a.getAttribute('href') ?? '';
        expect(href).not.toMatch(/^(javascript:|data:|vbscript:|\/\/|\/\\)/i);
        if (href.startsWith('http://') || href.startsWith('https://')) {
          expect(a.getAttribute('rel')).toContain('noopener');
          expect(a.getAttribute('rel')).toContain('noreferrer');
        }
      }
    };
    assertNoUnsafeAnchors();

    fireEvent.click(screen.getByRole('button', { name: 'Nodes' }));
    await screen.findByText('ws-evil');
    assertNoUnsafeAnchors();
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
