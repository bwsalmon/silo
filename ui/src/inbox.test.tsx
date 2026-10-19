// Unified Inbox, Deployment Switcher, Theme Switcher (light / dark / auto ->
// resolved light/dark propagation via PUT /v1/me/theme), and streamlined Hub
// navigation tests.
import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { App } from './App';
import type { InboxItem, NodeEntry, Task } from './api';

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

const sampleNodes: NodeEntry[] = [
  {
    id: 'grain-alpha',
    repos: ['acme/app'],
    slots: 4,
    owner: 'alice@example.com',
    status: 'online',
    free: 3,
    claimed: 1,
    connected_at: '2026-10-18T10:00:00Z',
    last_seen: '2026-10-18T10:05:00Z',
    features: ['nodeui', 'interactive'],
    ui: { offered: true, team: 'infra', url: 'https://grain-alpha.nodes.hub.example.com/' },
    ui_url: 'https://grain-alpha.nodes.hub.example.com/',
    viewer_role: 'admin',
    require_approval: false,
  },
  {
    id: 'grain-beta',
    repos: ['acme/docs'],
    slots: 2,
    owner: 'bob@example.com',
    status: 'online',
    free: 2,
    claimed: 0,
    connected_at: '2026-10-18T10:00:00Z',
    last_seen: '2026-10-18T10:05:00Z',
    features: ['nodeui'],
    ui: { offered: true, team: 'docs', url: 'https://grain-beta.nodes.hub.example.com/' },
    ui_url: 'https://grain-beta.nodes.hub.example.com/',
    viewer_role: 'proposer',
    require_approval: true,
  },
];

const sampleInboxItems: InboxItem[] = [
  {
    id: 'awaiting_reply:t-question',
    kind: 'awaiting_reply',
    task_id: 't-question',
    node_id: 'grain-alpha',
    node_ui_url: 'https://grain-alpha.nodes.hub.example.com/',
    team: 'infra',
    repo: 'acme/app',
    task_kind: 'interactive',
    title: 'Plan DB migration',
    state: 'running',
    question: 'Which database migration strategy should I use?',
    summary: 'Which database migration strategy should I use?',
    submitter: 'alice@example.com',
    viewer_role: 'admin',
    can_act: true,
    updated_at: '2026-10-18T10:10:00Z',
  },
  {
    id: 'approval_request:t-approval:req-deploy',
    kind: 'approval_request',
    task_id: 't-approval',
    node_id: 'grain-alpha',
    node_ui_url: 'https://grain-alpha.nodes.hub.example.com/',
    team: 'infra',
    repo: 'acme/app',
    title: 'Rollout v2',
    state: 'running',
    approval_id: 'req-deploy',
    tool: 'kubectl_apply',
    summary: 'kubectl_apply: Apply production manifest',
    submitter: 'alice@example.com',
    viewer_role: 'admin',
    can_act: true,
    updated_at: '2026-10-18T10:09:00Z',
  },
  {
    id: 'proposal:t-proposal',
    kind: 'proposal',
    task_id: 't-proposal',
    node_id: 'grain-alpha',
    node_ui_url: 'https://grain-alpha.nodes.hub.example.com/',
    team: 'infra',
    repo: 'acme/app',
    title: 'Proposed improvement',
    state: 'proposed',
    summary: 'Add dark mode to docs',
    submitter: 'carol@example.com',
    proposer: 'carol@example.com',
    viewer_role: 'admin',
    can_act: true,
    updated_at: '2026-10-18T10:08:00Z',
  },
  {
    id: 'failed:t-failed',
    kind: 'failed',
    task_id: 't-failed',
    node_id: 'grain-alpha',
    node_ui_url: 'https://grain-alpha.nodes.hub.example.com/',
    team: 'infra',
    repo: 'acme/app',
    title: 'Fix flaky integration test',
    state: 'failed',
    reason: 'exit status 1',
    summary: 'exit status 1',
    submitter: 'alice@example.com',
    viewer_role: 'admin',
    can_act: true,
    updated_at: '2026-10-18T10:07:00Z',
  },
];

const questionTask: Task = {
  id: 't-question',
  team: 'infra',
  repo: 'acme/app',
  kind: 'interactive',
  title: 'Plan DB migration',
  prompt: 'Design the migration.',
  submitter: 'alice@example.com',
  state: 'running',
  node: 'grain-alpha',
  attempts: 1,
  created_at: '2026-10-18T10:00:00Z',
  updated_at: '2026-10-18T10:10:00Z',
};

