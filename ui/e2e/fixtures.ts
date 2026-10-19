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
import { execFileSync, spawn, type ChildProcess } from 'node:child_process';
import { createHmac } from 'node:crypto';
import { mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync, existsSync, createWriteStream } from 'node:fs';
import { createServer as createHttpServer, type Server as HttpServer } from 'node:http';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { binDir } from './paths';

export const userHeader = 'X-Silo-E2e-User';
export const hubAdmin = 'admin@e2e.test';
export const webhookSecret = 'e2e-webhook-secret';
export const githubToken = 'ghs_e2e_sim_token';
export const nodeUIDomain = 'nodes.hub.e2e.test';

export interface Reply<T = any> {
  status: number;
  body: T;
}

export class Hub {
  constructor(
    readonly url: string,
    readonly proc: ChildProcess,
    readonly dir: string = '',
    readonly githubSim?: HttpServer,
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

  gitRepoDir(slug: string): string {
    return join(this.dir, 'repos', ...slug.split('/'));
  }

  private initGitRepo(slug: string, branch = 'main'): void {
    const repoDir = this.gitRepoDir(slug);
    mkdirSync(repoDir, { recursive: true });
    execFileSync('git', ['init', '-b', branch, repoDir], { stdio: 'ignore' });
    execFileSync('git', ['-C', repoDir, 'config', 'user.name', 'E2E Simulator'], { stdio: 'ignore' });
    execFileSync('git', ['-C', repoDir, 'config', 'user.email', 'sim@e2e.test'], { stdio: 'ignore' });
  }

  // pushGitHub commits files to a real local git repository served by the
  // worker's GitHub simulator and posts an HMAC-SHA256 signed push webhook
  // to the hub.
  async pushGitHub(slug: string, branch: string, files: Record<string, string>, removed: string[] = []): Promise<string> {
    const repoDir = this.gitRepoDir(slug);
    if (!existsSync(join(repoDir, '.git'))) this.initGitRepo(slug, branch);
    let beforeSha = '0000000000000000000000000000000000000000';
    try {
      beforeSha = execFileSync('git', ['-C', repoDir, 'rev-parse', `refs/heads/${branch}`], {
        encoding: 'utf8',
        stdio: ['ignore', 'pipe', 'ignore'],
      }).trim();
    } catch {
      // First commit on branch.
    }
    execFileSync('git', ['-C', repoDir, 'checkout', '-B', branch], { stdio: 'ignore' });
    const modified: string[] = [];
    for (const [relPath, content] of Object.entries(files)) {
      const abs = join(repoDir, relPath);
      mkdirSync(dirname(abs), { recursive: true });
      writeFileSync(abs, content, 'utf8');
      modified.push(relPath);
    }
    for (const relPath of removed) {
      const abs = join(repoDir, relPath);
      if (existsSync(abs)) rmSync(abs, { force: true });
    }
    execFileSync('git', ['-C', repoDir, 'add', '-A'], { stdio: 'ignore' });
    execFileSync('git', ['-C', repoDir, 'commit', '--allow-empty', '-m', `update ${modified.join(', ')}`], { stdio: 'ignore' });
    const afterSha = execFileSync('git', ['-C', repoDir, 'rev-parse', 'HEAD'], { encoding: 'utf8' }).trim();

    const payload = JSON.stringify({
      ref: `refs/heads/${branch}`,
      before: beforeSha,
      after: afterSha,
      repository: {
        full_name: slug,
        html_url: `https://github.com/${slug}`,
        default_branch: 'main',
      },
      head_commit: {
        id: afterSha,
        added: [],
        modified,
        removed,
      },
      commits: [
        {
          id: afterSha,
          added: [],
          modified,
          removed,
        },
      ],
    });
    const sig = 'sha256=' + createHmac('sha256', webhookSecret).update(payload).digest('hex');
    const res = await fetch(`${this.url}/v1/webhooks/github`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-GitHub-Event': 'push',
        'X-Hub-Signature-256': sig,
      },
      body: payload,
    });
    if (!res.ok) {
      throw new Error(`webhook failed: ${res.status} ${await res.text()}`);
    }
    return afterSha;
  }

  // pullRequestGitHub posts an HMAC-SHA256 signed pull_request webhook to the hub.
  async pullRequestGitHub(
    slug: string,
    opts: {
      action?: string;
      number?: number;
      headRef?: string;
      baseRef?: string;
      sha: string;
      modified?: string[];
      merged?: boolean;
    },
  ): Promise<void> {
    const payload = JSON.stringify({
      action: opts.action ?? (opts.merged ? 'closed' : 'opened'),
      merged: Boolean(opts.merged),
      repository: {
        full_name: slug,
        html_url: `https://github.com/${slug}`,
        default_branch: 'main',
      },
      pull_request: {
        number: opts.number ?? 1,
        merged: Boolean(opts.merged),
        merge_commit_sha: opts.merged ? opts.sha : undefined,
        head: { ref: opts.headRef ?? 'feature', sha: opts.sha },
        base: { ref: opts.baseRef ?? 'main', sha: opts.sha },
        modified: opts.modified ?? [],
        changed_paths: opts.modified ?? [],
      },
    });
    const sig = 'sha256=' + createHmac('sha256', webhookSecret).update(payload).digest('hex');
    const res = await fetch(`${this.url}/v1/webhooks/github`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        'X-GitHub-Event': 'pull_request',
        'X-Hub-Signature-256': sig,
      },
      body: payload,
    });
    if (!res.ok) {
      throw new Error(`pull_request webhook failed: ${res.status} ${await res.text()}`);
    }
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
  const githubSim = createHttpServer((req, res) => {
    const auth = req.headers['authorization'];
    if (auth !== `Bearer ${githubToken}`) {
      res.writeHead(401, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ message: 'Bad credentials' }));
      return;
    }
    const u = new URL(req.url ?? '/', 'http://127.0.0.1');
    const m = u.pathname.match(/^\/repos\/([^/]+)\/([^/]+)\/contents\/(.+)$/);
    if (!m) {
      res.writeHead(404);
      res.end('not found');
      return;
    }
    const [, owner, repoName, rawPath] = m;
    const filePath = rawPath.split('/').map(decodeURIComponent).join('/');
    const ref = u.searchParams.get('ref') || 'HEAD';
    const repoDir = join(dir, 'repos', owner, repoName);
    try {
      const out = execFileSync('git', ['-C', repoDir, 'show', `${ref}:${filePath}`], {
        stdio: ['ignore', 'pipe', 'ignore'],
      });
      res.writeHead(200, { 'Content-Type': 'application/octet-stream' });
      res.end(out);
    } catch {
      res.writeHead(404, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ message: 'Not Found' }));
    }
  });
  await new Promise<void>((resolve) => githubSim.listen(0, '127.0.0.1', () => resolve()));
  const simAddr = githubSim.address();
  const githubSimUrl = `http://127.0.0.1:${simAddr && typeof simAddr === 'object' ? simAddr.port : 0}`;

  const port = await freePort();
  const url = `http://127.0.0.1:${port}`;
  let database = process.env.SILO_E2E_DATABASE || `sqlite:${join(dir, 'hub.db')}`;
  if (database.startsWith('postgres://') || database.startsWith('postgresql://')) {
    const dbName = `silo_e2e_${port}_${process.pid}`;
    execFileSync('psql', [database, '-c', `CREATE DATABASE ${dbName}`], { stdio: 'ignore' });
    const parsed = new URL(database);
    parsed.pathname = `/${dbName}`;
    database = parsed.toString();
  }
  const log = createWriteStream(join(dir, 'hub.log'));
  const proc = spawn(
    `${binDir}/silo`,
    [
      'hub',
      'serve',
      '-addr',
      `127.0.0.1:${port}`,
      '-database',
      database,
      '-dev-user-header',
      userHeader,
      '-admins',
      `user:${hubAdmin}`,
      '-heartbeat',
      '1s',
      '-dispatch-retry',
      '1s',
      '-shutdown-grace',
      '2s',
      '-webhook-secret',
      webhookSecret,
      '-github-token',
      githubToken,
      '-github-api-url',
      githubSimUrl,
      '-node-ui-domain',
      nodeUIDomain,
    ],
    { stdio: ['ignore', 'pipe', 'pipe'] },
  );
  proc.stdout?.pipe(log);
  proc.stderr?.pipe(log);
  await until('the hub to answer /healthz', async () => {
    if (proc.exitCode !== null) throw new Error(`the hub exited ${proc.exitCode}; see ${join(dir, 'hub.log')}`);
    return (await fetch(url + '/healthz')).ok || undefined;
  });
  return new Hub(url, proc, dir, githubSim);
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
  failures: number;
  last_error: string;
  tasks: GrainTask[];
  grainfile_events?: any[];
}

export interface GrainOptions {
  owner: string;
  repos: string[];
  id?: string;
  interactive?: boolean;
  nodeui?: boolean;
  recipients?: string[];
  folders?: string[];
  grainfiles?: string[];
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
      hub.githubSim?.close();
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
      await hub.must(hubAdmin, 'PUT', `/v1/users/${encodeURIComponent(w.outsider)}`, { sys_admin: false });
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
      if (o.nodeui) args.push('-nodeui');
      if (o.recipients && o.recipients.length > 0) args.push('-recipients', o.recipients.join(','));
      if (o.folders && o.folders.length > 0) args.push('-folders', o.folders.join(','));
      if (o.grainfiles && o.grainfiles.length > 0) args.push('-grainfiles', o.grainfiles.join(','));
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
