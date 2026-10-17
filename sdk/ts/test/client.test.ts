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

  it('mint a node token uncached', async () => {
    const f = stub(201, JSON.stringify({ token: 'silo-node_x', node_token: { id: 'k' } }));
    expect((await new HubClient({ fetch: f }).mintNodeToken('ws-1')).token).toBe('silo-node_x');
    expect(f.mock.calls[0][1]?.cache).toBe('no-store');
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
