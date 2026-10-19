// U2's writes (docs/ui.md §6): filing, cancel and requeue, team admin
// and node tokens. Each form and button sends the public API's request
// with the browser's own credentials and nothing else, shows its edit
// actions only where /v1/me says the caller may, and shows the API's
// refusals as the API says them rather than deciding them itself.
import { describe, expect, it, vi } from 'vitest';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { App } from './App';
import * as api from './api';

type Handler = (body: unknown) => Response;

interface Call {
  method: string;
  url: string;
  init?: RequestInit;
  body?: unknown;
}

// stubHub answers "METHOD /path" from routes, anything else with a 404,
// and records every call.
function stubHub(routes: Record<string, Handler | Response>) {
  const calls: Call[] = [];
  const f = vi.fn(async (url: string, init?: RequestInit) => {
    const method = init?.method ?? 'GET';
    const body = typeof init?.body === 'string' ? JSON.parse(init.body) : undefined;
    calls.push({ method, url, init, body });
    const h = routes[`${method} ${url}`];
    if (!h) return new Response('no such thing\n', { status: 404 });
    return typeof h === 'function' ? h(body) : h.clone();
  });
  vi.stubGlobal('fetch', f);
  return calls;
}

const writes = (calls: Call[]) => calls.filter((c) => c.method !== 'GET');

const me = (email: string, teams: Record<string, string>, hubAdmin = false) => Response.json({ email, hub_admin: hubAdmin, teams });

const infra = {
  id: 'infra',
  name: 'Infra',
  members: { 'user:alice@example.com': 'admin', 'user:bob@example.com': 'member' },
  repos: ['acme/app', 'acme/lib'],
};

const task = {
  id: 't-1',
  team: 'infra',
  repo: 'acme/app',
  title: 'Fix the thing',
  prompt: 'Fix it.',
  submitter: 'bob@example.com',
  created_at: '2026-10-16T12:00:00Z',
  updated_at: '2026-10-16T12:01:00Z',
  state: 'queued',
  attempts: 0,
};

function open(path: string) {
  window.history.replaceState(null, '', path);
  render(<App />);
}

async function choose(label: string, option: string) {
  let opt: HTMLElement | undefined;
  await waitFor(async () => {
    const box = await screen.findByRole('combobox', { name: label });
    fireEvent.mouseDown(box);
    opt = screen.getByRole('option', { name: option });
  });
  fireEvent.click(opt!);
}

