// HubClient is the hub's public API (docs/hub.md §5-§7) as a client
// calls it. It has no other: no private endpoint, and no decision of
// its own about who may see what. A task the caller may not see answers
// 404 here as anywhere.
//
// In a browser on the hub's own origin it needs no options: every
// request is same-origin and carries only the browser's own credentials
// (IAP's cookie, or the OIDC session's), and no token is read, stored
// or forwarded. Anywhere else (Node, a script, another origin) the
// caller passes baseURL and a token, which goes in Authorization as a
// bearer token: the hub's OIDC ID token, or a Google-signed OIDC token
// for IAP.
//
// A write carries a JSON body, as the hub's identity.SameOrigin
// requires; a browser marks it same-origin itself (Origin and
// Sec-Fetch-Site), and a client that is not a browser sends neither.
import { check } from './errors.js';
import { followEventSource, followFetch, type Connection, type EventStream, type FollowOptions, type Transport } from './events.js';
import type {
  Approval,
  ArtifactResult,
  Decision,
  Grainfile,
  GrainfileEvent,
  GrainfileInspection,
  GrainfileQuery,
  GrainfilesOverview,
  GrainfileSubscriptions,
  HubEvent,
  InboxItem,
  InboxPreferences,
  InboxQuery,
  Me,
  MintedToken,
  NewTask,
  NodeEntry,
  NodeToken,
  ResolvedTheme,
  Role,
  Task,
  TaskMessage,
  TaskQuery,
  Team,
  TeamSettings,
} from './models.js';

// TokenSource is a bearer token, or a function that answers the current
// one (called before every request, so it may refresh).
export type TokenSource = string | (() => string | Promise<string>);

export interface HubClientOptions {
  // baseURL is the hub's origin, such as https://hub.example.com. Empty,
  // the default, is the page's own origin.
  baseURL?: string;
  // token is sent as `Authorization: Bearer <token>`.
  token?: TokenSource;
  // tokenHeader overrides the header token goes in: behind IAP, a
  // caller whose Authorization is the hub's own uses
  // Proxy-Authorization for IAP's.
  tokenHeader?: string;
  // headers go on every request.
  headers?: Record<string, string>;
  // credentials is fetch's: 'same-origin' by default; 'include' for a
  // browser page on another origin that signs in with the hub's cookie.
  credentials?: RequestCredentials;
  // fetch replaces the global fetch.
  fetch?: (input: string, init?: RequestInit) => Promise<Response>;
  // transport is how follow() reads events: 'eventsource' by default
  // in a browser with no token or headers to send, 'fetch' otherwise.
  transport?: Transport;
  // adminMode, when explicitly set (true or false), sends
  // `X-Silo-Admin-Mode: on` or `off` on every request so a configured
  // hub admin can toggle global hub admin privileges on and off.
  adminMode?: boolean;
}

type Method = 'POST' | 'PUT' | 'DELETE';

const enc = (s: string): string => {
  const encoded = encodeURIComponent(s);
  if (encoded === '.' || encoded === '..') {
    throw new Error(`invalid path segment: ${JSON.stringify(s)}`);
  }
  return encoded;
};
const taskPath = (id: string) => `/v1/tasks/${enc(id)}`;
const teamPath = (team: string) => `/v1/teams/${enc(team)}`;
// repoPath keeps the slash between a repository's owner and name: the
// API's route is /repos/{owner}/{name}.
const repoPath = (team: string, repo: string) => `${teamPath(team)}/repos/${repo.split('/').map(enc).join('/')}`;

function cleanTimestamp(ts?: string): string | undefined {
  if (!ts || ts.startsWith('0001-01-01')) return undefined;
  return ts;
}

