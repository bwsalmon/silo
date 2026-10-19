import { describe, expect, it, vi } from 'vitest';
import { ApiError, ConflictError, ForbiddenError, HubClient, NotFoundError, UnauthenticatedError, UnavailableError, errorFor } from '../src/index.js';

function stub(status: number, body: string) {
  return vi.fn(async (_url: string, _init?: RequestInit) => new Response(status === 204 ? null : body, { status }));
}

const headers = (init?: RequestInit) => new Headers(init?.headers);

describe('HubClient in a browser, on the hub’s origin', () => {
  it('sends same-origin requests with the browser’s own credentials and no token', async () => {
    const f = stub(200, JSON.stringify({ tasks: [{ id: 't-1' }] }));
    vi.stubGlobal('fetch', f);
    try {
      const ts = await new HubClient().listTasks({ team: 'infra', states: ['queued', 'running'], limit: 5 });
      expect(ts).toEqual([{ id: 't-1' }]);
      expect(f.mock.calls[0][0]).toBe('/v1/tasks?team=infra&state=queued%2Crunning&limit=5');
      expect(f.mock.calls[0][1]?.credentials).toBe('same-origin');
      expect(headers(f.mock.calls[0][1]).get('Authorization')).toBeNull();
    } finally {
      vi.unstubAllGlobals();
    }
  });
});

describe('HubClient with a token, as in Node', () => {
  it('sends the token, a fresh one each time, to baseURL', async () => {
    const f = stub(200, JSON.stringify({ email: 'a@example.com', hub_admin: false, teams: null }));
    let n = 0;
    const h = new HubClient({ baseURL: 'https://hub.example.com/', token: async () => `tok-${++n}`, headers: { 'X-Extra': '1' }, fetch: f });
    await h.getMe();
    await h.getMe();
    expect(f.mock.calls[0][0]).toBe('https://hub.example.com/v1/me');
    expect(headers(f.mock.calls[0][1]).get('Authorization')).toBe('Bearer tok-1');
    expect(headers(f.mock.calls[1][1]).get('Authorization')).toBe('Bearer tok-2');
    expect(headers(f.mock.calls[0][1]).get('X-Extra')).toBe('1');
  });

  it('puts the token in another header when asked', async () => {
    const f = stub(200, '{}');
    await new HubClient({ token: 't', tokenHeader: 'Proxy-Authorization', fetch: f }).getTask('x');
    expect(headers(f.mock.calls[0][1]).get('Proxy-Authorization')).toBe('Bearer t');
    expect(headers(f.mock.calls[0][1]).get('Authorization')).toBeNull();
  });

  it('toggles X-Silo-Admin-Mode via adminMode option and setAdminMode', async () => {
    const f = stub(200, JSON.stringify({ email: 'root@example.com', hub_admin: false, can_hub_admin: true, teams: { infra: 'admin' } }));
    const h = new HubClient({ fetch: f });
    expect(h.getAdminMode()).toBe(false);
    await h.getMe();
    expect(headers(f.mock.calls[0][1]).get('X-Silo-Admin-Mode')).toBeNull();

    h.setAdminMode(false);
    expect(h.getAdminMode()).toBe(false);
    const meOff = await h.getMe();
    expect(meOff.can_hub_admin).toBe(true);
    expect(meOff.hub_admin).toBe(false);
    expect(headers(f.mock.calls[1][1]).get('X-Silo-Admin-Mode')).toBe('off');

    h.setAdminMode(true);
    expect(h.getAdminMode()).toBe(true);
    await h.listTeams();
    expect(headers(f.mock.calls[2][1]).get('X-Silo-Admin-Mode')).toBe('on');

    const hInitOn = new HubClient({ adminMode: true, fetch: f });
    expect(hInitOn.getAdminMode()).toBe(true);
    await hInitOn.getMe();
    expect(headers(f.mock.calls[3][1]).get('X-Silo-Admin-Mode')).toBe('on');
  });
});

