// U4 (docs/ui.md §6): interactive sessions. "Start a session" files an
// interactive task; its page shows a clear "waiting for you" while its
// agent waits on a human, renders the messages sent to it as they
// stream, and gives its submitter and the members of its team a reply
// box that posts through the public API with the browser's own
// credentials, retrying a failed send as the same message.
import { describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { App } from './App';
import * as api from './api';
import { awaiting } from './events';
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

const session = {
  id: 't-3',
  team: 'infra',
  repo: 'acme/app',
  kind: 'interactive',
  title: 'Pair on the parser',
  prompt: 'Let’s look at the parser together.',
  submitter: 'dave@example.com',
  created_at: '2026-10-17T12:00:00Z',
  updated_at: '2026-10-17T12:01:00Z',
  state: 'running',
  node: 'ws-1',
  attempts: 1,
};

function open(path: string) {
  window.history.replaceState(null, '', path);
  render(<App />);
}

const ev = (seq: number, type: string, payload: object) => ({ v: 1, seq, time: '2026-10-17T12:02:00Z', type, task: 't-3', ...payload });

async function stream() {
  await waitFor(() => expect(FakeEventSource.instances.length).toBe(1));
  const es = FakeEventSource.instances[0];
  act(() => es.open());
  return (type: string, e: object) => act(() => es.emit(type, e));
}

describe('the API layer’s session calls', () => {
  it('sends a message by its ID, same-origin, with no token', async () => {
    const calls = stubHub({ 'POST /v1/tasks/t%203/messages': Response.json({}, { status: 201 }) });
    await api.sendMessage('t 3', 'm-1', 'hello');
    expect(calls).toHaveLength(1);
    expect(calls[0]).toMatchObject({ method: 'POST', url: '/v1/tasks/t%203/messages', body: { id: 'm-1', text: 'hello' } });
    expect(calls[0].init?.credentials).toBe('same-origin');
    expect(JSON.stringify(calls[0].init?.headers ?? {})).not.toMatch(/authorization/i);
  });

  it('lists a session’s messages', async () => {
    stubHub({ 'GET /v1/tasks/t-3/messages': Response.json({ messages: [{ id: 'm-1', sender: 'dave@example.com' }] }) });
    expect((await api.listMessages('t-3'))[0].sender).toBe('dave@example.com');
  });

  it('reads the wait from the last awaiting_reply event', () => {
    const w = ev(1, 'awaiting_reply', { awaiting_reply: { state: 'waiting', question: 'Which branch?' } }) as api.HubEvent;
    const r = ev(2, 'awaiting_reply', { awaiting_reply: { state: 'resumed' } }) as api.HubEvent;
    const s = ev(3, 'status', { status: { phase: 'running', state: 'info' } }) as api.HubEvent;
    expect(awaiting([])).toBeUndefined();
    expect(awaiting([w, s])).toBe('Which branch?');
    expect(awaiting([w, r, s])).toBeUndefined();
  });
});

describe('starting a session', () => {
  it('files the form as an interactive task, and opens it', async () => {
    const calls = stubHub({
      'GET /v1/me': () => me('dave@example.com', { infra: 'member' }),
      'GET /v1/teams': () => Response.json({ teams: [{ id: 'infra', name: 'Infra', repos: ['acme/app'] }] }),
      'POST /v1/tasks': (b) => Response.json({ ...session, ...(b as object) }, { status: 201 }),
      'GET /v1/tasks/t-3': () => Response.json(session),
      'GET /v1/tasks/t-3/messages': () => Response.json({ messages: [] }),
    });
    open('/teams/infra/file');
    fireEvent.change(await screen.findByLabelText(/Prompt/), { target: { value: 'Pair with me.' } });
    const start = screen.getByRole('button', { name: 'Start a session' }) as HTMLButtonElement;
    await waitFor(() => expect(start.disabled).toBe(false));
    fireEvent.click(start);
    await waitFor(() => expect(window.location.pathname).toBe('/tasks/t-3'));
    expect(writes(calls).map((c) => [c.method, c.url, c.body])).toEqual([
      ['POST', '/v1/tasks', { team: 'infra', kind: 'interactive', title: '', prompt: 'Pair with me.' }],
    ]);
  });
});

describe('a session’s page', () => {
  it('shows "waiting for you" while the agent waits, and not once it goes on', async () => {
    stubHub({
      'GET /v1/me': me('dave@example.com', { infra: 'member' }),
      'GET /v1/tasks/t-3': Response.json(session),
      'GET /v1/tasks/t-3/messages': Response.json({ messages: [] }),
    });
    open('/tasks/t-3');
    const emit = await stream();
    expect(screen.queryByText('Waiting for you.')).toBeNull();
    emit('awaiting_reply', ev(1, 'awaiting_reply', { awaiting_reply: { state: 'waiting', question: 'Which branch should I use?' } }));
    expect(await screen.findByText('Waiting for you.')).toBeTruthy();
    expect(screen.getByText('waiting for you')).toBeTruthy();
    expect(screen.getAllByText('Which branch should I use?').length).toBeGreaterThan(0);
    emit('message', ev(2, 'message', { message: { id: 'm-1', text: 'Use main.', sender: 'alice@example.com', sent_at: '2026-10-17T12:03:00Z' } }));
    emit('awaiting_reply', ev(3, 'awaiting_reply', { awaiting_reply: { state: 'resumed', message_id: 'm-1' } }));
    await waitFor(() => expect(screen.queryByText('Waiting for you.')).toBeNull());
    // The message is in the conversation, as the hub recorded it.
    expect(screen.getByText('Use main.')).toBeTruthy();
    expect(screen.getByText('alice@example.com')).toBeTruthy();
  });

  it('lets a member reply, shows the message until the session takes it in, then once', async () => {
    let kept: api.TaskMessage[] = [];
    const calls = stubHub({
      'GET /v1/me': me('alice@example.com', { infra: 'admin' }),
      'GET /v1/tasks/t-3': Response.json(session),
      'GET /v1/tasks/t-3/messages': () => Response.json({ messages: kept }),
      'POST /v1/tasks/t-3/messages': (b) => {
        const m = { task: 't-3', ...(b as { id: string; text: string }), attachments: [], sender: 'alice@example.com', sent_at: '2026-10-17T12:03:00Z', delivered: false };
        kept = [m];
        return Response.json(m, { status: 201 });
      },
    });
    open('/tasks/t-3');
    const emit = await stream();
    const box = await screen.findByLabelText('Reply to the session');
    fireEvent.change(box, { target: { value: 'Use main, please.' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(writes(calls)).toHaveLength(1));
    const sent = writes(calls)[0];
    expect(sent).toMatchObject({ method: 'POST', url: '/v1/tasks/t-3/messages', body: { text: 'Use main, please.' } });
    expect((sent.body as { id: string }).id).toMatch(/^m-/);
    expect(await screen.findByText(/waiting to be sent to the session/)).toBeTruthy();
    await waitFor(() => expect((box as HTMLTextAreaElement).value).toBe(''));

    emit('message', ev(1, 'message', { message: { id: (sent.body as { id: string }).id, text: 'Use main, please.', sender: 'alice@example.com' } }));
    await waitFor(() => expect(screen.queryByText(/waiting to be sent to the session/)).toBeNull());
    expect(screen.getAllByText('Use main, please.')).toHaveLength(1);
  });

  it('retries a failed send as the same message', async () => {
    let fail = true;
    const calls = stubHub({
      'GET /v1/me': me('dave@example.com', { infra: 'member' }),
      'GET /v1/tasks/t-3': Response.json(session),
      'GET /v1/tasks/t-3/messages': Response.json({ messages: [] }),
      'POST /v1/tasks/t-3/messages': (b) => {
        if (fail) {
          fail = false;
          return new Response('the database is away\n', { status: 503 });
        }
        return Response.json({ task: 't-3', ...(b as object), sender: 'dave@example.com' }, { status: 201 });
      },
    });
    open('/tasks/t-3');
    await stream();
    fireEvent.change(await screen.findByLabelText('Reply to the session'), { target: { value: 'hello' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    expect(await screen.findByText(/the database is away/)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    await waitFor(() => expect(writes(calls)).toHaveLength(2));
    const [a, b] = writes(calls).map((c) => (c.body as { id: string }).id);
    expect(a).toBe(b);
  });

  it('shows the API’s refusal of a message to a session that is over', async () => {
    stubHub({
      'GET /v1/me': me('dave@example.com', { infra: 'member' }),
      'GET /v1/tasks/t-3': Response.json(session),
      'GET /v1/tasks/t-3/messages': Response.json({ messages: [] }),
      'POST /v1/tasks/t-3/messages': new Response('task t-3 is done: it takes no messages\n', { status: 409 }),
    });
    open('/tasks/t-3');
    await stream();
    fireEvent.change(await screen.findByLabelText('Reply to the session'), { target: { value: 'hello' } });
    fireEvent.click(screen.getByRole('button', { name: 'Send' }));
    expect(await screen.findByText('Conflict (409): task t-3 is done: it takes no messages')).toBeTruthy();
  });

  it.each([
    ['a hub admin outside the team', me('root@example.com', {}, true)],
    ['someone in another team', me('carol@example.com', { web: 'member' })],
  ])('offers no reply box to %s', async (_, who) => {
    stubHub({
      'GET /v1/me': who,
      'GET /v1/tasks/t-3': Response.json(session),
      'GET /v1/tasks/t-3/messages': Response.json({ messages: [] }),
    });
    open('/tasks/t-3');
    await stream();
    expect(await screen.findByText('Interactive session')).toBeTruthy();
    expect(screen.queryByLabelText('Reply to the session')).toBeNull();
  });

  it('offers its submitter a reply box, though they are outside its team', async () => {
    stubHub({
      'GET /v1/me': me('dave@example.com', { web: 'member' }),
      'GET /v1/tasks/t-3': Response.json(session),
      'GET /v1/tasks/t-3/messages': Response.json({ messages: [] }),
    });
    open('/tasks/t-3');
    expect(await screen.findByLabelText('Reply to the session')).toBeTruthy();
  });

  it('offers no reply box on a session that is over', async () => {
    stubHub({
      'GET /v1/me': me('dave@example.com', { infra: 'member' }),
      'GET /v1/tasks/t-3': Response.json({ ...session, state: 'done' }),
      'GET /v1/tasks/t-3/messages': Response.json({ messages: [] }),
    });
    open('/tasks/t-3');
    expect(await screen.findByText('Interactive session')).toBeTruthy();
    expect(screen.queryByLabelText('Reply to the session')).toBeNull();
  });

  it('offers no reply box, and fetches no messages, on an ordinary task', async () => {
    const calls = stubHub({
      'GET /v1/me': me('dave@example.com', { infra: 'member' }),
      'GET /v1/tasks/t-4': Response.json({ ...session, id: 't-4', kind: undefined }),
    });
    open('/tasks/t-4');
    await stream();
    expect(await screen.findByText('Events')).toBeTruthy();
    expect(screen.queryByLabelText('Reply to the session')).toBeNull();
    expect(calls.some((c) => c.url.endsWith('/messages'))).toBe(false);
  });
});