describe('Deployment Switcher, Theme Switcher, and Unified Inbox', () => {
  it('shows streamlined Hub navigation, Deployment Switcher with 5-role chips, and Theme Switcher syncing resolved light/dark to PUT /v1/me/theme', async () => {
    localStorage.clear();
    const calls = stubHub({
      'GET /v1/me': () =>
        Response.json({
          email: 'alice@example.com',
          hub_admin: false,
          teams: { infra: 'admin', docs: 'proposer' },
          theme: 'light',
          inbox_nodes: ['grain-alpha'],
          inbox_configured: false,
        }),
      'GET /v1/nodes': () => Response.json({ nodes: sampleNodes }),
      'GET /v1/inbox': () => Response.json({ items: sampleInboxItems }),
      'PUT /v1/me/theme': (body) => Response.json({ theme: (body as { theme: string }).theme }),
    });

    const { container } = open('/');
    await screen.findByText('Unified Inbox');

    // Backlog and File a task buttons are retired from top-level AppBar.
    const banner = screen.getByRole('banner');
    expect(within(banner).queryByRole('button', { name: 'Backlog' })).toBeNull();
    expect(within(banner).queryByRole('button', { name: 'File a task' })).toBeNull();
    expect(within(banner).getByRole('button', { name: 'Inbox' })).toBeTruthy();
    expect(within(banner).getByRole('button', { name: 'Grainfiles' })).toBeTruthy();
    expect(within(banner).getByRole('button', { name: 'Teams' })).toBeTruthy();
    expect(within(banner).getByRole('button', { name: 'Nodes' })).toBeTruthy();
    expect(screen.getByTestId('inbox-count-badge').textContent).toBe('4');

    // Deployment Switcher lists accessible deployments and role chips.
    const switcher = screen.getByTestId('deployment-switcher');
    expect(switcher.textContent).toContain('All deployments');
    fireEvent.mouseDown(switcher);
    expect(await screen.findByRole('option', { name: 'Grain Hub (All deployments)' })).toBeTruthy();
    const alphaOpt = await screen.findByTestId('deployment-option-grain-alpha');
    expect(within(alphaOpt).getByText('grain-alpha')).toBeTruthy();
    expect(within(alphaOpt).getByText('infra')).toBeTruthy();
    expect(within(alphaOpt).getByText('admin')).toBeTruthy();
    const betaOpt = screen.getByTestId('deployment-option-grain-beta');
    expect(within(betaOpt).getByText('grain-beta')).toBeTruthy();
    expect(within(betaOpt).getByText('proposer')).toBeTruthy();

    // Close deployment menu and switch theme mode to dark.
    fireEvent.click(screen.getByRole('option', { name: 'Grain Hub (All deployments)' }));
    const themeSelect = screen.getByTestId('theme-mode-select');
    fireEvent.mouseDown(themeSelect);
    fireEvent.click(await screen.findByRole('option', { name: 'dark' }));

    await waitFor(() => {
      const themeCalls = calls.filter((c) => c.method === 'PUT' && c.url === '/v1/me/theme');
      expect(themeCalls).toHaveLength(1);
      expect(themeCalls[0].body).toEqual({ theme: 'dark' });
    });
    expect(container.querySelector('[data-theme="dark"]')).toBeTruthy();
    localStorage.clear();
  });

  it('supports deployment selection, kind filtering, inline quick actions, and Open in <node> Grain UI links on Unified Inbox and TaskPage', async () => {
    localStorage.clear();
    const calls = stubHub({
      'GET /v1/me': () =>
        Response.json({
          email: 'alice@example.com',
          hub_admin: false,
          teams: { infra: 'admin', docs: 'proposer' },
          theme: 'light',
          inbox_nodes: ['grain-alpha'],
          inbox_configured: false,
        }),
      'GET /v1/nodes': () => Response.json({ nodes: sampleNodes }),
      'GET /v1/inbox': (_b, url) => {
        const u = new URL(url, 'http://localhost');
        const kind = u.searchParams.get('kind');
        const items = kind ? sampleInboxItems.filter((i) => i.kind === kind) : sampleInboxItems;
        return Response.json({ items });
      },
      'PUT /v1/me/inbox': (body) =>
        Response.json({ nodes: (body as { nodes: string[] }).nodes, configured: true }),
      'POST /v1/tasks/t-question/messages': (body) =>
        Response.json({ task: 't-question', ...(body as object), sender: 'alice@example.com' }, { status: 201 }),
      'POST /v1/tasks/t-approval/approvals/req-deploy': (body) =>
        Response.json({ task: 't-approval', request_id: 'req-deploy', ...(body as object), answered_by: 'alice@example.com' }),
      'POST /v1/tasks/t-proposal/approve': () =>
        Response.json({ id: 't-proposal', state: 'queued', approved_by: 'alice@example.com' }),
      'POST /v1/tasks/t-failed/requeue': () =>
        Response.json({ id: 't-failed', state: 'queued' }),
      'GET /v1/tasks/t-question': () => Response.json(questionTask),
      'GET /v1/tasks/t-question/messages': () => Response.json({ messages: [] }),
    });

    open('/inbox');
    await screen.findByText('Plan DB migration');
    expect(screen.getByText('Which database migration strategy should I use?')).toBeTruthy();
    expect(screen.getByText(/kubectl_apply: Apply production manifest/)).toBeTruthy();
    expect(screen.getByText('Proposed improvement')).toBeTruthy();
    expect(screen.getByText('Fix flaky integration test')).toBeTruthy();

    // Open in <node> Grain UI links on inbox cards
    const openLinks = screen.getAllByTestId('open-in-node-ui');
    expect(openLinks.length).toBeGreaterThanOrEqual(4);
    expect(openLinks[0].getAttribute('href')).toBe('https://grain-alpha.nodes.hub.example.com/');
    expect(openLinks[0].textContent).toBe('Open in grain-alpha Grain UI');

    // Toggle deployment in inbox-deployment-selector
    const selector = screen.getByTestId('inbox-deployment-selector');
    fireEvent.click(within(selector).getByTestId('inbox-deployment-grain-beta'));
    await waitFor(() => {
      const inboxPut = calls.find((c) => c.method === 'PUT' && c.url === '/v1/me/inbox');
      expect(inboxPut?.body).toEqual({ nodes: ['grain-alpha', 'grain-beta'] });
    });

    // Inline quick action 1: Reply to awaiting_reply
    const questionCard = screen.getByTestId('inbox-item-t-question');
    fireEvent.change(within(questionCard).getByLabelText('Reply to the session'), {
      target: { value: 'Use blue-green migration.' },
    });
    fireEvent.click(within(questionCard).getByRole('button', { name: 'Send' }));
    await waitFor(() => {
      const msgCall = calls.find((c) => c.method === 'POST' && c.url === '/v1/tasks/t-question/messages');
      expect(msgCall?.body).toMatchObject({ text: 'Use blue-green migration.' });
    });

    // Inline quick action 2: Allow approval_request
    const approvalCard = screen.getByTestId('inbox-item-t-approval');
    fireEvent.click(within(approvalCard).getByRole('button', { name: 'Allow' }));
    await waitFor(() => {
      const appCall = calls.find((c) => c.method === 'POST' && c.url === '/v1/tasks/t-approval/approvals/req-deploy');
      expect(appCall?.body).toEqual({ decision: 'allow' });
    });

    // Inline quick action 3: Approve proposal
    const proposalCard = screen.getByTestId('inbox-item-t-proposal');
    fireEvent.click(within(proposalCard).getByRole('button', { name: 'Approve' }));
    await waitFor(() => {
      expect(calls.some((c) => c.method === 'POST' && c.url === '/v1/tasks/t-proposal/approve')).toBe(true);
    });

    // Inline quick action 4: Requeue failed task
    const failedCard = screen.getByTestId('inbox-item-t-failed');
    fireEvent.click(within(failedCard).getByRole('button', { name: 'Requeue' }));
    await waitFor(() => {
      expect(calls.some((c) => c.method === 'POST' && c.url === '/v1/tasks/t-failed/requeue')).toBe(true);
    });

    // Filter by kind
    fireEvent.click(screen.getByTestId('inbox-kind-failed'));
    await waitFor(() => {
      expect(calls.some((c) => c.method === 'GET' && c.url.includes('/v1/inbox') && c.url.includes('kind=failed'))).toBe(true);
    });

    // Navigate to TaskPage and verify Open in <node> Grain UI link is shown
    fireEvent.click(screen.getByTestId('inbox-kind-all'));
    fireEvent.click(await screen.findByRole('link', { name: 'Plan DB migration' }));
    const taskOpenLink = await screen.findByTestId('open-in-node-ui');
    expect(taskOpenLink.getAttribute('href')).toBe('https://grain-alpha.nodes.hub.example.com/');
    expect(taskOpenLink.textContent).toBe('Open in grain-alpha Grain UI');
  });
});
