// U3 (docs/ui.md §6): proposals and in-run approvals. Each button sends
// the public API's request with the browser's own credentials, is shown
// only to a member of the task's team (and, for a proposal, not to its
// submitter) as /v1/me says, and shows the API's refusal as the API
// says it.
import { describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { App } from './App';
import * as api from './api';
import { FakeEventSource } from './test/setup';

type Handler = (body: unknown) => Response;

interface Call {
  method: string;
  url: string;
  init?: RequestInit;
  body?: unknown;
}

function stubHub(routes: Record<string, Handler | Response>) {
  const calls: Call[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init?: RequestInit) => {
      const method = init?.method ?? 'GET';
      const body = typeof init?.body === 'string' ? JSON.parse(init.body) : undefined;
      calls.push({ method, url, init, body });
      const h = routes[`${method} ${url}`];
      if (!h) return new Response('no such thing\n', { status: 404 });
      return typeof h === 'function' ? h(body) : h.clone();
    }),
  );
  return calls;
}

const writes = (calls: Call[]) => calls.filter((c) => c.method !== 'GET');

const me = (email: string, teams: Record<string, string>, hubAdmin = false) => Response.json({ email, hub_admin: hubAdmin, teams });

// carol, outside infra, proposed it in infra's repository.
const proposal = {
  id: 't-1',
  team: 'infra',
  repo: 'acme/app',
  title: 'An idea',
  prompt: 'Do the idea.',
  submitter: 'carol@example.com',
  created_at: '2026-10-16T12:00:00Z',
  updated_at: '2026-10-16T12:00:00Z',
  state: 'proposed',
  attempts: 0,
};

const running = {
  ...proposal,
  id: 't-2',
  state: 'running',
  node: 'ws-1',
  approved_by: 'dave@example.com',
  approved_at: '2026-10-16T12:05:00Z',
  attempts: 1,
};

function open(path: string) {
  window.history.replaceState(null, '', path);
  render(<App />);
}

describe('the API layer’s approval writes', () => {
  const cases: [string, () => Promise<unknown>, string, string, unknown][] = [
    ['approveTask', () => api.approveTask('t 1'), 'POST', '/v1/tasks/t%201/approve', undefined],
    ['rejectTask', () => api.rejectTask('t-1', 'not now'), 'POST', '/v1/tasks/t-1/reject', { reason: 'not now' }],
    ['rejectTask, no reason', () => api.rejectTask('t-1'), 'POST', '/v1/tasks/t-1/reject', undefined],
    ['answerApproval', () => api.answerApproval('t-1', 'r/1', 'allow'), 'POST', '/v1/tasks/t-1/approvals/r%2F1', { decision: 'allow' }],
    ['answerApproval, a reason', () => api.answerApproval('t-1', 'r-1', 'deny', 'too broad'), 'POST', '/v1/tasks/t-1/approvals/r-1', { decision: 'deny', reason: 'too broad' }],
  ];
  it.each(cases)('%s', async (_, call, method, url, body) => {
    const calls = stubHub({ [`${method} ${url}`]: Response.json({}) });
    await call();
    expect(calls).toHaveLength(1);
    const c = calls[0];
    expect(c.method).toBe(method);
    expect(c.url).toBe(url);
    expect(c.body).toEqual(body);
    expect(c.init?.credentials).toBe('same-origin');
    expect(JSON.stringify(c.init?.headers ?? {})).not.toMatch(/authorization/i);
  });

  it('lists a task’s answers', async () => {
    stubHub({ 'GET /v1/tasks/t-1/approvals': Response.json({ approvals: [{ request_id: 'r-1', decision: 'allow', answered_by: 'dave@example.com' }] }) });
    expect((await api.listApprovals('t-1'))[0].answered_by).toBe('dave@example.com');
  });
});

describe('a team’s proposals', () => {
  const routes = (who: Response) => ({
    'GET /v1/me': who,
    'GET /v1/tasks?team=infra&state=proposed': Response.json({ tasks: [proposal] }),
  });

  it('lets a member approve and reject', async () => {
    const calls = stubHub({
      ...routes(me('dave@example.com', { infra: 'member' })),
      'POST /v1/tasks/t-1/approve': Response.json({ ...proposal, state: 'queued', approved_by: 'dave@example.com' }),
      'POST /v1/tasks/t-1/reject': Response.json({ ...proposal, state: 'rejected' }),
    });
    open('/teams/infra/proposals');
    expect(await screen.findByText('An idea')).toBeTruthy();
    expect(screen.getByText('carol@example.com')).toBeTruthy();
    fireEvent.click(await screen.findByRole('button', { name: 'Approve' }));
    await waitFor(() => expect(writes(calls)).toHaveLength(1));
    expect(writes(calls)[0]).toMatchObject({ method: 'POST', url: '/v1/tasks/t-1/approve' });

    fireEvent.change(screen.getByLabelText('Reason to reject An idea'), { target: { value: 'not now' } });
    fireEvent.click(screen.getByRole('button', { name: 'Reject' }));
    fireEvent.click(screen.getByRole('button', { name: 'Confirm reject' }));
    await waitFor(() => expect(writes(calls)).toHaveLength(2));
    expect(writes(calls)[1]).toMatchObject({ method: 'POST', url: '/v1/tasks/t-1/reject', body: { reason: 'not now' } });
  });

  it('shows the API’s refusal of a second approval', async () => {
    stubHub({
      ...routes(me('dave@example.com', { infra: 'admin' })),
      'POST /v1/tasks/t-1/approve': new Response('tasks: not allowed in the task\'s state: task t-1 is queued\n', { status: 409 }),
    });
    open('/teams/infra/proposals');
    fireEvent.click(await screen.findByRole('button', { name: 'Approve' }));
    expect(await screen.findByText(/Conflict \(409\): tasks: not allowed in the task's state/)).toBeTruthy();
  });

  it.each([
    ['its submitter', me('carol@example.com', { web: 'member' })],
    ['a hub admin outside the team', me('root@example.com', {}, true)],
  ])('offers no approval to %s', async (_, who) => {
    stubHub(routes(who));
    open('/teams/infra/proposals');
    expect(await screen.findByText('An idea')).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Approve' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Reject' })).toBeNull();
  });
});

