// The SDK against a real `silo hub serve` (this tree's, built by
// setup.ts) and a scripted fake Grain (internal/fakegrain) on the real
// rendezvous: the README's examples, run in Node. Sign-on is the hub's
// development user header, sent with the client's headers option as a
// bearer token would be with token.
import { execFileSync, spawn, type ChildProcess } from 'node:child_process';
import { createHmac } from 'node:crypto';
import { createWriteStream, existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { createServer as createHttpServer, type Server as HttpServer } from 'node:http';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { dirname, join } from 'node:path';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { ConflictError, HubClient, NotFoundError, type HubEvent, type Task } from '../../src/index.js';
import { binDir } from './paths.js';

const userHeader = 'X-Silo-Sdk-User';
const admin = 'admin@sdk.test';
const alice = 'alice@sdk.test';
const outsider = 'carol@sdk.test';
const team = 'sdk';
const repo = 'sdk/app';
const repoDocs = 'sdk/docs';
const node = 'ws-sdk';
const recipientAlpha = 'https://github.com/sdk/grain-alpha';
const recipientBeta = 'https://github.com/sdk/grain-beta';
const webhookSecret = 'sdk-webhook-secret';
const githubToken = 'ghs_sdk_sim_token';

const procs: ChildProcess[] = [];
let dir: string;
let url: string;
let grain: string;
let githubSim: HttpServer;
let githubSimUrl: string;

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

function gitRepoDir(slug: string): string {
  return join(dir, 'repos', ...slug.split('/'));
}

function initGitRepo(slug: string): void {
  const repoDir = gitRepoDir(slug);
  mkdirSync(repoDir, { recursive: true });
  execFileSync('git', ['init', '-b', 'main', repoDir], { stdio: 'ignore' });
  execFileSync('git', ['-C', repoDir, 'config', 'user.name', 'SDK Simulator'], { stdio: 'ignore' });
  execFileSync('git', ['-C', repoDir, 'config', 'user.email', 'sim@sdk.test'], { stdio: 'ignore' });
}

async function commitAndPushWebhook(slug: string, branch: string, files: Record<string, string>): Promise<string> {
  const repoDir = gitRepoDir(slug);
  if (!existsSync(join(repoDir, '.git'))) initGitRepo(slug);
  let beforeSha = '0000000000000000000000000000000000000000';
  try {
    beforeSha = execFileSync('git', ['-C', repoDir, 'rev-parse', `refs/heads/${branch}`], { encoding: 'utf8', stdio: ['ignore', 'pipe', 'ignore'] }).trim();
  } catch {
    // first commit on branch
  }
  const modified: string[] = [];
  for (const [relPath, content] of Object.entries(files)) {
    const abs = join(repoDir, relPath);
    mkdirSync(dirname(abs), { recursive: true });
    writeFileSync(abs, content, 'utf8');
    modified.push(relPath);
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
      removed: [],
    },
    commits: [
      {
        id: afterSha,
        added: [],
        modified,
        removed: [],
      },
    ],
  });
  const sig = 'sha256=' + createHmac('sha256', webhookSecret).update(payload).digest('hex');
  const res = await fetch(`${url}/v1/webhooks/github`, {
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

beforeAll(async () => {
  dir = mkdtempSync(join(tmpdir(), 'silo-sdk-'));

  githubSim = createHttpServer((req, res) => {
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
    const repoDir = gitRepoDir(`${owner}/${repoName}`);
    try {
      const out = execFileSync('git', ['-C', repoDir, 'show', `${ref}:${filePath}`], { stdio: ['ignore', 'pipe', 'ignore'] });
      res.writeHead(200, { 'Content-Type': 'application/octet-stream' });
      res.end(out);
    } catch {
      res.writeHead(404, { 'Content-Type': 'application/json' });
      res.end(JSON.stringify({ message: 'Not Found' }));
    }
  });
  await new Promise<void>((resolve) => githubSim.listen(0, '127.0.0.1', () => resolve()));
  const simAddr = githubSim.address();
  githubSimUrl = `http://127.0.0.1:${simAddr && typeof simAddr === 'object' ? simAddr.port : 0}`;

  const port = await freePort();
  url = `http://127.0.0.1:${port}`;
  const hub = start('silo', [
    'hub', 'serve', '-addr', `127.0.0.1:${port}`, '-database', `sqlite:${join(dir, 'hub.db')}`,
    '-dev-user-header', userHeader, '-admins', `user:${admin}`, '-heartbeat', '1s', '-dispatch-retry', '1s', '-shutdown-grace', '2s',
    '-webhook-secret', webhookSecret, '-github-token', githubToken, '-github-api-url', githubSimUrl,
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
  await a.assignRepo(team, repoDocs);

  // A fake Grain with a token alice mints through the SDK.
  const { token } = await as(alice).mintNodeToken(node);
  writeFileSync(join(dir, 'token'), token, { mode: 0o600 });
  start('fakegrain', [
    '-hub', url, '-token-file', join(dir, 'token'), '-id', node,
    '-repos', `${repo},${repoDocs}`, '-recipients', recipientAlpha,
    '-addr-file', join(dir, 'addr'),
  ]);
  grain = 'http://' + (await until('the fake grain to listen', async () => (existsSync(join(dir, 'addr')) ? readFileSync(join(dir, 'addr'), 'utf8') : undefined)));
  await until('the fake grain to be online', async () => ((await control('GET', '/state')).online ? true : undefined));
}, 60_000);

afterAll(() => {
  for (const p of procs) p.kill('SIGTERM');
  githubSim?.close();
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

  it('indexes Grainfiles from real git repos and filters/inspects them through the SDK', async () => {
    const h = as(alice);

    await commitAndPushWebhook(repo, 'main', {
      'docs/spec.md': '# Spec\n',
      'tasks/schema.json': '{"type":"object"}\n',
      'tasks/migrate.task.md': [
        '---',
        'title: Migrate Database',
        `recipient: ${recipientAlpha}`,
        'requestor: Alice <alice@sdk.test>',
        'draft: false',
        'references:',
        '  - /docs/spec.md',
        '  - ./schema.json',
        '  - https://github.com/other/repo/blob/main/secret.md',
        'depends_on:',
        `  - https://github.com/${repoDocs}/blob/main/processes/lint.process.md`,
        'trigger_with:',
        '  - ./verify.process.md',
        '---',
        '# Migrate Database',
        '',
        'Run the migration.',
      ].join('\n'),
      'tasks/draft-idea.task.md': [
        '---',
        'title: Draft Idea',
        `recipient: ${recipientAlpha}`,
        'requestor: https://github.com/sdk/grain-beta',
        'draft: true',
        '---',
        '# Draft Idea',
        '',
        'Work in progress.',
      ].join('\n'),
      'tasks/verify.process.md': [
        '---',
        'title: Verify Migration',
        `recipient: ${recipientAlpha}`,
        'requestor: Alice <alice@sdk.test>',
        'gate: true',
        'on:',
        '  push:',
        '    branches: [main]',
        '    paths: ["src/**"]',
        '---',
        '# Verify Migration',
        '',
        'Verify migration step.',
      ].join('\n'),
    });

    await commitAndPushWebhook(repoDocs, 'main', {
      'processes/lint.process.md': [
        '---',
        'title: Lint Docs',
        `recipient: ${recipientBeta}`,
        'requestor: https://github.com/sdk/grain-alpha',
        'gate: false',
        'on:',
        '  pull_request:',
        '    branches: [main]',
        '---',
        '# Lint Docs',
        '',
        'Lint markdown files.',
      ].join('\n'),
    });

    // Default listGrainfiles includes non-drafts and drafts unless filtered.
    const all = await h.listGrainfiles({ include_drafts: true });
    expect(all.map((g) => `${g.repo}:${g.path}`)).toEqual([
      `https://github.com/${repoDocs}:processes/lint.process.md`,
      `https://github.com/${repo}:tasks/draft-idea.task.md`,
      `https://github.com/${repo}:tasks/migrate.task.md`,
      `https://github.com/${repo}:tasks/verify.process.md`,
    ]);

    // Filter by kind
    const tasksOnly = await h.listGrainfiles({ kind: 'task', include_drafts: true });
    expect(tasksOnly.map((g) => g.path)).toEqual(['tasks/draft-idea.task.md', 'tasks/migrate.task.md']);
    const procsOnly = await h.listGrainfiles({ kind: 'process', include_drafts: true });
    expect(procsOnly.map((g) => g.path)).toEqual(['processes/lint.process.md', 'tasks/verify.process.md']);

    // Filter by recipient
    const alphaOnly = await h.listGrainfiles({ recipient: recipientAlpha, include_drafts: true });
    expect(alphaOnly.map((g) => g.path)).toEqual([
      'tasks/draft-idea.task.md',
      'tasks/migrate.task.md',
      'tasks/verify.process.md',
    ]);
    const betaOnly = await h.listGrainfiles({ recipient: recipientBeta, include_drafts: true });
    expect(betaOnly.map((g) => g.path)).toEqual(['processes/lint.process.md']);

    // Filter by repo
    const docsOnly = await h.listGrainfiles({ repo: repoDocs, include_drafts: true });
    expect(docsOnly.map((g) => g.path)).toEqual(['processes/lint.process.md']);

    // Filter by draft
    const draftsOnly = await h.listGrainfiles({ draft: true });
    expect(draftsOnly.map((g) => g.path)).toEqual(['tasks/draft-idea.task.md']);
    const nonDraftsOnly = await h.listGrainfiles({ draft: false });
    expect(nonDraftsOnly.map((g) => g.path)).toEqual([
      'processes/lint.process.md',
      'tasks/migrate.task.md',
      'tasks/verify.process.md',
    ]);

    // Filter by gate
    const gatedOnly = await h.listGrainfiles({ gate: true, include_drafts: true });
    expect(gatedOnly.map((g) => g.path)).toEqual(['tasks/verify.process.md']);

    // Filter by requestor (both Name <email> and https:// URL)
    const byEmail = await h.listGrainfiles({ requestor: 'Alice <alice@sdk.test>', include_drafts: true });
    expect(byEmail.map((g) => g.path)).toEqual(['tasks/migrate.task.md', 'tasks/verify.process.md']);
    const byUrl = await h.listGrainfiles({ requestor: 'https://github.com/sdk/grain-beta', include_drafts: true });
    expect(byUrl.map((g) => g.path)).toEqual(['tasks/draft-idea.task.md']);

    // Combined filter
    const combined = await h.listGrainfiles({
      kind: 'process',
      recipient: recipientAlpha,
      repo,
      draft: false,
      gate: true,
      requestor: 'Alice <alice@sdk.test>',
    });
    expect(combined.map((g) => g.path)).toEqual(['tasks/verify.process.md']);

    // Direct getGrainfilesOverview and filter parameters (kind, recipient, repo, draft, node)
    const overview = await h.getGrainfilesOverview({ include_drafts: true });
    expect(overview.grainfiles.map((g) => g.path)).toEqual([
      'processes/lint.process.md',
      'tasks/draft-idea.task.md',
      'tasks/migrate.task.md',
      'tasks/verify.process.md',
    ]);
    expect(overview.subscriptions.some((s) => s.node_id === node && s.recipients?.includes(recipientAlpha))).toBe(true);
    expect(overview.events.map((e) => e.type)).toEqual(expect.arrayContaining(['task_ready', 'process_updated']));

    const overviewByKind = await h.getGrainfilesOverview({ kind: 'task', draft: false });
    expect(overviewByKind.grainfiles.map((g) => g.path)).toEqual(['tasks/migrate.task.md']);
    expect(overviewByKind.events.every((e) => !e.kind || e.kind === 'task')).toBe(true);

    const overviewByRecipient = await h.getGrainfilesOverview({ recipient: recipientBeta });
    expect(overviewByRecipient.grainfiles.map((g) => g.path)).toEqual(['processes/lint.process.md']);
    expect(overviewByRecipient.events.every((e) => !e.recipient || e.recipient === recipientBeta)).toBe(true);

    const overviewByRepo = await h.getGrainfilesOverview({ repo, draft: true });
    expect(overviewByRepo.grainfiles.map((g) => g.path)).toEqual(['tasks/draft-idea.task.md']);

    // Verify node recipient subscription reporting via listNodes and listGrainfileSubscriptions
    const nodes = await h.listNodes();
    const sdkNode = nodes.find((n) => n.id === node);
    expect(sdkNode).toBeDefined();
    expect(sdkNode?.features).toContain('grainfiles');
    expect(sdkNode?.recipients).toEqual([recipientAlpha]);

    const nodeSubs = await h.listGrainfileSubscriptions(node);
    expect(nodeSubs).toHaveLength(1);
    expect(nodeSubs[0].node_id).toBe(node);
    expect(nodeSubs[0].recipients).toEqual([recipientAlpha]);

    const alphaEvents = await h.listGrainfileEvents({ recipient: recipientAlpha, kind: 'task' });
    expect(alphaEvents.some((e) => e.type === 'task_ready' && e.path === 'tasks/migrate.task.md')).toBe(true);

    // Inspect a Grainfile for detail metadata, authorized_references, recent_events, and subscriptions
    const inspection = await h.inspectGrainfile(`https://github.com/${repo}/blob/main/tasks/migrate.task.md`);
    expect(inspection.grainfile).not.toBeNull();
    expect(inspection.grainfile?.title).toBe('Migrate Database');
    expect(inspection.grainfile?.references).toEqual([
      `https://github.com/${repo}/blob/main/docs/spec.md`,
      `https://github.com/${repo}/blob/main/tasks/schema.json`,
      'https://github.com/other/repo/blob/main/secret.md',
    ]);
    expect(inspection.grainfile?.authorized_references).toEqual([
      `https://github.com/${repo}/blob/main/docs/spec.md`,
      `https://github.com/${repo}/blob/main/tasks/schema.json`,
    ]);
    expect(inspection.grainfile?.depends_on).toEqual([
      `https://github.com/${repoDocs}/blob/main/processes/lint.process.md`,
    ]);
    expect(inspection.grainfile?.trigger_with).toEqual([
      `https://github.com/${repo}/blob/main/tasks/verify.process.md`,
    ]);
    expect(inspection.grainfile?.subscribed_nodes).toContain(node);
    expect(inspection.grainfile?.recent_events?.map((e) => e.type)).toContain('task_ready');
    expect(inspection.events.map((e) => e.type)).toContain('task_ready');
    expect(inspection.subscriptions.some((s) => s.node_id === node)).toBe(true);

    // Verify the live fakegrain node received the grainfile_events for recipientAlpha
    await until('fakegrain to receive grainfile_events', async () => {
      const st = await control('GET', '/state');
      const evs: Array<{ type: string; path: string }> = st.grainfile_events ?? [];
      return evs.some((e) => e.type === 'task_ready' && e.path === 'tasks/migrate.task.md') ? true : undefined;
    });
  });
});
