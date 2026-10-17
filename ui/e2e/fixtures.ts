// The suite's fixtures: a hub per worker, and per test a world (a team
// of its own, with an admin, a member and a repository), browser pages
// signed in as anyone, and fake Grains.
//
// Sign-on is the hub's development user header (-dev-user-header),
// which `silo hub serve` refuses on Kubernetes (server.CheckDevSignOn):
// each signed-in page is a browser context that sends the header on
// every request, its EventSource streams included. The hub admin seeds
// the directory through the same API the UI uses.
import { test as base, expect, type Browser, type Page } from '@playwright/test';
import { spawn, type ChildProcess } from 'node:child_process';
import { mkdtempSync, readFileSync, writeFileSync, existsSync, createWriteStream } from 'node:fs';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { binDir } from './paths';

export const userHeader = 'X-Silo-E2e-User';
export const hubAdmin = 'admin@e2e.test';

export interface Reply<T = any> {
  status: number;
  body: T;
}

export class Hub {
  constructor(
    readonly url: string,
    readonly proc: ChildProcess,
  ) {}

  // api calls the hub's API as user, with the hub's own Origin, which
  // it requires of a write (identity.SameOrigin).
  async api<T = any>(user: string, method: string, path: string, body?: unknown): Promise<Reply<T>> {
    const r = await fetch(this.url + path, {
      method,
      headers: { [userHeader]: user, Origin: this.url, 'Content-Type': 'application/json', Accept: 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const text = await r.text();
    let parsed: any = text;
    try {
      parsed = text ? JSON.parse(text) : undefined;
    } catch {
      // Not JSON: an error's text.
    }
    return { status: r.status, body: parsed };
  }

  // must is api, failing the test on anything but a 2xx.
  async must<T = any>(user: string, method: string, path: string, body?: unknown): Promise<T> {
    const r = await this.api<T>(user, method, path, body);
    if (r.status < 200 || r.status > 299) throw new Error(`${method} ${path} as ${user}: ${r.status} ${JSON.stringify(r.body)}`);
    return r.body;
  }
}

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
      if (v !== undefined && v !== false) return v;
    } catch (e) {
      last = e;
    }
    await new Promise((r) => setTimeout(r, 100));
  }
  throw new Error(`timed out waiting for ${what}${last ? `: ${last}` : ''}`);
}

async function startHub(dir: string): Promise<Hub> {
  const port = await freePort();
  const url = `http://127.0.0.1:${port}`;
  const database = process.env.SILO_E2E_DATABASE || `sqlite:${join(dir, 'hub.db')}`;
  const log = createWriteStream(join(dir, 'hub.log'));
  const proc = spawn(
    `${binDir}/silo`,
    ['hub', 'serve', '-addr', `127.0.0.1:${port}`, '-database', database, '-dev-user-header', userHeader, '-admins', `user:${hubAdmin}`,
      '-heartbeat', '1s', '-dispatch-retry', '1s', '-shutdown-grace', '2s'],
    { stdio: ['ignore', 'pipe', 'pipe'] },
  );
  proc.stdout?.pipe(log);
  proc.stderr?.pipe(log);
  await until('the hub to answer /healthz', async () => {
    if (proc.exitCode !== null) throw new Error(`the hub exited ${proc.exitCode}; see ${join(dir, 'hub.log')}`);
    return (await fetch(url + '/healthz')).ok || undefined;
  });
  return new Hub(url, proc);
}

// A Grain is a fake Grain node (internal/fakegrain), driven through its
// control API.
export class Grain {
  constructor(
    readonly id: string,
    readonly control: string,
    readonly proc: ChildProcess,
  ) {}

  private async call(method: string, path: string, body?: unknown): Promise<any> {
    const r = await fetch(this.control + path, {
      method,
      body: body === undefined ? undefined : typeof body === 'string' ? body : JSON.stringify(body),
    });
    const text = await r.text();
    if (!r.ok) throw new Error(`fake grain ${this.id}: ${method} ${path}: ${r.status} ${text}`);
    return text ? JSON.parse(text) : undefined;
  }

  state(): Promise<GrainState> {
    return this.call('GET', '/state');
  }
  // emit sends events for task up, numbered by the node.
  emit(task: string, ...events: Record<string, unknown>[]): Promise<void> {
    return this.call('POST', `/tasks/${task}/emit`, events);
  }
  finish(task: string, state: 'done' | 'failed', reason?: string): Promise<void> {
    return this.call('POST', `/tasks/${task}/finish`, { state, reason });
  }
  artifact(ref: string, content: string): Promise<void> {
    return this.call('PUT', `/artifacts/${encodeURIComponent(ref)}`, content);
  }
  offline(): Promise<void> {
    return this.call('POST', '/offline');
  }
  online(): Promise<void> {
    return this.call('POST', '/online');
  }
  // task waits for the hub to have placed task on this node.
  task(id: string): Promise<GrainTask> {
    return until(`${id} on ${this.id}`, async () => (await this.state()).tasks.find((t) => t.id === id));
  }
  // received waits for the hub to have written a message of type down
  // task's stream, and returns it.
  received(task: string, type: string): Promise<any> {
    return until(`a ${type} for ${task} at ${this.id}`, async () =>
      (await this.state()).tasks.find((t) => t.id === task)?.received.find((m: any) => m.type === type),
    );
  }
  waitFor(what: string, f: (s: GrainState) => boolean): Promise<GrainState> {
    return until(what, async () => {
      const s = await this.state();
      return f(s) ? s : undefined;
    });
  }
}

export interface GrainTask {
  id: string;
  node_task: string;
  received: any[];
}