describe('the API layer’s writes', () => {
  // Each write as the hub's identity.SameOrigin wants it: same-origin
  // credentials, JSON for a body, and no credential of the UI's own.
  const cases: [string, () => Promise<unknown>, string, string, unknown][] = [
    ['submitTask', () => api.submitTask({ repo: 'acme/app', title: 't', prompt: 'p' }), 'POST', '/v1/tasks', { repo: 'acme/app', title: 't', prompt: 'p' }],
    ['cancelTask', () => api.cancelTask('t 1', 'stale'), 'POST', '/v1/tasks/t%201/cancel', { reason: 'stale' }],
    ['cancelTask, no reason', () => api.cancelTask('t-1'), 'POST', '/v1/tasks/t-1/cancel', undefined],
    ['requeueTask', () => api.requeueTask('t-1'), 'POST', '/v1/tasks/t-1/requeue', undefined],
    ['createTeam', () => api.createTeam('infra', 'Infra'), 'POST', '/v1/teams', { id: 'infra', name: 'Infra' }],
    ['deleteTeam', () => api.deleteTeam('infra'), 'DELETE', '/v1/teams/infra', undefined],
    ['setMember', () => api.setMember('infra', 'user:a@example.com', 'admin'), 'PUT', '/v1/teams/infra/members/user%3Aa%40example.com', { role: 'admin' }],
    ['removeMember', () => api.removeMember('infra', 'group:ops'), 'DELETE', '/v1/teams/infra/members/group%3Aops', undefined],
    ['assignRepo', () => api.assignRepo('infra', 'acme/app', false), 'PUT', '/v1/teams/infra/repos/acme/app', undefined],
    ['assignRepo, history', () => api.assignRepo('infra', 'acme/app', true), 'PUT', '/v1/teams/infra/repos/acme/app', { history: true }],
    ['releaseRepo', () => api.releaseRepo('infra', 'acme/app'), 'DELETE', '/v1/teams/infra/repos/acme/app', undefined],
    ['setTeamSettings', () => api.setTeamSettings('infra', { lost_requeue_after_seconds: 60 }), 'PUT', '/v1/teams/infra/settings', { lost_requeue_after_seconds: 60 }],
    ['setUser', () => api.setUser('dev@example.com', true), 'PUT', '/v1/users/dev%40example.com', { sys_admin: true }],
    ['deleteUser', () => api.deleteUser('dev@example.com'), 'DELETE', '/v1/users/dev%40example.com', undefined],
    ['createDeployment', () => api.createDeployment('payments-prod', 'Payments'), 'POST', '/v1/deployments', { id: 'payments-prod', name: 'Payments' }],
    ['deleteDeployment', () => api.deleteDeployment('payments-prod'), 'DELETE', '/v1/deployments/payments-prod', undefined],
    ['setDeploymentMember', () => api.setDeploymentMember('payments-prod', 'user:a@example.com', 'admin'), 'PUT', '/v1/deployments/payments-prod/members/user%3Aa%40example.com', { role: 'admin' }],
    ['removeDeploymentMember', () => api.removeDeploymentMember('payments-prod', 'group:ops'), 'DELETE', '/v1/deployments/payments-prod/members/group%3Aops', undefined],
    ['assignDeploymentRepo', () => api.assignDeploymentRepo('payments-prod', 'acme/pay', false), 'PUT', '/v1/deployments/payments-prod/repos/acme/pay', undefined],
    ['releaseDeploymentRepo', () => api.releaseDeploymentRepo('payments-prod', 'acme/pay'), 'DELETE', '/v1/deployments/payments-prod/repos/acme/pay', undefined],
    ['setDeploymentSettings', () => api.setDeploymentSettings('payments-prod', { lost_requeue_after_seconds: 30 }), 'PUT', '/v1/deployments/payments-prod/settings', { lost_requeue_after_seconds: 30 }],
    ['mintDeploymentToken', () => api.mintDeploymentToken('payments-prod', { replace_host: true }), 'POST', '/v1/deployments/payments-prod/token', { replace_host: true }],
    ['replaceDeploymentHost', () => api.replaceDeploymentHost('payments-prod'), 'POST', '/v1/deployments/payments-prod/replace-host', undefined],
    ['mintNodeToken', () => api.mintNodeToken('ws-1'), 'POST', '/v1/node-tokens', { node: 'ws-1' }],
    ['revokeNodeToken', () => api.revokeNodeToken('tok-1'), 'DELETE', '/v1/node-tokens/tok-1', undefined],
  ];
  it.each(cases)('%s', async (_name, call, method, url, body) => {
    const calls = stubHub({ [`${method} ${url}`]: () => Response.json({}, { status: 200 }) });
    await call();
    expect(calls).toHaveLength(1);
    const c = calls[0];
    expect([c.method, c.url, c.body]).toEqual([method, url, body]);
    expect(c.init?.credentials).toBe('same-origin');
    const headers = new Headers(c.init?.headers);
    expect(headers.get('Authorization')).toBeNull();
    expect(headers.get('Content-Type')).toBe(body === undefined ? null : 'application/json');
  });

  it('asks for a minted token uncached', async () => {
    const calls = stubHub({ 'POST /v1/node-tokens': () => Response.json({ token: 'silo-node_x', node_token: { id: 'k', node: 'x' } }, { status: 201 }) });
    await api.mintNodeToken('x');
    expect(calls[0].init?.cache).toBe('no-store');
  });

  it('reads one team', async () => {
    const calls = stubHub({ 'GET /v1/teams/infra': () => Response.json(infra) });
    expect(await api.getTeam('infra')).toEqual(infra);
    expect(calls[0].url).toBe('/v1/teams/infra');
  });

  it('answers a refusal as an ApiError with the API’s words', async () => {
    stubHub({ 'POST /v1/tasks/t-1/requeue': () => new Response('tasks: not allowed in the task\'s state\n', { status: 409 }) });
    const err = await api.requeueTask('t-1').catch((e: unknown) => e);
    expect(err).toBeInstanceOf(api.ApiError);
    expect((err as api.ApiError).status).toBe(409);
    expect((err as api.ApiError).message).toBe("tasks: not allowed in the task's state");
  });
});