describe('writes', () => {
  it('send JSON, and no Content-Type without a body', async () => {
    const f = stub(200, JSON.stringify({ id: 't-1' }));
    const h = new HubClient({ fetch: f });
    expect(await h.submitTask({ team: 'infra', title: 't', prompt: 'p' })).toEqual({ id: 't-1' });
    expect(f.mock.calls[0][1]?.method).toBe('POST');
    expect(f.mock.calls[0][1]?.body).toBe(JSON.stringify({ team: 'infra', title: 't', prompt: 'p' }));
    expect(headers(f.mock.calls[0][1]).get('Content-Type')).toBe('application/json');
    await h.requeueTask('t 1');
    expect(f.mock.calls[1][0]).toBe('/v1/tasks/t%201/requeue');
    expect(headers(f.mock.calls[1][1]).get('Content-Type')).toBeNull();
  });

  it('answer undefined for an empty body', async () => {
    expect(await new HubClient({ fetch: stub(204, '') }).deleteTeam('infra')).toBeUndefined();
  });

  it('keep the slash in a repository', async () => {
    const f = stub(200, '');
    await new HubClient({ fetch: f }).assignRepo('infra', 'acme/app', true);
    expect(f.mock.calls[0][0]).toBe('/v1/teams/infra/repos/acme/app');
    expect(f.mock.calls[0][1]?.body).toBe('{"history":true}');
  });

  it('refuses . and .. path segments', async () => {
    const f = stub(200, '{}');
    const h = new HubClient({ fetch: f });
    await expect(async () => h.getTask('..')).rejects.toThrow(/invalid path segment/);
    await expect(async () => h.getTeam('.')).rejects.toThrow(/invalid path segment/);
    await expect(async () => h.assignRepo('infra', 'acme/..')).rejects.toThrow(/invalid path segment/);
    expect(f).not.toHaveBeenCalled();
  });

  it('mint a node token uncached', async () => {
    const f = stub(201, JSON.stringify({ token: 'silo-node_x', node_token: { id: 'k' } }));
    expect((await new HubClient({ fetch: f }).mintNodeToken('ws-1')).token).toBe('silo-node_x');
    expect(f.mock.calls[0][1]?.cache).toBe('no-store');
  });

  it('sets proposer and viewer roles and team submission policies', async () => {
    const f = stub(204, '');
    const h = new HubClient({ fetch: f });
    await h.setMember('infra', 'user:prop@example.com', 'proposer');
    await h.setMember('infra', 'user:view@example.com', 'viewer');
    await h.setTeamSettings('infra', {
      lost_requeue_after_seconds: 60,
      anyone_may_propose: false,
      require_approval: true,
    });
    expect(f.mock.calls[0][1]?.body).toBe(JSON.stringify({ role: 'proposer' }));
    expect(f.mock.calls[1][1]?.body).toBe(JSON.stringify({ role: 'viewer' }));
    expect(f.mock.calls[2][1]?.body).toBe(
      JSON.stringify({
        lost_requeue_after_seconds: 60,
        anyone_may_propose: false,
        require_approval: true,
      }),
    );
  });

  it('syncs resolved theme and queries/updates unified inbox preferences', async () => {
    const f = vi.fn(async (url: string, init?: RequestInit) => {
      if (url === '/v1/me/theme') {
        return new Response(JSON.stringify({ theme: 'dark' }), { status: 200 });
      }
      if (url === '/v1/me/inbox' && init?.method === 'PUT') {
        return new Response(JSON.stringify({ nodes: ['ws-1', 'ws-2'], configured: true }), { status: 200 });
      }
      if (url === '/v1/me/inbox') {
        return new Response(JSON.stringify({ nodes: ['ws-1'], configured: false }), { status: 200 });
      }
      return new Response(
        JSON.stringify({
          items: [
            {
              id: 't-1:proposal',
              kind: 'proposal',
              task_id: 't-1',
              node_id: 'ws-1',
              team: 'infra',
              title: 'Fix typo',
              state: 'proposed',
              updated_at: '2026-10-18T12:00:00Z',
            },
          ],
          nodes: ['ws-1'],
        }),
        { status: 200 },
      );
    });
    const h = new HubClient({ fetch: f });
    expect(await h.setTheme('dark')).toEqual({ theme: 'dark' });
    expect(await h.getInboxPreferences()).toEqual({ nodes: ['ws-1'], configured: false });
    expect(await h.setInboxPreferences(['ws-1', 'ws-2'])).toEqual({ nodes: ['ws-1', 'ws-2'], configured: true });
    const items = await h.listInbox({ nodes: ['ws-1'], kind: 'proposal' });
    expect(items).toHaveLength(1);
    expect(items[0].kind).toBe('proposal');
    expect(f.mock.calls[3][0]).toBe('/v1/inbox?nodes=ws-1&kind=proposal');
  });
});

describe('errors', () => {
  it.each([
    [401, UnauthenticatedError],
    [403, ForbiddenError],
    [404, NotFoundError],
    [409, ConflictError],
    [503, UnavailableError],
  ])('%i is its own ApiError', async (status, cls) => {
    const err = await new HubClient({ fetch: stub(status, 'the API’s words\n') }).getTask('t-1').catch((e: unknown) => e);
    expect(err).toBeInstanceOf(cls);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(status);
    expect((err as ApiError).message).toBe('the API’s words');
  });

  it('keeps any other status', () => {
    const e = errorFor(418, 'teapot');
    expect(e.constructor).toBe(ApiError);
    expect(e.status).toBe(418);
  });
});

