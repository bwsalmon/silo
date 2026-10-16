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
});

describe('safeUrl', () => {
  it('keeps only links a viewer may follow', () => {
    for (const ok of ['https://example.com/a?b#c', 'http://x', 'mailto:a@b.c', '/tasks/t-1', 'relative/path', '#frag'])
      expect(safeUrl(ok)).toBe(ok);
    for (const bad of ['javascript:alert(1)', 'JavaScript:alert(1)', 'java\tscript:alert(1)', ' javascript:x', 'data:text/html,x', 'vbscript:x', 'file:///etc/passwd', 'jav\u0000ascript:x'])
      expect(safeUrl(bad)).toBe('');
  });
});