function normalizeGrainfile(g: Grainfile): Grainfile {
  const out: Grainfile = {
    ...g,
    grainfile: g.grainfile ?? 'v1',
    spec_summary_path: g.spec_summary_path ?? 'docs/grainfile-v1.md',
    spec_path: g.spec_path ?? 'docs/grainfiles-spec.md',
  };
  if (g.is_template || g.path?.endsWith('.jinja2')) {
    out.is_template = true;
  }
  const triggers = g.when ?? g.on ?? g.on_spec;
  if (triggers && Object.keys(triggers).length > 0) {
    out.when = triggers;
    out.on = triggers;
  } else {
    delete out.when;
    delete out.on;
  }
  out.not_before = cleanTimestamp(g.not_before);
  if (!out.not_before) delete out.not_before;
  out.created_at = cleanTimestamp(g.created_at);
  if (!out.created_at) delete out.created_at;
  if (g.recent_events) {
    out.recent_events = g.recent_events.map(normalizeGrainfileEvent);
  }
  return out;
}

function normalizeGrainfileEvent(e: GrainfileEvent): GrainfileEvent {
  const out: GrainfileEvent = {
    ...e,
    grainfile: e.grainfile ?? 'v1',
    spec_summary_path: e.spec_summary_path ?? 'docs/grainfile-v1.md',
    spec_path: e.spec_path ?? 'docs/grainfiles-spec.md',
  };
  if (e.is_template || e.path?.endsWith('.jinja2')) {
    out.is_template = true;
  }
  const triggers = e.when ?? e.on ?? e.on_spec;
  if (triggers && Object.keys(triggers).length > 0) {
    out.when = triggers;
    out.on = triggers;
  } else {
    delete out.when;
    delete out.on;
  }
  out.not_before = cleanTimestamp(e.not_before);
  if (!out.not_before) delete out.not_before;
  out.created_at = cleanTimestamp(e.created_at);
  if (!out.created_at) delete out.created_at;
  return out;
}

function grainfilesQueryString(q: GrainfileQuery = {}): string {
  const p = new URLSearchParams();
  if (q.kind) p.set('kind', q.kind);
  if (q.recipient) p.set('recipient', q.recipient);
  if (q.repo) p.set('repo', q.repo);
  if (q.draft !== undefined) p.set('draft', q.draft ? 'true' : 'false');
  if (q.include_drafts !== undefined) p.set('include_drafts', q.include_drafts ? 'true' : 'false');
  if (q.gate !== undefined) p.set('gate', q.gate ? 'true' : 'false');
  if (q.requestor) p.set('requestor', q.requestor);
  if (q.path) p.set('path', q.path);
  if (q.node) p.set('node', q.node);
  if (q.url) p.set('url', q.url);
  const qs = p.toString();
  return `/v1/grainfiles${qs ? `?${qs}` : ''}`;
}