describe('fetchArtifact', () => {
  it('reads the view-through uncached', async () => {
    const f = stub(200, 'diff --git a/x b/x\n');
    const r = await new HubClient({ fetch: f, token: 't' }).fetchArtifact('t 1', 'diff/1');
    expect(r).toEqual({ kind: 'text', text: 'diff --git a/x b/x\n' });
    expect(f.mock.calls[0][0]).toBe('/v1/tasks/t%201/artifacts?ref=diff%2F1');
    expect(f.mock.calls[0][1]?.cache).toBe('no-store');
    expect(headers(f.mock.calls[0][1]).get('Authorization')).toBe('Bearer t');
  });

  it('says an offline node’s artifact is unavailable, with no copy', async () => {
    const r = await new HubClient({ fetch: stub(503, 'unavailable: node ws-1 is offline\n') }).fetchArtifact('t-1', 'diff-1');
    expect(r).toEqual({ kind: 'unavailable', message: 'unavailable: node ws-1 is offline' });
  });

  it('passes other refusals on', async () => {
    expect(await new HubClient({ fetch: stub(404, 'no such artifact') }).fetchArtifact('t-1', 'x')).toEqual({
      kind: 'error',
      status: 404,
      message: 'no such artifact',
    });
  });
});

describe('Grainfiles and node subscriptions', () => {
  const sampleOverview = {
    grainfiles: [
      {
        repo: 'https://github.com/acme/app',
        path: 'tasks/01-deploy.task.md',
        commit_sha: '1111111',
        ref: 'main',
        url: 'https://github.com/acme/app/blob/main/tasks/01-deploy.task.md',
        kind: 'task',
        title: 'Deploy staging service',
        from_template: 'https://github.com/acme/app/blob/main/.grain/templates/deploy.task.grain.md.jinja2',
        recipient: 'https://github.com/acme/grain-eu',
        requestor: 'Alice <alice@example.com>',
        draft: false,
        not_before: '2026-10-19T00:00:00Z',
        created_at: '2026-10-18T10:00:00Z',
        references: ['/docs/arch.md'],
        authorized_references: ['https://github.com/acme/app/blob/main/docs/arch.md'],
        depends_on: ['https://github.com/acme/app/blob/main/tasks/00-prep.task.md'],
        updated_at: '2026-10-18T10:05:00Z',
      },
      {
        repo: 'https://github.com/acme/app',
        path: '.grain/processes/ci.process.md.jinja2',
        commit_sha: '1111111',
        ref: 'main',
        url: 'https://github.com/acme/app/blob/main/.grain/processes/ci.process.md.jinja2',
        kind: 'process',
        title: 'Continuous verification gate',
        is_template: true,
        template_vars: ['branch', 'env'],
        draft: true,
        not_before: '0001-01-01T00:00:00Z',
        created_at: '0001-01-01T00:00:00Z',
        references: ['https://github.com/acme/app/blob/main/.grain/processes/smoke.process.md'],
        on_spec: { push: { repo: 'acme/app', branches: ['main'] }, schedule: { cron: '0 * * * *' } },
        gate: true,
        updated_at: '2026-10-18T10:06:00Z',
      },
    ],
    subscriptions: [
      {
        node_id: 'ws-eu-1',
        owner: 'alice@example.com',
        recipients: ['https://github.com/acme/grain-eu'],
        repos: ['https://github.com/acme/app'],
        folders: ['https://github.com/acme/app/tree/main/.grain/processes'],
        grainfiles: ['https://github.com/acme/app/blob/main/tasks/01-deploy.task.md'],
        cursor: 4,
        revision: 2,
        updated_at: '2026-10-18T10:07:00Z',
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
        title: 'Deploy staging service',
        from_template: 'https://github.com/acme/app/blob/main/.grain/templates/deploy.task.grain.md.jinja2',
        recipient: 'https://github.com/acme/grain-eu',
        created_at: '2026-10-18T10:05:00Z',
      },
      {
        seq: 2,
        type: 'process_triggered',
        repo: 'https://github.com/acme/app',
        path: '.grain/processes/ci.process.md.jinja2',
        grainfile_url: 'https://github.com/acme/app/blob/main/.grain/processes/ci.process.md.jinja2',
        kind: 'process',
        title: 'Continuous verification gate',
        is_template: true,
        template_vars: ['branch', 'env'],
        recipient: 'https://github.com/acme/grain-eu',
        trigger_source: 'push',
        gate: true,
        on_spec: { push: { branches: ['main'] } },
        created_at: '2026-10-18T10:06:00Z',
      },
    ],
  };

  it('lists grainfiles filtered by kind, recipient, repo, and draft', async () => {
    const f = stub(200, JSON.stringify(sampleOverview));
    const h = new HubClient({ fetch: f });
    const tasks = await h.listGrainfiles({
      kind: 'task',
      recipient: 'https://github.com/acme/grain-eu',
      repo: 'acme/app',
      draft: false,
    });
    expect(f.mock.calls[0][0]).toBe(
      '/v1/grainfiles?kind=task&recipient=https%3A%2F%2Fgithub.com%2Facme%2Fgrain-eu&repo=acme%2Fapp&draft=false',
    );
    expect(tasks).toHaveLength(1);
    expect(tasks[0].title).toBe('Deploy staging service');
    expect(tasks[0].from_template).toBe('https://github.com/acme/app/blob/main/.grain/templates/deploy.task.grain.md.jinja2');
    expect(tasks[0].requestor).toBe('Alice <alice@example.com>');
    expect(tasks[0].depends_on).toEqual(['https://github.com/acme/app/blob/main/tasks/00-prep.task.md']);
  });

  it('inspects a grainfile’s metadata, normalized on triggers, and recent events', async () => {
    const f = stub(200, JSON.stringify(sampleOverview));
    const h = new HubClient({ fetch: f });
    const insp = await h.inspectGrainfile('https://github.com/acme/app/blob/main/.grain/processes/ci.process.md.jinja2');
    expect(insp.grainfile?.title).toBe('Continuous verification gate');
    expect(insp.grainfile?.grainfile).toBe('v1');
    expect(insp.grainfile?.spec_summary_path).toBe('docs/grainfile-v1.md');
    expect(insp.grainfile?.spec_path).toBe('docs/grainfiles-spec.md');
    expect(insp.grainfile?.is_template).toBe(true);
    expect(insp.grainfile?.template_vars).toEqual(['branch', 'env']);
    expect(insp.grainfile?.gate).toBe(true);
    expect(!Array.isArray(insp.grainfile?.when?.push) && insp.grainfile?.when?.push?.repo).toBe('acme/app');
    expect(!Array.isArray(insp.grainfile?.when?.push) && insp.grainfile?.when?.push?.branches).toEqual(['main']);
    expect(!Array.isArray(insp.grainfile?.on?.push) && insp.grainfile?.on?.push?.branches).toEqual(['main']);
    expect(insp.grainfile?.not_before).toBeUndefined();
    expect(insp.grainfile?.references).toEqual([
      'https://github.com/acme/app/blob/main/.grain/processes/smoke.process.md',
    ]);
    expect(insp.events).toHaveLength(1);
    expect(insp.events[0].type).toBe('process_triggered');
    expect(insp.events[0].trigger_source).toBe('push');
    expect(insp.events[0].is_template).toBe(true);
    expect(insp.events[0].template_vars).toEqual(['branch', 'env']);
    expect(insp.subscriptions).toHaveLength(1);
    expect(insp.subscriptions[0].node_id).toBe('ws-eu-1');
  });

  it('reads node recipient identities, subscriptions, and nodeui URLs on GET /v1/nodes', async () => {
    const f = stub(
      200,
      JSON.stringify({
        nodes: [
          {
            id: 'ws-eu-1',
            repos: ['acme/app'],
            slots: 2,
            owner: 'alice@example.com',
            status: 'online',
            free: 1,
            claimed: 0,
            connected_at: '2026-10-18T10:00:00Z',
            last_seen: '2026-10-18T10:08:00Z',
            features: ['grainfiles', 'nodeui'],
            recipients: ['https://github.com/acme/grain-eu'],
            folders: ['https://github.com/acme/app/tree/main/.grain/processes'],
            grainfiles: ['https://github.com/acme/app/blob/main/tasks/01-deploy.task.md'],
            ui: { offered: true, url: 'https://ws-eu-1.nodes.hub.example.com/' },
            ui_url: 'https://ws-eu-1.nodes.hub.example.com/',
          },
        ],
      }),
    );
    const nodes = await new HubClient({ fetch: f }).listNodes();
    expect(nodes[0].recipients).toEqual(['https://github.com/acme/grain-eu']);
    expect(nodes[0].folders).toEqual(['https://github.com/acme/app/tree/main/.grain/processes']);
    expect(nodes[0].grainfiles).toEqual(['https://github.com/acme/app/blob/main/tasks/01-deploy.task.md']);
    expect(nodes[0].ui?.offered).toBe(true);
    expect(nodes[0].ui_url).toBe('https://ws-eu-1.nodes.hub.example.com/');
  });
});