export interface GrainState {
  online: boolean;
  refused: boolean;
  connects: number;
  tasks: GrainTask[];
}

export interface GrainOptions {
  owner: string;
  repos: string[];
  id?: string;
  interactive?: boolean;
  // token, when set, is the node token to connect with; otherwise the
  // owner mints one through the API.
  token?: string;
}

// World is a test's own corner of the hub: team (with admin, a team
// admin, and member, a member), owning repo; and outsider, signed in
// but in no team of it.
export interface World {
  n: string;
  team: string;
  repo: string;
  admin: string;
  member: string;
  outsider: string;
}

let counter = 0;

export const test = base.extend<
  {
    world: World;
    newWorld: () => Promise<World>;
    pageAs: (user: string) => Promise<Page>;
    grain: (o: GrainOptions) => Promise<Grain>;
    fileTask: (user: string, body: Record<string, unknown>) => Promise<any>;
  },
  { hub: Hub; workDir: string }
>({
  workDir: [
    async ({}, use) => {
      await use(mkdtempSync(join(tmpdir(), 'silo-e2e-')));
    },
    { scope: 'worker' },
  ],
  hub: [
    async ({ workDir }, use) => {
      const hub = await startHub(workDir);
      await use(hub);
      hub.proc.kill('SIGTERM');
    },
    { scope: 'worker' },
  ],
  newWorld: async ({ hub }, use, info) => {
    await use(async () => {
      const n = `${info.workerIndex}-${process.pid}-${++counter}`;
      const w: World = {
        n,
        team: `team-${n}`,
        repo: `e2e/repo-${n}`,
        admin: `alice-${n}@e2e.test`,
        member: `bob-${n}@e2e.test`,
        outsider: `carol-${n}@e2e.test`,
      };
      await hub.must(hubAdmin, 'POST', '/v1/teams', { id: w.team, name: `Team ${n}` });
      await hub.must(hubAdmin, 'PUT', `/v1/teams/${w.team}/members/user:${w.admin}`, { role: 'admin' });
      await hub.must(hubAdmin, 'PUT', `/v1/teams/${w.team}/members/user:${w.member}`, { role: 'member' });
      await hub.must(hubAdmin, 'PUT', `/v1/teams/${w.team}/repos/${w.repo}`);
      return w;
    });
  },
  world: async ({ newWorld }, use) => {
    await use(await newWorld());
  },
  pageAs: async ({ browser, hub }, use) => {
    const contexts: Awaited<ReturnType<Browser['newContext']>>[] = [];
    await use(async (user: string) => {
      const ctx = await browser.newContext({ baseURL: hub.url, extraHTTPHeaders: { [userHeader]: user } });
      contexts.push(ctx);
      return ctx.newPage();
    });
    for (const c of contexts) await c.close();
  },
  grain: async ({ hub }, use, info) => {
    const procs: ChildProcess[] = [];
    let i = 0;
    await use(async (o: GrainOptions) => {
      const id = o.id ?? `ws-${info.workerIndex}-${process.pid}-${++counter}-${++i}`;
      const token = o.token ?? (await hub.must(o.owner, 'POST', '/v1/node-tokens', { node: id })).token;
      const dir = mkdtempSync(join(tmpdir(), `grain-${id}-`));
      writeFileSync(join(dir, 'token'), token, { mode: 0o600 });
      const args = ['-hub', hub.url, '-token-file', join(dir, 'token'), '-id', id, '-repos', o.repos.join(','), '-addr-file', join(dir, 'addr')];
      if (o.interactive) args.push('-interactive');
      const log = createWriteStream(join(dir, 'grain.log'));
      const proc = spawn(`${binDir}/fakegrain`, args, { stdio: ['ignore', 'pipe', 'pipe'] });
      proc.stdout?.pipe(log);
      proc.stderr?.pipe(log);
      procs.push(proc);
      const addr = await until(`fake grain ${id} to listen`, async () => {
        if (proc.exitCode !== null) throw new Error(`fake grain ${id} exited ${proc.exitCode}`);
        return existsSync(join(dir, 'addr')) ? readFileSync(join(dir, 'addr'), 'utf8') : undefined;
      });
      const g = new Grain(id, `http://${addr}`, proc);
      await g.waitFor(`${id} online`, (s) => s.online);
      return g;
    });
    for (const p of procs) p.kill('SIGTERM');
  },
  fileTask: async ({ hub }, use) => {
    await use((user, body) => hub.must(user, 'POST', '/v1/tasks', body));
  },
});

export { expect };

// storage reports what a page's origin has put in every store a browser
// keeps: Web Storage, IndexedDB, Cache Storage, and service workers.
export async function storage(page: Page) {
  return page.evaluate(async () => {
    const dump = (s: Storage) => Object.fromEntries(Array.from({ length: s.length }, (_, i) => s.key(i)!).map((k) => [k, s.getItem(k)]));
    const caches_: Record<string, string[]> = {};
    for (const name of await caches.keys()) {
      const c = await caches.open(name);
      const entries: string[] = [];
      for (const req of await c.keys()) {
        const res = await c.match(req);
        entries.push(`${req.url} ${res ? await res.text() : ''}`);
      }
      caches_[name] = entries;
    }
    const dbs = indexedDB.databases ? await indexedDB.databases() : [];
    const workers = navigator.serviceWorker ? await navigator.serviceWorker.getRegistrations() : [];
    return {
      localStorage: dump(localStorage),
      sessionStorage: dump(sessionStorage),
      indexedDB: dbs.map((d) => d.name ?? ''),
      caches: caches_,
      serviceWorkers: workers.map((w) => w.scope),
    };
  });
}
