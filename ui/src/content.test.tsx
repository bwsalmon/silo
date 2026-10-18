// Text a submitter, a node or an agent wrote is shown as text, never
// interpreted as HTML or script in the hub's origin
// (docs/threat-model.md T-TB4-1, INV-15, O-6): a script-bearing title,
// prompt, reason, status, conversation event, result, artifact kind and
// artifact bytes all reach the page as inert text.
import { describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { App } from './App';
import { safeUrl } from './components/Markdown';
import { FakeEventSource } from './test/setup';

const xss = `<script>window.pwned = 1</script><img src=x onerror="window.pwned = 2"><svg onload="window.pwned = 3"></svg>`;
const markdownXSS = `${xss}

[click](javascript:window.pwned=4) [tab](java\tscript:window.pwned=5) [data](data:text/html,<script>window.pwned=6</script>) [ok](https://example.com/x)

![remote](https://evil.example/track.png) ![js](javascript:window.pwned=7)

<a href="javascript:window.pwned=8">raw</a>`;

const task = {
  id: 't-x',
  team: 'infra',
  title: `title ${xss}`,
  prompt: markdownXSS,
  submitter: 'mallory@example.com',
  created_at: '2026-10-16T12:00:00Z',
  updated_at: '2026-10-16T12:01:00Z',
  state: 'failed',
  reason: `reason ${xss}`,
  node: `ws-1${xss}`,
  attempts: 1,
};

function stubHub() {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string) => {
      if (url === '/v1/me') return Response.json({ email: 'alice@example.com', hub_admin: false, teams: { infra: 'member' } });
      if (url === '/v1/tasks/t-x') return Response.json(task);
      if (url.startsWith('/v1/tasks/t-x/artifacts')) return new Response(`artifact ${xss}`, { status: 200 });
      return new Response('no', { status: 404 });
    }),
  );
}

function assertInert(root: HTMLElement) {
  expect((window as unknown as { pwned?: number }).pwned).toBeUndefined();
  expect(root.querySelectorAll('script, iframe, object, embed, svg[onload], img').length).toBe(0);
  for (const el of root.querySelectorAll('*')) {
    for (const a of el.getAttributeNames()) expect([el.tagName, a]).not.toEqual([el.tagName, expect.stringMatching(/^on/i)]);
  }
  for (const a of root.querySelectorAll('a[href]')) {
    const href = a.getAttribute('href') ?? '';
    expect(href).toMatch(/^(https?:|mailto:|\/)/);
  }
}

