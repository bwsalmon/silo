// The SDK against a real `silo hub serve` (this tree's, built by
// setup.ts) and a scripted fake Grain (internal/fakegrain) on the real
// rendezvous: the README's examples, run in Node. Sign-on is the hub's
// development user header, sent with the client's headers option as a
// bearer token would be with token.
import { spawn, type ChildProcess } from 'node:child_process';
import { createWriteStream, existsSync, mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { ConflictError, HubClient, NotFoundError, type HubEvent, type Task } from '../../src/index.js';
import { binDir } from './paths.js';

const userHeader = 'X-Silo-Sdk-User';
const admin = 'admin@sdk.test';
const alice = 'alice@sdk.test';
const outsider = 'carol@sdk.test';
const team = 'sdk';
const repo = 'sdk/app';
const node = 'ws-sdk';

const procs: ChildProcess[] = [];
let dir: string;
let url: string;
let grain: string;

const as = (user: string) => new HubClient({ baseURL: url, headers: { [userHeader]: user } });

async function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const s = createServer();
    s.listen(0, '127.0.0.1', () => {
      const a = s.address();
      s.close(() => (a && typeof a === 'object' ? resolve(a.port) : reject(new Error('no port'))));
    });
  });
}

async function until<T>(what: string, f: () => Promise<T | undefined>, timeout = 15_000): Promise<T> {
  const deadline = Date.now() + timeout;
  let last: unknown;
  while (Date.now() < deadline) {
    try {
      const v = await f();
      if (v !== undefined) return v;
    } catch (e) {
      last = e;
    }
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(`timed out waiting for ${what}${last ? `: ${last}` : ''}`);
}

function start(name: string, args: string[]): ChildProcess {
  const log = createWriteStream(join(dir, `${name}.log`));
  const p = spawn(`${binDir}/${name}`, args, { stdio: ['ignore', 'pipe', 'pipe'] });
  p.stdout?.pipe(log);
  p.stderr?.pipe(log);
  procs.push(p);
  return p;
}

// control drives the fake Grain.
async function control(method: string, path: string, body?: unknown): Promise<any> {
  const r = await fetch(grain + path, { method, body: body === undefined ? undefined : typeof body === 'string' ? body : JSON.stringify(body) });
  const text = await r.text();
  if (!r.ok) throw new Error(`fake grain: ${method} ${path}: ${r.status} ${text}`);
  return text ? JSON.parse(text) : undefined;
}

const emit = (task: string, ...events: Record<string, unknown>[]) => control('POST', `/tasks/${task}/emit`, events);
const status = (message: string) => ({ type: 'status', status: { phase: 'running', state: 'info', message } });

beforeAll(async () => {
  dir = mkdtempSync(join(tmpdir(), 'silo-sdk-'));
  const port = await freePort();
  url = `http://127.0.0.1:${port}`;
  const hub = start('silo', [
    'hub', 'serve', '-addr', `127.0.0.1:${port}`, '-database', `sqlite:${join(dir, 'hub.db')}`,
    '-dev-user-header', userHeader, '-admins', `user:${admin}`, '-heartbeat', '1s', '-dispatch-retry', '1s', '-shutdown-grace', '2s',
  ]);
  await until('the hub to answer /healthz', async () => {
    if (hub.exitCode !== null) throw new Error(`the hub exited ${hub.exitCode}; see ${join(dir, 'silo.log')}`);
    return (await fetch(url + '/healthz')).ok || undefined;
  });

  // The directory, through the SDK as a hub admin.
  const a = as(admin);
  await a.createTeam(team, 'SDK');
  await a.setMember(team, `user:${alice}`, 'admin');
  await a.assignRepo(team, repo);

  // A fake Grain with a token alice mints through the SDK.
  const { token } = await as(alice).mintNodeToken(node);
  writeFileSync(join(dir, 'token'), token, { mode: 0o600 });
  start('fakegrain', ['-hub', url, '-token-file', join(dir, 'token'), '-id', node, '-repos', repo, '-addr-file', join(dir, 'addr')]);
  grain = 'http://' + (await until('the fake grain to listen', async () => (existsSync(join(dir, 'addr')) ? readFileSync(join(dir, 'addr'), 'utf8') : undefined)));
  await until('the fake grain to be online', async () => ((await control('GET', '/state')).online ? true : undefined));
}, 60_000);

afterAll(() => {
  for (const p of procs) p.kill('SIGTERM');
});

describe('the SDK against silo hub serve', () => {
  let task: Task;

  it('files a task and lists it in its team’s backlog', async () => {
    const h = as(alice);
    task = await h.submitTask({ repo, title: 'Rename the greeting', prompt: 'Rename it.' });
    expect(task.team).toBe(team);
    const backlog = await h.listTasks({ team });
    expect(backlog.map((t) => t.id)).toContain(task.id);
    await until('the task to reach the node', async () => ((await control('GET', '/state')).tasks.some((t: { id: string }) => t.id === task.id) ? true : undefined));
    expect((await h.listNodes()).map((n) => n.id)).toContain(node);
  });

  it('refuses as the API does', async () => {
    await expect(as(outsider).getTask(task.id)).rejects.toBeInstanceOf(NotFoundError);
  });

  it('follows the task’s events, and resumes from its cursor', async () => {
    const h = as(alice);
    await emit(task.id, status('step 1'), status('step 2'));

    // Follow until both are in, then close.
    const first: HubEvent[] = [];
    const s = h.follow(task.id, { onEvent: (e) => first.push(e) });
    await until('two events', async () => (first.length >= 2 ? true : undefined));
    s.close();
    expect(first.map((e) => e.status?.message)).toEqual(['step 1', 'step 2']);

    // What happens while no one is following is there from the cursor.
    await emit(task.id, status('step 3'));
    await control('PUT', '/artifacts/diff-1', 'diff --git a/greeting b/greeting\n');
    await emit(task.id, { type: 'artifact', artifact: { kind: 'diff', ref: 'diff-1' } });
    await emit(task.id, { type: 'result', result: { exit: { code: 0, reason: 'completed' }, final_text: 'Renamed.' } });
    await control('POST', `/tasks/${task.id}/finish`, { state: 'done' });

    const rest: HubEvent[] = [];
    for await (const e of h.events(task.id, { from: s.cursor })) rest.push(e);
    expect(rest.map((e) => e.type)).toEqual(['status', 'artifact', 'result']);
    expect(rest[0].seq).toBeGreaterThan(s.cursor);
    expect(rest[1].artifact).toEqual({ kind: 'diff', ref: 'diff-1' });
    expect((await h.getTask(task.id)).state).toBe('done');
  });

  it('fetches an artifact through the hub without keeping it', async () => {
    const r = await as(alice).fetchArtifact(task.id, 'diff-1');
    expect(r).toEqual({ kind: 'text', text: 'diff --git a/greeting b/greeting\n' });
  });

  it('answers a write the task’s state does not allow with a ConflictError', async () => {
    await expect(as(alice).cancelTask(task.id)).rejects.toBeInstanceOf(ConflictError);
  });
});