describe('filing a task', () => {
  const hub = (extra: Record<string, Handler> = {}) =>
    stubHub({
      'GET /v1/me': () => me('bob@example.com', { infra: 'member' }),
      'GET /v1/teams': () => Response.json({ teams: [infra] }),
      'GET /v1/tasks/t-9': () => Response.json({ ...task, id: 't-9' }),
      ...extra,
    });

  it('files to a repository the team owns, and opens the task', async () => {
    const calls = hub({ 'POST /v1/tasks': (b) => Response.json({ ...task, ...(b as object), id: 't-9' }, { status: 201 }) });
    open('/file');
    await choose('Team', 'Infra (infra)');
    await choose('Repository', 'acme/lib');
    fireEvent.change(screen.getByLabelText('Title'), { target: { value: 'Bump it' } });
    fireEvent.change(screen.getByLabelText(/Prompt/), { target: { value: 'Bump the version.' } });
    fireEvent.click(screen.getByRole('button', { name: 'File task' }));
    await waitFor(() => expect(window.location.pathname).toBe('/tasks/t-9'));
    expect(writes(calls).map((c) => [c.method, c.url, c.body])).toEqual([
      ['POST', '/v1/tasks', { repo: 'acme/lib', title: 'Bump it', prompt: 'Bump the version.' }],
    ]);
  });

  it('files to the team when no repository is chosen, with a node if named', async () => {
    const calls = hub({ 'POST /v1/tasks': () => Response.json({ ...task, id: 't-9' }, { status: 201 }) });
    open('/teams/infra/file');
    fireEvent.change(await screen.findByLabelText(/Prompt/), { target: { value: 'Anything.' } });
    fireEvent.change(screen.getByLabelText('Node (optional)'), { target: { value: 'ws-1' } });
    await waitFor(() => expect((screen.getByRole('button', { name: 'File task' }) as HTMLButtonElement).disabled).toBe(false));
    fireEvent.click(screen.getByRole('button', { name: 'File task' }));
    await waitFor(() => expect(writes(calls)).toHaveLength(1));
    expect(writes(calls)[0].body).toEqual({ team: 'infra', title: '', prompt: 'Anything.', constraints: { node: 'ws-1' } });
  });

  it('shows the API’s refusal and stays on the form', async () => {
    hub({ 'POST /v1/tasks': () => new Response('no such repository in your teams: acme/lib\n', { status: 404 }) });
    open('/teams/infra/file');
    await choose('Repository', 'acme/lib');
    fireEvent.change(screen.getByLabelText(/Prompt/), { target: { value: 'x' } });
    fireEvent.click(screen.getByRole('button', { name: 'File task' }));
    expect(await screen.findByText('Not found (404): no such repository in your teams: acme/lib')).toBeTruthy();
    expect(window.location.pathname).toBe('/teams/infra/file');
  });
});

