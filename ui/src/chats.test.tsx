import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { App } from './App';
import type { ChatMessage, ChatSession } from './api';

type Handler = (body: unknown, url: string) => Response;

interface Call {
  method: string;
  url: string;
  body?: unknown;
}

function stubHub(routes: Record<string, Handler | Response>) {
  const calls: Call[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init?: RequestInit) => {
      const method = init?.method ?? 'GET';
      const body = typeof init?.body === 'string' ? JSON.parse(init.body) : undefined;
      calls.push({ method, url, body });
      const exact = routes[`${method} ${url}`];
      if (exact) return typeof exact === 'function' ? exact(body, url) : exact.clone();
      const basePath = url.split('?')[0];
      const base = routes[`${method} ${basePath}`];
      if (base) return typeof base === 'function' ? base(body, url) : base.clone();
      return new Response('not found\n', { status: 404 });
    }),
  );
  return calls;
}

function open(path: string) {
  window.history.replaceState(null, '', path);
  return render(<App />);
}

const sampleChat: ChatSession = {
  id: 'c-0102030405060708',
  task_id: 't-0102030405060708',
  title: 'Payments Ops Room',
  deployment: 'ops-agent',
  linked_deployments: ['design-agent'],
  reply_mode: 'mention',
  participants: [
    { email: 'alice@example.com', role: 'participant', source: 'deployment' },
    { email: 'pm@example.com', role: 'participant', source: 'invited' },
  ],
  repos: ['bwsalmon/silo'],
  references: ['https://docs.example.com/payments-spec'],
  external_bindings: [{ provider: 'google_chat', external_id: 'spaces/AAA123' }],
  state: 'running',
  awaiting_reply: true,
  question: 'Should we roll out the 500ms jitter backoff now?',
  proposed_tasks: [
    {
      id: 't-9999999999999999',
      team: 'ops-agent',
      title: 'Implement 500ms jitter backoff',
      prompt: 'Update retry client',
      submitter: 'alice@example.com',
      origin_task: 't-0102030405060708',
      state: 'proposed',
      attempts: 0,
      created_at: '2026-10-19T12:00:00Z',
      updated_at: '2026-10-19T12:00:00Z',
    },
  ],
  messages: [
    {
      seq: 1,
      id: 'm-1',
      chat_id: 'c-0102030405060708',
      sender: 'alice@example.com',
      role: 'user',
      text: 'Hey @pm@example.com what do you think of 500ms backoff?',
      triggered_agent: false,
      created_at: '2026-10-19T12:01:00Z',
    },
    {
      seq: 2,
      id: 'ev-10',
      chat_id: 'c-0102030405060708',
      sender: 'ops-agent',
      role: 'agent',
      text: 'I drafted the retry policy and proposed task t-9999999999999999.',
      created_at: '2026-10-19T12:02:00Z',
    },
  ],
  created_by: 'alice@example.com',
  created_at: '2026-10-19T12:00:00Z',
  updated_at: '2026-10-19T12:02:00Z',
};

describe('Shared Chat Sessions UI (/chats and /chats/:id)', () => {
  it('renders active chat rooms, multi-party transcript, Ask @<deployment>, reply_mode toggle, participants drawer, and external bindings', async () => {
    let currentChat: ChatSession = { ...sampleChat };
    const calls = stubHub({
      'GET /v1/me': new Response(
        JSON.stringify({
          email: 'alice@example.com',
          hub_admin: false,
          teams: { 'ops-agent': 'admin', 'design-agent': 'member' },
        }),
      ),
      'GET /v1/nodes': new Response(JSON.stringify({ nodes: [] })),
      'GET /v1/inbox': new Response(JSON.stringify({ items: [] })),
      'GET /v1/deployments': new Response(
        JSON.stringify({
          deployments: [
            { id: 'ops-agent', name: 'ops-agent', members: { 'user:alice@example.com': 'admin' }, repos: ['bwsalmon/silo'] },
          ],
        }),
      ),
      'GET /v1/chats': () => new Response(JSON.stringify({ chats: [currentChat] })),
      'GET /v1/chats/c-0102030405060708': () => new Response(JSON.stringify(currentChat)),
      'GET /v1/chats/c-0102030405060708/events': new Response('event: end\ndata: {"type":"end"}\n\n', { status: 200 }),
      'POST /v1/chats/c-0102030405060708/messages': (body) => {
        const req = body as { id: string; text: string; trigger_agent?: boolean };
        const msg: ChatMessage = {
          seq: (currentChat.messages?.length ?? 0) + 1,
          id: req.id,
          chat_id: currentChat.id,
          sender: 'alice@example.com',
          role: 'user',
          text: req.text,
          triggered_agent: Boolean(req.trigger_agent),
          created_at: '2026-10-19T12:05:00Z',
        };
        currentChat = { ...currentChat, messages: [...(currentChat.messages ?? []), msg] };
        return new Response(JSON.stringify(msg), { status: 201 });
      },
      'PATCH /v1/chats/c-0102030405060708': (body) => {
        const patch = body as Partial<ChatSession>;
        currentChat = { ...currentChat, ...patch };
        return new Response(JSON.stringify(currentChat), { status: 200 });
      },
    });

    open('/chats/c-0102030405060708');

    expect((await screen.findByTestId('active-chat-title')).textContent).toContain('Payments Ops Room');
    expect(screen.getByTestId('active-chat-deployment').textContent).toContain('@ops-agent');
    expect(screen.getByTestId('chat-transcript').textContent).toContain('Hey @pm@example.com what do you think of 500ms backoff?');
    expect(screen.getByTestId('chat-transcript').textContent).toContain('I drafted the retry policy and proposed task t-9999999999999999.');
    expect(screen.getByTestId('proposed-task-t-9999999999999999').textContent).toContain('Implement 500ms jitter backoff');
    expect(screen.getByTestId('participant-row-pm@example.com').textContent).toContain('pm@example.com');
    expect((screen.getByTestId('drawer-google-chat-space') as HTMLInputElement).value).toBe('spaces/AAA123');

    // Click "Ask @ops-agent" button to wake agent with trigger_agent: true
    const input = screen.getByTestId('chat-message-input');
    fireEvent.change(input, { target: { value: 'Please roll out to staging first.' } });
    fireEvent.click(screen.getByTestId('ask-agent-btn'));

    await waitFor(() => {
      const postCall = calls.find((c) => c.method === 'POST' && c.url === '/v1/chats/c-0102030405060708/messages');
      expect(postCall).toBeDefined();
      expect((postCall?.body as { trigger_agent?: boolean; text?: string })?.trigger_agent).toBe(true);
      expect((postCall?.body as { text?: string })?.text).toBe('Please roll out to staging first.');
    });

    // Toggle reply_mode to 'all'
    fireEvent.click(screen.getByTestId('reply-mode-all-btn'));
    await waitFor(() => {
      const patchCall = calls.find((c) => c.method === 'PATCH' && c.url === '/v1/chats/c-0102030405060708');
      expect(patchCall).toBeDefined();
      expect((patchCall?.body as { reply_mode?: string })?.reply_mode).toBe('all');
    });
  });
});
