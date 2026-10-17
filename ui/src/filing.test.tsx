// A task a Grain filed at the hub (docs/hub.md §6.6) says, on its page,
// which Grain filed it on whose behalf and from which task, and links to
// the tasks it depends on.
import { describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { App } from './App';

const filed = {
  id: 't-3',
  team: 'web',
  repo: 'acme/web',
  title: 'Bump the client',
  prompt: 'Bump it.',
  submitter: 'carol@example.com',
  filed_by: 'ws-1',
  filed_by_owner: 'alice@example.com',
  origin_task: 't-1',
  depends_on: ['t-1', 't-2'],
  created_at: '2026-10-16T12:00:00Z',
  updated_at: '2026-10-16T12:00:00Z',
  state: 'proposed',
  attempts: 0,
};

function stubHub(routes: Record<string, unknown>) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init?: RequestInit) => {
      const r = routes[`${init?.method ?? 'GET'} ${url}`];
      return r === undefined ? new Response('no such thing\n', { status: 404 }) : Response.json(r);
    }),
  );
}

function open(path: string) {
  window.history.replaceState(null, '', path);
  render(<App />);
}

describe('a task a Grain filed', () => {
  it('says who filed it, for whom, and from where, and links its dependencies', async () => {
    stubHub({ 'GET /v1/me': { email: 'bob@example.com', hub_admin: false, teams: { web: 'member' } }, 'GET /v1/tasks/t-3': filed });
    open('/tasks/t-3');
    expect(await screen.findByText('Filed by')).toBeTruthy();
    expect(screen.getByText(/Grain ws-1 \(alice@example\.com\) on behalf of carol@example\.com/)).toBeTruthy();
    expect(screen.getByText('Depends on')).toBeTruthy();
    const links = screen.getAllByRole('link').map((a) => a.getAttribute('href'));
    expect(links.filter((h) => h === '/tasks/t-1')).toHaveLength(2);
    expect(links).toContain('/tasks/t-2');
  });

  it('says nothing of a filer for a task a human filed', async () => {
    const { filed_by: _f, filed_by_owner: _o, origin_task: _t, depends_on: _d, ...human } = filed;
    stubHub({ 'GET /v1/me': { email: 'bob@example.com', hub_admin: false, teams: { web: 'member' } }, 'GET /v1/tasks/t-3': human });
    open('/tasks/t-3');
    expect(await screen.findByText('Bump the client')).toBeTruthy();
    expect(screen.queryByText('Filed by')).toBeNull();
    expect(screen.queryByText('Depends on')).toBeNull();
  });
});