describe('node and submitter content', () => {
  it('is shown as text', async () => {
    stubHub();
    window.history.replaceState(null, '', '/tasks/t-x');
    const { container } = render(<App />);
    await screen.findByText(`title ${xss}`);
    await waitFor(() => expect(FakeEventSource.instances.length).toBe(1));
    const es = FakeEventSource.instances[0];
    const base = { v: 1, time: '2026-10-16T12:00:30Z', task: 't-x' };
    act(() => {
      es.open();
      es.emit('status', { ...base, seq: 1, type: 'status', status: { phase: 'setup', step: xss, state: 'failed', message: xss } });
      es.emit('transcript', { ...base, seq: 2, type: 'transcript', transcript: { kind: 'text', text: markdownXSS } });
      es.emit('transcript', { ...base, seq: 3, type: 'transcript', transcript: { kind: 'error', text: xss } });
      es.emit('approval_request', { ...base, seq: 4, type: 'approval_request', approval_request: { id: 'a', tool: xss, message: xss } });
      es.emit('artifact', { ...base, seq: 5, type: 'artifact', artifact: { kind: 'log', ref: `ref${xss}` } });
      es.emit('result', {
        ...base,
        seq: 6,
        type: 'result',
        result: { exit: { code: 1, reason: xss, cli_error: { kind: xss, message: xss } }, final_text: markdownXSS, errors: [xss] },
      });
      es.emit('unknown<script>', { ...base, seq: 7, type: xss });
    });
    await screen.findByText(`reason ${xss}`);
    fireEvent.click(await screen.findByRole('button', { name: 'View log' }));
    await screen.findByText(`artifact ${xss}`);

    assertInert(container);
    // The links that survive are the safe ones.
    const hrefs = [...container.querySelectorAll('a[href]')].map((a) => a.getAttribute('href'));
    expect(hrefs).toContain('https://example.com/x');
    expect(hrefs).toContain('https://evil.example/track.png');
    expect(hrefs.some((h) => /script|data:/i.test(h ?? ''))).toBe(false);
  });

  it('neutralizes script-bearing and unsafe URLs in /grainfiles and /nodes subscription views', async () => {
    const overview = {
      grainfiles: [
        {
          repo: 'https://github.com/acme/app',
          path: 'tasks/01-evil.task.md',
          commit_sha: '1111111111111111111111111111111111111111',
          ref: 'main',
          url: 'javascript:window.pwned=10',
          kind: 'task',
          title: `task ${xss}`,
          recipient: 'javascript:window.pwned=11',
          requestor: `req ${xss} javascript:window.pwned=12`,
          draft: false,
          not_before: '2026-10-19T00:00:00Z',
          references: [
            'javascript:window.pwned=13',
            'data:text/html,<script>window.pwned=14</script>',
            xss,
            'https://github.com/acme/app/blob/main/docs/safe.md',
          ],
          authorized_references: [
            'javascript:window.pwned=13',
            'https://github.com/acme/app/blob/main/docs/safe.md',
          ],
          depends_on: [
            'javascript:window.pwned=15',
            'data:text/html,<script>window.pwned=16</script>',
            'https://github.com/acme/app/blob/main/tasks/dep.task.md',
          ],
          trigger_with: [
            'javascript:window.pwned=17',
            'data:text/html,<script>window.pwned=18</script>',
            xss,
          ],
          on: {
            scenario: [
              { description: `scen ${xss}` },
              { description: 'javascript:window.pwned=19' },
              { description: 'data:text/html,<script>window.pwned=20</script>' },
            ],
          },
          updated_at: '2026-10-18T10:05:00Z',
        },
        {
          repo: 'data:text/html,<script>window.pwned=21</script>',
          path: 'processes/02-evil.process.md',
          commit_sha: '2222222222222222222222222222222222222222',
          ref: 'main',
          url: 'https://github.com/acme/app/blob/main/processes/02-evil.process.md',
          kind: 'process',
          title: `proc ${xss}`,
          recipient: 'data:text/html,<script>window.pwned=22</script>',
          requestor: 'data:text/html,<script>window.pwned=23</script>',
          draft: true,
          gate: true,
          references: ['java\tscript:window.pwned=24'],
          depends_on: ['java\tscript:window.pwned=25'],
          trigger_with: ['java\tscript:window.pwned=26'],
          on: {
            scenario: { description: `proc-scen ${xss}` },
          },
          updated_at: '2026-10-18T10:06:00Z',
        },
      ],
      subscriptions: [
        {
          node_id: `ws-${xss}`,
          owner: xss,
          recipients: ['javascript:window.pwned=11'],
          repos: ['https://github.com/acme/app'],
          folders: ['javascript:window.pwned=27'],
          grainfiles: ['javascript:window.pwned=10'],
          cursor: 1,
          revision: 1,
          updated_at: '2026-10-18T10:05:00Z',
        },
      ],
      events: [
        {
          seq: 1,
          type: 'process_triggered',
          repo: 'https://github.com/acme/app',
          path: 'processes/02-evil.process.md',
          grainfile_url: 'https://github.com/acme/app/blob/main/processes/02-evil.process.md',
          kind: 'process',
          title: `proc ${xss}`,
          recipient: 'javascript:window.pwned=22',
          trigger_source: xss,
          triggered_by: 'javascript:window.pwned=28',
          dependency_url: 'data:text/html,<script>window.pwned=29</script>',
          reason: `event-reason ${xss}`,
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
                id: `ws-node-${xss}`,
                repos: [`acme/app-${xss}`],
                slots: 2,
                owner: 'alice@example.com',
                status: 'online',
                free: 2,
                claimed: 0,
                connected_at: '2026-10-18T10:00:00Z',
                last_seen: '2026-10-18T10:05:00Z',
                features: ['grainfiles', 'nodeui'],
                recipients: [
                  'javascript:window.pwned=30',
                  'data:text/html,<script>window.pwned=31</script>',
                  'https://github.com/acme/grain-safe',
                ],
                folders: ['javascript:window.pwned=32', xss],
                grainfiles: [
                  'javascript:window.pwned=33',
                  'data:text/html,<script>window.pwned=34</script>',
                  'https://github.com/acme/app/blob/main/tasks/ok.task.md',
                ],
                ui: { offered: true, url: 'javascript:window.pwned=35' },
                ui_url: 'javascript:window.pwned=35',
              },
            ],
          });
        }
        if (url === '/v1/node-tokens') return Response.json({ node_tokens: [] });
        return new Response('no', { status: 404 });
      }),
    );

    window.history.replaceState(null, '', '/grainfiles');
    const { container, unmount } = render(<App />);
    await screen.findAllByText(`task ${xss}`);
    await screen.findAllByText(`scenario: scen ${xss}`);

    assertInert(container);
    const taskHrefs = [...container.querySelectorAll('a[href]')].map((a) => a.getAttribute('href'));
    expect(taskHrefs).toContain('https://github.com/acme/app/blob/main/docs/safe.md');
    expect(taskHrefs).toContain('https://github.com/acme/app/blob/main/tasks/dep.task.md');
    expect(taskHrefs.some((h) => /javascript:|data:/i.test(h ?? ''))).toBe(false);

    // Inspect the second row (process) and verify its metadata and trigger events are inert.
    fireEvent.click(screen.getByText(`proc ${xss}`));
    await screen.findByText(`event-reason ${xss}`);
    await screen.findAllByText(`scenario: proc-scen ${xss}`);
    assertInert(container);
    const procHrefs = [...container.querySelectorAll('a[href]')].map((a) => a.getAttribute('href'));
    expect(procHrefs.some((h) => /javascript:|data:/i.test(h ?? ''))).toBe(false);
    unmount();

    // Verify /nodes subscription view neutralizes unsafe links and scripts as well.
    window.history.replaceState(null, '', '/nodes');
    const { container: nodesContainer } = render(<App />);
    await screen.findByText(`ws-node-${xss}`);
    assertInert(nodesContainer);
    const nodeHrefs = [...nodesContainer.querySelectorAll('a[href]')].map((a) => a.getAttribute('href'));
    expect(nodeHrefs).toContain('https://github.com/acme/grain-safe');
    expect(nodeHrefs).toContain('https://github.com/acme/app/blob/main/tasks/ok.task.md');
    expect(nodeHrefs.some((h) => /javascript:|data:/i.test(h ?? ''))).toBe(false);
    expect(screen.queryByRole('link', { name: 'Open Grain UI' })).toBeNull();
  });
});

describe('safeUrl', () => {
  it('keeps only links a viewer may follow', () => {
    for (const ok of ['https://example.com/a?b#c', 'http://x', 'mailto:a@b.c', '/tasks/t-1', 'relative/path', '#frag'])
      expect(safeUrl(ok)).toBe(ok);
    for (const bad of ['javascript:alert(1)', 'JavaScript:alert(1)', 'java\tscript:alert(1)', ' javascript:x', 'data:text/html,x', 'vbscript:x', 'file:///etc/passwd', 'jav\u0000ascript:x'])
      expect(safeUrl(bad)).toBe('');
  });
});