function matchesRepoFilter(itemRepo: string, filterRepo: string): boolean {
  const norm = (s: string) => s.trim().replace(/^https?:\/\/github\.com\//i, '').replace(/^\/+|\/+$/g, '').toLowerCase();
  return norm(itemRepo) === norm(filterRepo);
}

function matchesRecipientFilter(itemRecipient: string | undefined, filterRecipient: string): boolean {
  if (!itemRecipient) return false;
  const norm = (s: string) => s.trim().replace(/\/+$/, '').toLowerCase();
  return norm(itemRecipient) === norm(filterRecipient);
}

function matchesGrainfileTarget(g: Grainfile, urlOrPath: string): boolean {
  const target = urlOrPath.trim().replace(/\/+$/, '');
  const repoSlug = g.repo.replace(/^https?:\/\/github\.com\//i, '').replace(/\/+$/, '');
  return (
    g.url.replace(/\/+$/, '') === target ||
    g.path === target.replace(/^\/+/, '') ||
    `${repoSlug}/${g.path}` === target ||
    `${g.repo.replace(/\/+$/, '')}/${g.path}` === target
  );
}

export class HubClient {
  private readonly base: string;
  private adminMode?: boolean;

  constructor(private readonly opts: HubClientOptions = {}) {
    this.base = (opts.baseURL ?? '').replace(/\/+$/, '');
    this.adminMode = opts.adminMode;
  }

  setAdminMode(on: boolean): void {
    this.adminMode = on;
  }

  getAdminMode(): boolean {
    return Boolean(this.adminMode);
  }

  // request sends one request to path and answers the response, or
  // throws the ApiError for a refusal.
  private async request(path: string, init: RequestInit = {}): Promise<Response> {
    const headers = { ...(await this.headers()), ...(init.headers as Record<string, string> | undefined) };
    return check(await this.send(this.base + path, { credentials: this.opts.credentials ?? 'same-origin', ...init, headers }));
  }

  // send is fetch, looked up when it is called: the global one, or the
  // caller's.
  private send(url: string, init: RequestInit): Promise<Response> {
    const f = this.opts.fetch;
    return f ? f(url, init) : fetch(url, init);
  }

  private async headers(): Promise<Record<string, string>> {
    const h: Record<string, string> = { ...this.opts.headers };
    if (this.adminMode !== undefined) {
      h['X-Silo-Admin-Mode'] = this.adminMode ? 'on' : 'off';
    }
    const t = this.opts.token;
    if (t !== undefined) h[this.opts.tokenHeader ?? 'Authorization'] = `Bearer ${typeof t === 'function' ? await t() : t}`;
    return h;
  }

  private async getJSON<T>(path: string, signal?: AbortSignal): Promise<T> {
    return (await this.request(path, { headers: { Accept: 'application/json' }, signal })).json() as Promise<T>;
  }

  // write sends one state-changing request, with body as JSON when
  // given, and answers the response's JSON, or undefined for an empty
  // one.
  private async write<T>(method: Method, path: string, body?: unknown, init: RequestInit = {}): Promise<T> {
    const headers: Record<string, string> = { Accept: 'application/json' };
    if (body !== undefined) headers['Content-Type'] = 'application/json';
    const r = await this.request(path, { ...init, method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
    const text = await r.text();
    return (text ? JSON.parse(text) : undefined) as T;
  }

  // The caller (docs/hub.md §5.3).

  getMe(signal?: AbortSignal): Promise<Me> {
    return this.getJSON<Me>('/v1/me', signal);
  }

  setTheme(theme: ResolvedTheme): Promise<{ theme: ResolvedTheme }> {
    return this.write<{ theme: ResolvedTheme }>('PUT', '/v1/me/theme', { theme });
  }

  getInboxPreferences(signal?: AbortSignal): Promise<InboxPreferences> {
    return this.getJSON<InboxPreferences>('/v1/me/inbox', signal);
  }

  setInboxPreferences(nodes: string[]): Promise<InboxPreferences> {
    return this.write<InboxPreferences>('PUT', '/v1/me/inbox', { nodes });
  }

  async listInbox(q: InboxQuery = {}, signal?: AbortSignal): Promise<InboxItem[]> {
    const p = new URLSearchParams();
    if (q.nodes !== undefined) {
      p.set('nodes', q.nodes.join(','));
    }
    if (q.kind) {
      p.set('kind', Array.isArray(q.kind) ? q.kind.join(',') : q.kind);
    }
    const s = p.toString();
    const r = await this.getJSON<{ items: InboxItem[]; nodes?: string[] }>(`/v1/inbox${s ? `?${s}` : ''}`, signal);
    return r.items ?? [];
  }

  // The backlog (docs/hub.md §6.1).

  async listTasks(q: TaskQuery = {}, signal?: AbortSignal): Promise<Task[]> {
    const p = new URLSearchParams();
    if (q.team) p.set('team', q.team);
    if (q.states?.length) p.set('state', q.states.join(','));
    if (q.limit) p.set('limit', String(q.limit));
    const s = p.toString();
    const r = await this.getJSON<{ tasks: Task[] }>(`/v1/tasks${s ? `?${s}` : ''}`, signal);
    return r.tasks ?? [];
  }

  getTask(id: string, signal?: AbortSignal): Promise<Task> {
    return this.getJSON<Task>(taskPath(id), signal);
  }

  submitTask(t: NewTask): Promise<Task> {
    return this.write<Task>('POST', '/v1/tasks', t);
  }

  cancelTask(id: string, reason?: string): Promise<Task> {
    return this.write<Task>('POST', `${taskPath(id)}/cancel`, reason ? { reason } : undefined);
  }

  requeueTask(id: string): Promise<Task> {
    return this.write<Task>('POST', `${taskPath(id)}/requeue`);
  }

  // Proposals and approvals (docs/hub.md §6.1): a member of a proposed
  // task's team approves or rejects it, and answers a running task's
  // approval requests.

  approveTask(id: string): Promise<Task> {
    return this.write<Task>('POST', `${taskPath(id)}/approve`);
  }

  rejectTask(id: string, reason?: string): Promise<Task> {
    return this.write<Task>('POST', `${taskPath(id)}/reject`, reason ? { reason } : undefined);
  }

  async listApprovals(id: string, signal?: AbortSignal): Promise<Approval[]> {
    const r = await this.getJSON<{ approvals: Approval[] }>(`${taskPath(id)}/approvals`, signal);
    return r.approvals ?? [];
  }

  answerApproval(id: string, request: string, decision: Decision, reason?: string): Promise<Approval> {
    return this.write<Approval>('POST', `${taskPath(id)}/approvals/${enc(request)}`, reason ? { decision, reason } : { decision });
  }

  // Sessions (docs/hub.md §6.5): the submitter of an interactive task
  // and the members of its team send it messages.

  async listMessages(id: string, signal?: AbortSignal): Promise<TaskMessage[]> {
    const r = await this.getJSON<{ messages: TaskMessage[] }>(`${taskPath(id)}/messages`, signal);
    return r.messages ?? [];
  }

  // sendMessage posts text to the session. messageID names the
  // message: the same ID again is the same message retried, which the
  // hub keeps and sends once.
  sendMessage(id: string, messageID: string, text: string): Promise<TaskMessage> {
    return this.write<TaskMessage>('POST', `${taskPath(id)}/messages`, { id: messageID, text });
  }

  // Live events (docs/hub.md §7.1).

  // eventsURL is a task's event stream's URL, with from as its cursor
  // when it is given.
  eventsURL(task: string, from?: number): string {
    return `${this.base}${taskPath(task)}/events${from !== undefined ? `?from=${from}` : ''}`;
  }

  // follow subscribes to task's events from o.from, calling o.onEvent
  // for each in seq order, once, until the stream ends (the task's
  // events are over), is refused, or is closed.
  follow(task: string, o: FollowOptions): EventStream {
    const transport = o.transport ?? this.opts.transport ?? this.defaultTransport();
    return transport === 'eventsource' ? followEventSource(this.connection(), task, o) : followFetch(this.connection(), task, o);
  }

  // events is follow as an async iterable: for await (const e of
  // hub.events(id)) reads every event and finishes when the stream
  // ends. A refusal is thrown from the loop; breaking out of it closes
  // the stream.
  events(task: string, o: { from?: number; signal?: AbortSignal; transport?: Transport } = {}): AsyncIterable<HubEvent> {
    return {
      [Symbol.asyncIterator]: () => {
        const queue: HubEvent[] = [];
        let wake: (() => void) | undefined;
        let failure: unknown;
        const poke = () => {
          wake?.();
          wake = undefined;
        };
        const s = this.follow(task, {
          ...o,
          onEvent: (e) => {
            queue.push(e);
            poke();
          },
          onError: (err) => {
            if ((err as { status?: number })?.status !== undefined) failure = err;
          },
        });
        void s.done.then(poke);
        return {
          next: async (): Promise<IteratorResult<HubEvent>> => {
            for (;;) {
              const e = queue.shift();
              if (e) return { value: e, done: false };
              if (s.state === 'ended' || s.state === 'closed') {
                if (failure) throw failure;
                return { value: undefined, done: true };
              }
              await new Promise<void>((r) => (wake = r));
            }
          },
          return: async (): Promise<IteratorResult<HubEvent>> => {
            s.close();
            return { value: undefined, done: true };
          },
        };
      },
    };
  }

  private defaultTransport(): Transport {
    const plain = this.opts.token === undefined && !Object.keys(this.opts.headers ?? {}).length && !this.opts.fetch;
    return plain && typeof EventSource !== 'undefined' ? 'eventsource' : 'fetch';
  }

  private connection(): Connection {
    return {
      url: (task, from) => this.eventsURL(task, from),
      fetch: (url, init) => this.send(url, init),
      headers: () => this.headers(),
      withCredentials: this.opts.credentials === 'include',
    };
  }

  // fetchArtifact reads a node-only artifact through the hub's
  // view-through (GET /v1/tasks/{id}/artifacts?ref=, docs/hub.md §7.2)
  // from the node that holds it. It is never cached (cache: 'no-store',
  // as the hub's Cache-Control says) and never written anywhere: the
  // caller holds the text in memory and drops it. A node that is
  // offline answers 503, and then there is no copy to show.
  async fetchArtifact(task: string, ref: string, signal?: AbortSignal): Promise<ArtifactResult> {
    const r = await this.send(`${this.base}${taskPath(task)}/artifacts?ref=${encodeURIComponent(ref)}`, {
      credentials: this.opts.credentials ?? 'same-origin',
      cache: 'no-store',
      headers: await this.headers(),
      signal,
    });
    const text = await r.text();
    if (r.status === 503) return { kind: 'unavailable', message: text.trim() || 'the node that holds it is offline' };
    if (!r.ok) return { kind: 'error', status: r.status, message: text.trim() || r.statusText };
    return { kind: 'text', text };
  }

  // The node directory (docs/hub.md §4.2).

  async listNodes(signal?: AbortSignal): Promise<NodeEntry[]> {
    const r = await this.getJSON<{ nodes: NodeEntry[] }>('/v1/nodes', signal);
    return r.nodes ?? [];
  }

  // Node tokens (docs/hub.md §5.2).

  async listNodeTokens(signal?: AbortSignal): Promise<NodeToken[]> {
    const r = await this.getJSON<{ node_tokens: NodeToken[] }>('/v1/node-tokens', signal);
    return r.node_tokens ?? [];
  }

  // mintNodeToken's answer is uncached (the hub says no-store, and the
  // fetch asks for none) and is the caller's to show once and drop.
  mintNodeToken(node: string): Promise<MintedToken> {
    return this.write<MintedToken>('POST', '/v1/node-tokens', { node }, { cache: 'no-store' });
  }

  revokeNodeToken(id: string): Promise<void> {
    return this.write('DELETE', `/v1/node-tokens/${enc(id)}`);
  }

  // The team directory (docs/hub.md §5.3).

  async listTeams(signal?: AbortSignal): Promise<Team[]> {
    const r = await this.getJSON<{ teams: Team[] }>('/v1/teams', signal);
    return r.teams ?? [];
  }

  getTeam(team: string, signal?: AbortSignal): Promise<Team> {
    return this.getJSON<Team>(teamPath(team), signal);
  }

  createTeam(id: string, name: string): Promise<void> {
    return this.write('POST', '/v1/teams', { id, name });
  }

  deleteTeam(team: string): Promise<void> {
    return this.write('DELETE', teamPath(team));
  }

  setMember(team: string, principal: string, role: Role): Promise<void> {
    return this.write('PUT', `${teamPath(team)}/members/${enc(principal)}`, { role });
  }

  removeMember(team: string, principal: string): Promise<void> {
    return this.write('DELETE', `${teamPath(team)}/members/${enc(principal)}`);
  }

  assignRepo(team: string, repo: string, history = false): Promise<void> {
    return this.write('PUT', repoPath(team, repo), history ? { history } : undefined);
  }

  releaseRepo(team: string, repo: string): Promise<void> {
    return this.write('DELETE', repoPath(team, repo));
  }

  // Team settings (docs/hub.md §6.3).

  getTeamSettings(team: string, signal?: AbortSignal): Promise<TeamSettings> {
    return this.getJSON<TeamSettings>(`${teamPath(team)}/settings`, signal);
  }

  setTeamSettings(team: string, s: TeamSettings): Promise<TeamSettings> {
    return this.write<TeamSettings>('PUT', `${teamPath(team)}/settings`, s);
  }

  // Grainfile discovery, metadata inspection, and node subscriptions (docs/hub.md §6).

  async getGrainfilesOverview(q: GrainfileQuery = {}, signal?: AbortSignal): Promise<GrainfilesOverview> {
    const r = await this.getJSON<Partial<GrainfilesOverview>>(grainfilesQueryString(q), signal);
    let grainfiles = (r.grainfiles ?? []).map(normalizeGrainfile);
    let subscriptions = r.subscriptions ?? [];
    let events = (r.events ?? []).map(normalizeGrainfileEvent);

    if (q.kind) {
      grainfiles = grainfiles.filter((g) => g.kind === q.kind);
      events = events.filter((e) => !e.kind || e.kind === q.kind);
    }
    if (q.recipient) {
      grainfiles = grainfiles.filter((g) => matchesRecipientFilter(g.recipient, q.recipient!));
      events = events.filter((e) => !e.recipient || matchesRecipientFilter(e.recipient, q.recipient!));
    }
    if (q.repo) {
      grainfiles = grainfiles.filter((g) => matchesRepoFilter(g.repo, q.repo!));
      events = events.filter((e) => matchesRepoFilter(e.repo, q.repo!));
    }
    if (q.draft !== undefined) {
      grainfiles = grainfiles.filter((g) => Boolean(g.draft) === q.draft);
    } else if (q.include_drafts === false) {
      grainfiles = grainfiles.filter((g) => !g.draft);
    }
    if (q.gate !== undefined) {
      grainfiles = grainfiles.filter((g) => Boolean(g.gate) === q.gate);
    }
    if (q.requestor) {
      const wantReq = q.requestor.trim().replace(/\/+$/, '').toLowerCase();
      grainfiles = grainfiles.filter((g) => (g.requestor ?? '').trim().replace(/\/+$/, '').toLowerCase() === wantReq);
    }
    if (q.path) {
      const wantPath = q.path.trim().replace(/^\/+/, '');
      grainfiles = grainfiles.filter((g) => g.path === wantPath);
    }
    if (q.node) {
      subscriptions = subscriptions.filter((s) => s.node_id === q.node);
    }
    if (q.url) {
      grainfiles = grainfiles.filter((g) => matchesGrainfileTarget(g, q.url!));
      events = events.filter(
        (e) =>
          e.grainfile_url === q.url ||
          e.path === q.url ||
          e.dependency_url === q.url ||
          e.triggered_by === q.url,
      );
    }

    return { grainfiles, subscriptions, events };
  }

  async listGrainfiles(q: GrainfileQuery = {}, signal?: AbortSignal): Promise<Grainfile[]> {
    const r = await this.getGrainfilesOverview(q, signal);
    return r.grainfiles;
  }

  async inspectGrainfile(urlOrPath: string, signal?: AbortSignal): Promise<GrainfileInspection> {
    const r = await this.getGrainfilesOverview({ url: urlOrPath, include_drafts: true }, signal);
    const grainfile = r.grainfiles.find((g) => matchesGrainfileTarget(g, urlOrPath)) ?? r.grainfiles[0] ?? null;
    const targetUrl = grainfile?.url ?? urlOrPath;
    const events = r.events.filter(
      (e) =>
        e.grainfile_url === targetUrl ||
        e.grainfile_url === urlOrPath ||
        (grainfile && e.path === grainfile.path && matchesRepoFilter(e.repo, grainfile.repo)) ||
        e.dependency_url === targetUrl ||
        e.triggered_by === targetUrl,
    );
    return {
      grainfile,
      events,
      subscriptions: r.subscriptions,
    };
  }

  async listGrainfileSubscriptions(node?: string, signal?: AbortSignal): Promise<GrainfileSubscriptions[]> {
    const r = await this.getGrainfilesOverview(node ? { node } : {}, signal);
    return r.subscriptions;
  }

  async listGrainfileEvents(q: GrainfileQuery = {}, signal?: AbortSignal): Promise<GrainfileEvent[]> {
    const r = await this.getGrainfilesOverview(q, signal);
    return r.events;
  }
}