describe('the task page', () => {
  it('lets a member approve a proposal, and opens no event stream for it', async () => {
    const calls = stubHub({
      'GET /v1/me': me('dave@example.com', { infra: 'member' }),
      'GET /v1/tasks/t-1': Response.json(proposal),
      'POST /v1/tasks/t-1/approve': Response.json({ ...proposal, state: 'queued' }),
    });
    open('/tasks/t-1');
    fireEvent.click(await screen.findByRole('button', { name: 'Approve' }));
    await waitFor(() => expect(writes(calls)).toHaveLength(1));
    expect(writes(calls)[0].url).toBe('/v1/tasks/t-1/approve');
    expect(FakeEventSource.instances).toHaveLength(0);
  });

  it('shows who approved the task', async () => {
    stubHub({ 'GET /v1/me': me('carol@example.com', {}), 'GET /v1/tasks/t-2': Response.json(running) });
    open('/tasks/t-2');
    expect(await screen.findByText('Approved by')).toBeTruthy();
    expect(screen.getByText(/dave@example\.com/)).toBeTruthy();
  });

  async function request(id = 't-2') {
    await waitFor(() => expect(FakeEventSource.instances.length).toBe(1));
    const es = FakeEventSource.instances[0];
    act(() => {
      es.open();
      es.emit('approval_request', {
        v: 1,
        seq: 1,
        time: '2026-10-16T12:06:00Z',
        type: 'approval_request',
        task: id,
        approval_request: { id: 'r-1', tool: 'deploy', message: 'Deploy to prod?' },
      });
    });
  }

  it('lets a member allow or deny an approval request, and shows who answered', async () => {
    let answered: unknown[] = [];
    const calls = stubHub({
      'GET /v1/me': me('alice@example.com', { infra: 'admin' }),
      'GET /v1/tasks/t-2': Response.json(running),
      'GET /v1/tasks/t-2/approvals': () => Response.json({ approvals: answered }),
      'POST /v1/tasks/t-2/approvals/r-1': (body) => {
        const a = { task: 't-2', request_id: 'r-1', ...(body as object), answered_by: 'alice@example.com', answered_at: '2026-10-16T12:07:00Z', delivered: true };
        answered = [a];
        return Response.json(a);
      },
    });
    open('/tasks/t-2');
    await request();
    expect(await screen.findByText(/Deploy to prod\?/)).toBeTruthy();
    fireEvent.change(await screen.findByLabelText('Reason for r-1'), { target: { value: 'go' } });
    fireEvent.click(screen.getByRole('button', { name: 'Allow' }));
    await waitFor(() => expect(writes(calls)).toHaveLength(1));
    expect(writes(calls)[0]).toMatchObject({ method: 'POST', url: '/v1/tasks/t-2/approvals/r-1', body: { decision: 'allow', reason: 'go' } });
    expect(await screen.findByText(/Allowed by alice@example\.com: go/)).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Allow' })).toBeNull();
  });

  it('shows the API’s refusal of a stale request', async () => {
    stubHub({
      'GET /v1/me': me('dave@example.com', { infra: 'member' }),
      'GET /v1/tasks/t-2': Response.json(running),
      'GET /v1/tasks/t-2/approvals': Response.json({ approvals: [] }),
      'POST /v1/tasks/t-2/approvals/r-1': new Response('stale approval request: its deadline has passed\n', { status: 409 }),
    });
    open('/tasks/t-2');
    await request();
    fireEvent.click(await screen.findByRole('button', { name: 'Deny' }));
    expect(await screen.findByText('Conflict (409): stale approval request: its deadline has passed')).toBeTruthy();
  });

  it('offers no answer to someone outside the team', async () => {
    stubHub({
      'GET /v1/me': me('carol@example.com', { web: 'member' }),
      'GET /v1/tasks/t-2': Response.json(running),
      'GET /v1/tasks/t-2/approvals': Response.json({ approvals: [] }),
    });
    open('/tasks/t-2');
    await request();
    expect(await screen.findByText(/Deploy to prod\?/)).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Allow' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Deny' })).toBeNull();
  });
});
