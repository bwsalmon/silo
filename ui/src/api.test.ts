import { describe, expect, it, vi } from 'vitest';
import { ApiError, fetchArtifact, getTask, listTasks } from './api';

function stubFetch(status: number, body: string) {
  const f = vi.fn(async (_url: string, _init?: RequestInit) => new Response(body, { status }));
  vi.stubGlobal('fetch', f);
  return f;
}

describe('listTasks', () => {
  it('asks for one team and states', async () => {
    const f = stubFetch(200, JSON.stringify({ tasks: [{ id: 't-1' }] }));
    const ts = await listTasks({ team: 'infra', states: ['queued', 'running'] });
    expect(ts).toEqual([{ id: 't-1' }]);
    expect(f.mock.calls[0][0]).toBe('/v1/tasks?team=infra&state=queued%2Crunning');
    expect(f.mock.calls[0][1]?.credentials).toBe('same-origin');
  });

  it('asks for every team the caller is in when none is named', async () => {
    const f = stubFetch(200, JSON.stringify({ tasks: [] }));
    await listTasks({});
    expect(f.mock.calls[0][0]).toBe('/v1/tasks');
  });
});

describe('getTask', () => {
  it('reports the API status of a refusal', async () => {
    stubFetch(404, 'no such task\n');
    const err = await getTask('t-9').catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(404);
    expect((err as ApiError).message).toBe('no such task');
  });
});

describe('fetchArtifact', () => {
  it('reads the view-through uncached', async () => {
    const f = stubFetch(200, 'diff --git a/x b/x\n');
    const r = await fetchArtifact('t 1', 'diff/1');
    expect(r).toEqual({ kind: 'text', text: 'diff --git a/x b/x\n' });
    expect(f.mock.calls[0][0]).toBe('/v1/tasks/t%201/artifacts?ref=diff%2F1');
    expect(f.mock.calls[0][1]?.cache).toBe('no-store');
    expect(f.mock.calls[0][1]?.credentials).toBe('same-origin');
  });

  it('says an offline node’s artifact is unavailable, with no copy', async () => {
    stubFetch(503, 'unavailable: node ws-1, which holds it, is offline\n');
    const r = await fetchArtifact('t-1', 'diff-1');
    expect(r.kind).toBe('unavailable');
    expect(r).not.toHaveProperty('text');
  });

  it('passes other refusals on', async () => {
    stubFetch(404, 'no such artifact');
    expect(await fetchArtifact('t-1', 'x')).toEqual({ kind: 'error', status: 404, message: 'no such artifact' });
  });
});