describe('cancel and requeue', () => {
  const hub = (who: string, role: string, extra: Record<string, Handler> = {}) =>
    stubHub({
      'GET /v1/me': () => me(who, { infra: role }),
      'GET /v1/tasks/t-1': () => Response.json(task),
      ...extra,
    });

  it('are not shown to a member who neither filed the task nor administers its team', async () => {
    hub('carol@example.com', 'member');
    open('/tasks/t-1');
    await screen.findByText('Fix the thing');
    await screen.findByText('carol@example.com');
    expect(screen.queryByRole('button', { name: 'Requeue' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Cancel task' })).toBeNull();
  });

  it('cancel with a reason, for the submitter', async () => {
    const calls = hub('bob@example.com', 'member', {
      'POST /v1/tasks/t-1/cancel': () => Response.json({ ...task, state: 'cancelled' }),
    });
    open('/tasks/t-1');
    fireEvent.change(await screen.findByLabelText('Reason to cancel (optional)'), { target: { value: 'not needed' } });
    fireEvent.click(screen.getByRole('button', { name: 'Cancel task' }));
    fireEvent.click(screen.getByRole('button', { name: 'Confirm cancel' }));
    await waitFor(() => expect(writes(calls)).toHaveLength(1));
    expect(writes(calls)[0].body).toEqual({ reason: 'not needed' });
  });

  it('requeue for a team admin, and show the API’s 409 as it says it', async () => {
    const calls = hub('alice@example.com', 'admin', {
      'POST /v1/tasks/t-1/requeue': () => new Response('tasks: not allowed in the task\'s state: task t-1 is done\n', { status: 409 }),
    });
    open('/tasks/t-1');
    fireEvent.click(await screen.findByRole('button', { name: 'Requeue' }));
    expect(await screen.findByText("Conflict (409): tasks: not allowed in the task's state: task t-1 is done")).toBeTruthy();
    expect(writes(calls).map((c) => [c.method, c.url])).toEqual([['POST', '/v1/tasks/t-1/requeue']]);
  });
});

describe('team admin', () => {
  const hub = (who: string, teams: Record<string, string>, hubAdmin: boolean, extra: Record<string, Handler> = {}) =>
    stubHub({
      'GET /v1/me': () => me(who, teams, hubAdmin),
      'GET /v1/teams': () => Response.json({ teams: [infra] }),
      'GET /v1/teams/infra': () => Response.json(infra),
      'GET /v1/teams/infra/settings': () => Response.json({ lost_requeue_after_seconds: 0 }),
      ...extra,
    });

  it('shows a member the team with no edit actions', async () => {
    hub('bob@example.com', { infra: 'member' }, false);
    open('/teams/infra/members');
    await screen.findByText('bob@example.com');
    await screen.findByText('user:alice@example.com');
    expect(screen.getByText('acme/app')).toBeTruthy();
    await screen.findByText(/A lost task is requeued never/);
    for (const name of ['Remove', 'Add member', 'Release', 'Assign', 'Delete team', 'Save']) {
      expect([name, screen.queryByRole('button', { name })]).toEqual([name, null]);
    }
  });

  it('lets a team admin add a member, and shows the API’s refusal', async () => {
    let refuse = false;
    const calls = hub('alice@example.com', { infra: 'admin' }, false, {
      'PUT /v1/teams/infra/members/user%3Acarol%40example.com': () => (refuse ? new Response('forbidden\n', { status: 403 }) : new Response(null, { status: 204 })),
    });
    open('/teams/infra/members');
    fireEvent.change(await screen.findByLabelText('Principal'), { target: { value: 'user:carol@example.com' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add member' }));
    await waitFor(() => expect(writes(calls)).toHaveLength(1));
    expect(writes(calls)[0].body).toEqual({ role: 'member' });
    // A hub admin's (not this admin's) actions are not offered.
    expect(screen.queryByRole('button', { name: 'Assign' })).toBeNull();
    expect(screen.queryByRole('button', { name: 'Delete team' })).toBeNull();

    refuse = true;
    fireEvent.change(screen.getByLabelText('Principal'), { target: { value: 'user:carol@example.com' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add member' }));
    expect(await screen.findByText('Not allowed (403): forbidden')).toBeTruthy();
  });

  it('lets a team admin change a role, remove a member and release a repository', async () => {
    const calls = hub('alice@example.com', { infra: 'admin' }, false, {
      'PUT /v1/teams/infra/members/user%3Abob%40example.com': () => new Response(null, { status: 204 }),
      'DELETE /v1/teams/infra/members/user%3Abob%40example.com': () => new Response(null, { status: 204 }),
      'DELETE /v1/teams/infra/repos/acme/lib': () => new Response(null, { status: 204 }),
    });
    open('/teams/infra/members');
    await choose('Role of user:bob@example.com', 'admin');
    const bob = screen.getByText('user:bob@example.com').closest('tr')!;
    fireEvent.click(within(bob as HTMLElement).getByRole('button', { name: 'Remove' }));
    fireEvent.click(screen.getByRole('button', { name: 'Remove user:bob@example.com' }));
    const lib = screen.getByText('acme/lib').closest('tr')!;
    fireEvent.click(within(lib as HTMLElement).getByRole('button', { name: 'Release' }));
    fireEvent.click(screen.getByRole('button', { name: 'Release acme/lib' }));
    await waitFor(() => expect(writes(calls)).toHaveLength(3));
    expect(writes(calls).map((c) => [c.method, c.url, c.body])).toEqual([
      ['PUT', '/v1/teams/infra/members/user%3Abob%40example.com', { role: 'admin' }],
      ['DELETE', '/v1/teams/infra/members/user%3Abob%40example.com', undefined],
      ['DELETE', '/v1/teams/infra/repos/acme/lib', undefined],
    ]);
  });

  it('lets a hub admin assign a repository with its history and delete the team', async () => {
    const calls = hub('root@example.com', {}, true, {
      'PUT /v1/teams/infra/repos/acme/new': () => new Response('directory: repository is owned by another team\n', { status: 409 }),
      'DELETE /v1/teams/infra': () => new Response(null, { status: 204 }),
    });
    open('/teams/infra/members');
    fireEvent.change(await screen.findByLabelText('Repository'), { target: { value: 'acme/new' } });
    fireEvent.click(screen.getByLabelText('With its history'));
    fireEvent.click(screen.getByRole('button', { name: 'Assign' }));
    expect(await screen.findByText('Conflict (409): directory: repository is owned by another team')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Delete team' }));
    fireEvent.click(screen.getByRole('button', { name: 'Delete infra' }));
    await waitFor(() => expect(window.location.pathname).toBe('/teams'));
    expect(writes(calls).map((c) => [c.method, c.url, c.body])).toEqual([
      ['PUT', '/v1/teams/infra/repos/acme/new', { history: true }],
      ['DELETE', '/v1/teams/infra', undefined],
    ]);
  });

  it('lets a hub admin create a team; others do not see the form', async () => {
    const calls = hub('root@example.com', {}, true, { 'POST /v1/teams': () => new Response(null, { status: 201 }) });
    open('/teams');
    fireEvent.change(await screen.findByLabelText('Team ID'), { target: { value: 'web' } });
    fireEvent.change(screen.getByLabelText('Name'), { target: { value: 'Web' } });
    fireEvent.click(screen.getByRole('button', { name: 'Create team' }));
    await waitFor(() => expect(writes(calls)).toHaveLength(1));
    expect(writes(calls)[0].body).toEqual({ id: 'web', name: 'Web' });
  });

  it('says a team the caller is not in is not there', async () => {
    stubHub({ 'GET /v1/me': () => me('carol@example.com', {}) });
    open('/teams/infra/members');
    expect(await screen.findByText('No team infra in your teams.')).toBeTruthy();
  });
});

describe('node tokens', () => {
  const token = 'silo-node_ws-1_SECRET-TOKEN-VALUE';
  const rec = { id: 'tok-1', owner: 'bob@example.com', node: 'ws-1', created: '2026-10-16T12:00:00Z' };
  const hub = (extra: Record<string, Handler> = {}) => {
    let minted = false;
    return stubHub({
      'GET /v1/me': () => me('bob@example.com', { infra: 'member' }),
      'GET /v1/nodes': () => Response.json({ nodes: [] }),
      'GET /v1/node-tokens': () => Response.json({ node_tokens: minted ? [rec] : [] }),
      'POST /v1/node-tokens': () => {
        minted = true;
        return Response.json({ token, node_token: rec }, { status: 201, headers: { 'Cache-Control': 'no-store' } });
      },
      ...extra,
    });
  };

  it('shows a minted token once, copies it, and keeps no copy', async () => {
    const calls = hub();
    const writeText = vi.fn(async () => {});
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
    const setItem = vi.spyOn(Storage.prototype, 'setItem');
    open('/nodes');
    fireEvent.change(await screen.findByLabelText('Node ID'), { target: { value: 'ws-1' } });
    fireEvent.click(screen.getByRole('button', { name: 'Mint token' }));
    expect((await screen.findByTestId('minted-token')).textContent).toBe(token);
    expect(writes(calls)[0].init?.cache).toBe('no-store');

    fireEvent.click(screen.getByRole('button', { name: 'Copy' }));
    await screen.findByText('Copied.');
    expect(writeText).toHaveBeenCalledWith(token);

    // The list shows the token's record, never the token.
    await screen.findByText('tok-1');
    fireEvent.click(screen.getByRole('button', { name: 'Done' }));
    expect(screen.queryByText(token, { exact: false })).toBeNull();
    expect(document.body.textContent).not.toContain('SECRET-TOKEN-VALUE');
    expect(setItem).not.toHaveBeenCalled();
    expect(localStorage.length).toBe(0);
    expect(sessionStorage.length).toBe(0);
    expect(document.cookie).toBe('');
  });

  it('revokes a token, and shows the API’s refusal', async () => {
    let refuse = false;
    const calls = hub({
      'GET /v1/node-tokens': () => Response.json({ node_tokens: [rec] }),
      'DELETE /v1/node-tokens/tok-1': () => (refuse ? new Response('no such node token\n', { status: 404 }) : new Response(null, { status: 204 })),
    });
    open('/nodes');
    fireEvent.click(await screen.findByRole('button', { name: 'Revoke' }));
    fireEvent.click(screen.getByRole('button', { name: 'Revoke ws-1’s token' }));
    await waitFor(() => expect(writes(calls)).toHaveLength(1));
    expect([writes(calls)[0].method, writes(calls)[0].url]).toEqual(['DELETE', '/v1/node-tokens/tok-1']);

    refuse = true;
    fireEvent.click(await screen.findByRole('button', { name: 'Revoke' }));
    fireEvent.click(screen.getByRole('button', { name: 'Revoke ws-1’s token' }));
    expect(await screen.findByText('Not found (404): no such node token')).toBeTruthy();
  });

  it('shows the API’s 409 when another owner has the node ID', async () => {
    hub({ 'POST /v1/node-tokens': () => new Response('directory: node belongs to another owner\n', { status: 409 }) });
    open('/nodes');
    fireEvent.change(await screen.findByLabelText('Node ID'), { target: { value: 'ws-1' } });
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Mint token' }));
    });
    expect(await screen.findByText('Conflict (409): directory: node belongs to another owner')).toBeTruthy();
    expect(screen.queryByTestId('minted-token')).toBeNull();
  });
});

describe('hub admin mode toggle', () => {
  const web = {
    id: 'web',
    name: 'Web',
    members: { 'user:bob@example.com': 'admin' },
    repos: ['acme/web'],
  };

  it('starts a hub admin with admin mode off, toggles it on with visual marker and reload, and persists in localStorage', async () => {
    const calls: Call[] = [];
    const f = vi.fn(async (url: string, init?: RequestInit) => {
      const method = init?.method ?? 'GET';
      const hdrs = new Headers(init?.headers);
      const adminHeader = hdrs.get('X-Silo-Admin-Mode');
      const isOn = adminHeader === 'on';
      calls.push({ method, url, init });
      if (url === '/v1/me') {
        return Response.json({
          email: 'root@example.com',
          can_hub_admin: true,
          hub_admin: isOn,
          teams: { infra: 'member' },
        });
      }
      if (url === '/v1/teams') {
        return Response.json({ teams: isOn ? [infra, web] : [infra] });
      }
      return new Response('not found\n', { status: 404 });
    });
    vi.stubGlobal('fetch', f);

    const view = render((window.history.replaceState(null, '', '/teams'), <App />));
    await screen.findByText('Infra (infra)');

    // Starts with Admin mode off: header sent X-Silo-Admin-Mode: off, only own team visible, no marker or Create team form.
    const toggle = await screen.findByLabelText('Admin mode');
    expect((toggle as HTMLInputElement).checked).toBe(false);
    expect(screen.queryByTestId('admin-mode-marker')).toBeNull();
    expect(screen.queryByText(/\(hub admin\)/)).toBeNull();
    expect(screen.queryByText('Web (web)')).toBeNull();
    expect(screen.queryByRole('button', { name: 'Create team' })).toBeNull();
    expect(new Headers(calls[0].init?.headers).get('X-Silo-Admin-Mode')).toBe('off');

    // Toggle Admin mode on.
    fireEvent.click(toggle);
    await screen.findByText('Web (web)');
    expect((screen.getByLabelText('Admin mode') as HTMLInputElement).checked).toBe(true);
    expect(screen.getByTestId('admin-mode-marker')).toBeTruthy();
    expect(screen.getByText(/root@example\.com \(hub admin\)/)).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Create team' })).toBeTruthy();
    expect(localStorage.getItem('silo.adminMode')).toBe('true');
    expect(new Headers(calls[calls.length - 1].init?.headers).get('X-Silo-Admin-Mode')).toBe('on');

    // Remounting reads localStorage and starts with Admin mode on.
    view.unmount();
    render(<App />);
    await screen.findByText('Web (web)');
    expect((screen.getByLabelText('Admin mode') as HTMLInputElement).checked).toBe(true);
    expect(screen.getByTestId('admin-mode-marker')).toBeTruthy();
  });

  it('hides the admin mode switch from non-admins', async () => {
    stubHub({
      'GET /v1/me': () => Response.json({ email: 'alice@example.com', can_hub_admin: false, hub_admin: false, teams: { infra: 'admin' } }),
      'GET /v1/teams': () => Response.json({ teams: [infra] }),
    });
    open('/teams');
    await screen.findByText('Infra (infra)');
    expect(screen.queryByLabelText('Admin mode')).toBeNull();
    expect(screen.queryByTestId('admin-mode-marker')).toBeNull();
  });
});

describe('self-service deployments, canonical URLs, accepted users, and host replacement', () => {
  const payments = {
    id: 'payments-prod',
    name: 'Payments Production',
    url: 'https://hub.example.com/deployments/payments-prod',
    recipient_url: 'https://hub.example.com/deployments/payments-prod',
    members: { 'user:alice@example.com': 'admin' },
    repos: ['acme/payments'],
  };

  it('lets any accepted user create a deployment and opens its canonical deployment page', async () => {
    const calls = stubHub({
      'GET /v1/me': () => me('alice@example.com', { 'payments-prod': 'admin' }, false),
      'GET /v1/deployments': () => Response.json({ deployments: [] }),
      'POST /v1/deployments': (b) =>
        Response.json(
          {
            ...payments,
            id: (b as { id: string }).id,
            name: (b as { name?: string }).name || (b as { id: string }).id,
          },
          { status: 201 },
        ),
      'GET /v1/deployments/payments-prod': () => Response.json(payments),
      'GET /v1/deployments/payments-prod/settings': () => Response.json({ lost_requeue_after_seconds: 0 }),
    });
    open('/deployments');
    fireEvent.change(await screen.findByLabelText('Deployment ID'), { target: { value: 'payments-prod' } });
    fireEvent.change(screen.getByLabelText('Display name (optional)'), { target: { value: 'Payments Production' } });
    fireEvent.click(screen.getByRole('button', { name: 'Create deployment' }));

    await waitFor(() => expect(window.location.pathname).toBe('/deployments/payments-prod'));
    expect((await screen.findByTestId('canonical-deployment-url')).textContent).toBe('https://hub.example.com/deployments/payments-prod');
    expect(writes(calls).map((c) => [c.method, c.url, c.body])).toEqual([
      ['POST', '/v1/deployments', { id: 'payments-prod', name: 'Payments Production' }],
    ]);
  });

  it('lets a deployment admin claim an open repository and replace the host with reclaimed tasks and replayed events', async () => {
    const calls = stubHub({
      'GET /v1/me': () => me('alice@example.com', { 'payments-prod': 'admin' }, false),
      'GET /v1/nodes': () =>
        Response.json({
          nodes: [
            {
              id: 'payments-prod',
              owner: 'alice@example.com',
              status: 'online',
              slots: 2,
              free: 2,
              claimed: 0,
              repos: ['acme/payments'],
              ui: { offered: true, team: 'payments-prod' },
              ui_url: 'https://payments-prod.nodes.hub.example.com/',
              connected_at: '2026-10-18T12:00:00Z',
              last_seen: '2026-10-18T12:01:00Z',
            },
          ],
        }),
      'GET /v1/deployments/payments-prod': () => Response.json(payments),
      'GET /v1/deployments/payments-prod/settings': () => Response.json({ lost_requeue_after_seconds: 0 }),
      'PUT /v1/deployments/payments-prod/repos/acme/checkout': () => new Response(null, { status: 204 }),
      'POST /v1/deployments/payments-prod/replace-host': () =>
        Response.json(
          {
            token: 'silo-node_replace_123',
            node_token: { id: 'tok-rep', owner: 'alice@example.com', node: 'payments-prod', created: '2026-10-18T12:05:00Z' },
            replaced_host: true,
            reclaimed_tasks: ['t-101', 't-102'],
            replayed_events: 3,
          },
          { status: 201 },
        ),
    });

    open('/deployments/payments-prod');
    expect((await screen.findByTestId('canonical-deployment-url')).textContent).toBe('https://hub.example.com/deployments/payments-prod');
    expect((await screen.findByTestId('open-deployment-grain-ui')).getAttribute('href')).toBe('https://payments-prod.nodes.hub.example.com/');

    fireEvent.change(screen.getByLabelText('Repository'), { target: { value: 'acme/checkout' } });
    fireEvent.click(screen.getByRole('button', { name: 'Assign' }));
    await waitFor(() => expect(writes(calls)).toHaveLength(1));

    fireEvent.click(screen.getByRole('button', { name: 'Replace host' }));
    expect((await screen.findByTestId('minted-token')).textContent).toBe('silo-node_replace_123');
    expect((await screen.findByTestId('replace-host-summary')).textContent).toContain('2 in-flight task(s) reclaimed (t-101, t-102)');
    expect((await screen.findByTestId('replace-host-summary')).textContent).toContain('3 Grainfile event(s) queued for replay');
  });

  it('lets a sys admin manage accepted users and shows 409 when demoting the last sys admin', async () => {
    const calls = stubHub({
      'GET /v1/me': () => me('root@example.com', {}, true),
      'GET /v1/deployments': () => Response.json({ deployments: [payments] }),
      'GET /v1/users': () =>
        Response.json({
          users: [{ email: 'root@example.com', sys_admin: true, added_by: 'bootstrap', created_at: '2026-10-18T12:00:00Z' }],
        }),
      'PUT /v1/users/dev%40example.com': () =>
        Response.json({ email: 'dev@example.com', sys_admin: false, added_by: 'root@example.com', created_at: '2026-10-18T12:05:00Z' }),
      'PUT /v1/users/root%40example.com': () =>
        new Response('directory: cannot remove or demote the last sys admin\n', { status: 409 }),
    });

    open('/deployments');
    await screen.findByTestId('accepted-users-section');
    fireEvent.change(screen.getByLabelText('User email'), { target: { value: 'dev@example.com' } });
    fireEvent.click(screen.getByRole('button', { name: 'Add accepted user' }));
    await waitFor(() => expect(writes(calls)).toHaveLength(1));
    expect(writes(calls)[0]).toMatchObject({
      method: 'PUT',
      url: '/v1/users/dev%40example.com',
      body: { sys_admin: false },
    });

    fireEvent.click(screen.getByLabelText('Sys admin for root@example.com'));
    expect(await screen.findByText('Conflict (409): directory: cannot remove or demote the last sys admin')).toBeTruthy();
  });
});
