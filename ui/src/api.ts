// The hub's public API, as the UI uses it (docs/hub.md §5-§7). The UI
// has no other: no private endpoint, and no decision of its own about
// who may see what. A task the caller may not see answers 404 here as
// anywhere.
//
// Every request is same-origin and carries only the browser's own
// credentials (IAP's cookie, or the OIDC session's): no token is read,
// stored or forwarded.
//
// A write is a fetch the browser marks same-origin itself (Origin and
// Sec-Fetch-Site), with a JSON body, as the hub's identity.SameOrigin
// requires. The UI decides nothing about who may write: a refusal (401,
// 403, 404, 409) is the API's, and comes back as an ApiError carrying
// the API's own words.

export type TaskState = 'proposed' | 'rejected' | 'queued' | 'offered' | 'running' | 'done' | 'failed' | 'cancelled' | 'lost';

export const TASK_STATES: TaskState[] = ['proposed', 'queued', 'offered', 'running', 'done', 'failed', 'cancelled', 'rejected', 'lost'];

export type TaskKind = 'interactive';

export interface Task {
  id: string;
  team: string;
  repo?: string;
  // kind is "interactive" for a session, absent for a task that runs
  // to its result.
  kind?: TaskKind;
  title: string;
  prompt: string;
  submitter: string;
  approved_by?: string;
  approved_at?: string;
  constraints?: { labels?: Record<string, string>; node?: string; node_owner?: string };
  created_at: string;
  state: TaskState;
  node?: string;
  node_owner?: string;
  node_task?: string;
  reason?: string;
  excluded?: string[];
  attempts: number;
  updated_at: string;
  cancel_pending?: boolean;
}

export type Role = 'member' | 'admin' | string;

export interface Me {
  email: string;
  groups?: string[];
  hub_admin: boolean;
  teams: Record<string, Role> | null;
}

export interface NodeEntry {
  id: string;
  repos: string[];
  slots: number;
  version?: string;
  labels?: Record<string, string>;
  owner: string;
  status: 'online' | 'stale' | 'offline' | string;
  free: number;
  claimed: number;
  connected_at: string;
  last_seen: string;
}

// HubEvent is an event as the hub relays it (hub/live.Event): the
// envelope of package events, the hub's task ID, and for a node-only
// event only its artifact reference.
export interface HubEvent {
  v: number;
  seq: number;
  time: string;
  session?: string;
  type: string;
  task: string;
  status?: { phase: string; step?: string; state: string; message?: string };
  transcript?: { kind: string; text?: string };
  usage?: { source: string; model?: string; input_tokens: number; output_tokens: number };
  approval_request?: { id: string; tool: string; message?: string; deadline?: string };
  resumable?: { cli: string; cli_session: string };
  result?: {
    exit: { code: number; reason: string; detail?: string; cli_error?: { kind: string; message?: string } };
    final_text?: string;
    errors?: string[];
  };
  artifact?: { kind: string; ref?: string; size?: number };
  // An interactive session waiting for a human, and going on again.
  awaiting_reply?: { state: 'waiting' | 'resumed' | string; question?: string; message_id?: string };
  // A human's message, as the session took it in and the hub recorded
  // it: who sent it is the hub's word, not the node's.
  message?: MessageBody;
}

export interface Attachment {
  name: string;
  ref: string;
  media_type?: string;
  size?: number;
}

export interface MessageBody {
  id: string;
  text?: string;
  attachments?: Attachment[];
  sender?: string;
  sent_at?: string;
}

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
  }
}

async function check(r: Response): Promise<Response> {
  if (!r.ok) {
    const text = (await r.text().catch(() => '')).trim();
    throw new ApiError(r.status, text || `${r.status} ${r.statusText}`);
  }
  return r;
}

// write sends one state-changing request, with body as JSON when given,
// and answers the response's JSON, or undefined for an empty one.
async function write<T>(method: 'POST' | 'PUT' | 'DELETE', path: string, body?: unknown): Promise<T> {
  const init: RequestInit = { method, credentials: 'same-origin', headers: { Accept: 'application/json' } };
  if (body !== undefined) {
    init.headers = { Accept: 'application/json', 'Content-Type': 'application/json' };
    init.body = JSON.stringify(body);
  }
  const r = await check(await fetch(path, init));
  const text = await r.text();
  return (text ? JSON.parse(text) : undefined) as T;
}

async function getJSON<T>(path: string, signal?: AbortSignal): Promise<T> {
  const r = await fetch(path, { credentials: 'same-origin', headers: { Accept: 'application/json' }, signal });
  return (await check(r)).json() as Promise<T>;
}

export function getMe(signal?: AbortSignal): Promise<Me> {
  return getJSON<Me>('/v1/me', signal);
}

export async function listTasks(opts: { team?: string; states?: TaskState[]; limit?: number }, signal?: AbortSignal): Promise<Task[]> {
  const q = new URLSearchParams();
  if (opts.team) q.set('team', opts.team);
  if (opts.states?.length) q.set('state', opts.states.join(','));
  if (opts.limit) q.set('limit', String(opts.limit));
  const s = q.toString();
  const r = await getJSON<{ tasks: Task[] }>(`/v1/tasks${s ? `?${s}` : ''}`, signal);
  return r.tasks ?? [];
}

export function getTask(id: string, signal?: AbortSignal): Promise<Task> {
  return getJSON<Task>(`/v1/tasks/${encodeURIComponent(id)}`, signal);
}

export async function listNodes(signal?: AbortSignal): Promise<NodeEntry[]> {
  const r = await getJSON<{ nodes: NodeEntry[] }>('/v1/nodes', signal);
  return r.nodes ?? [];
}

// Filing, cancelling and requeueing (docs/hub.md §6.1).

// NewTask is what POST /v1/tasks takes: exactly one of repo and team.
export interface NewTask {
  repo?: string;
  team?: string;
  kind?: TaskKind;
  title: string;
  prompt: string;
  constraints?: { labels?: Record<string, string>; node?: string; node_owner?: string };
}

export function submitTask(t: NewTask): Promise<Task> {
  return write<Task>('POST', '/v1/tasks', t);
}

export function cancelTask(id: string, reason?: string): Promise<Task> {
  return write<Task>('POST', `/v1/tasks/${encodeURIComponent(id)}/cancel`, reason ? { reason } : undefined);
}

export function requeueTask(id: string): Promise<Task> {
  return write<Task>('POST', `/v1/tasks/${encodeURIComponent(id)}/requeue`);
}

// Proposals and approvals (docs/hub.md §6.1): a member of a proposed
// task's team approves or rejects it, and answers a running task's
// approval requests.

export function approveTask(id: string): Promise<Task> {
  return write<Task>('POST', `/v1/tasks/${encodeURIComponent(id)}/approve`);
}

export function rejectTask(id: string, reason?: string): Promise<Task> {
  return write<Task>('POST', `/v1/tasks/${encodeURIComponent(id)}/reject`, reason ? { reason } : undefined);
}

export type Decision = 'allow' | 'deny';

// Approval is a human's answer to one of a task's approval requests.
export interface Approval {
  task: string;
  request_id: string;
  decision: Decision;
  answered_by: string;
  reason?: string;
  answered_at: string;
  delivered: boolean;
}

export async function listApprovals(id: string, signal?: AbortSignal): Promise<Approval[]> {
  const r = await getJSON<{ approvals: Approval[] }>(`/v1/tasks/${encodeURIComponent(id)}/approvals`, signal);
  return r.approvals ?? [];
}

export function answerApproval(id: string, request: string, decision: Decision, reason?: string): Promise<Approval> {
  return write<Approval>('POST', `/v1/tasks/${encodeURIComponent(id)}/approvals/${encodeURIComponent(request)}`, reason ? { decision, reason } : { decision });
}

// The team directory (docs/hub.md §5.3).

// Team is a team the caller may see, with its members (principals,
// user:EMAIL or group:NAME) and the repositories it owns.
export interface Team {
  id: string;
  name: string;
  members: Record<string, Role> | null;
  repos: string[] | null;
}

export interface TeamSettings {
  lost_requeue_after_seconds?: number;
}

export async function listTeams(signal?: AbortSignal): Promise<Team[]> {
  const r = await getJSON<{ teams: Team[] }>('/v1/teams', signal);
  return r.teams ?? [];
}

const teamPath = (team: string) => `/v1/teams/${encodeURIComponent(team)}`;

// repoPath keeps the slash between a repository's owner and name: the
// API's route is /repos/{owner}/{name}.
const repoPath = (team: string, repo: string) => `${teamPath(team)}/repos/${repo.split('/').map(encodeURIComponent).join('/')}`;

export function getTeam(team: string, signal?: AbortSignal): Promise<Team> {
  return getJSON<Team>(teamPath(team), signal);
}

export function createTeam(id: string, name: string): Promise<void> {
  return write('POST', '/v1/teams', { id, name });
}

export function deleteTeam(team: string): Promise<void> {
  return write('DELETE', teamPath(team));
}

export function setMember(team: string, principal: string, role: Role): Promise<void> {
  return write('PUT', `${teamPath(team)}/members/${encodeURIComponent(principal)}`, { role });
}

export function removeMember(team: string, principal: string): Promise<void> {
  return write('DELETE', `${teamPath(team)}/members/${encodeURIComponent(principal)}`);
}

export function assignRepo(team: string, repo: string, history: boolean): Promise<void> {
  return write('PUT', repoPath(team, repo), history ? { history } : undefined);
}

export function releaseRepo(team: string, repo: string): Promise<void> {
  return write('DELETE', repoPath(team, repo));
}

export function getTeamSettings(team: string, signal?: AbortSignal): Promise<TeamSettings> {
  return getJSON<TeamSettings>(`${teamPath(team)}/settings`, signal);
}

export function setTeamSettings(team: string, s: TeamSettings): Promise<TeamSettings> {
  return write<TeamSettings>('PUT', `${teamPath(team)}/settings`, s);
}

// Node tokens (docs/hub.md §5.2).

export interface NodeToken {
  id: string;
  owner: string;
  node: string;
  created: string;
  revoked?: string;
  released?: string;
}

// MintedToken is POST /v1/node-tokens's answer: the token itself, which
// the hub keeps only a hash of, and its record.
export interface MintedToken {
  token: string;
  node_token: NodeToken;
}

export async function listNodeTokens(signal?: AbortSignal): Promise<NodeToken[]> {
  const r = await getJSON<{ node_tokens: NodeToken[] }>('/v1/node-tokens', signal);
  return r.node_tokens ?? [];
}

// mintNodeToken's answer is uncached (the hub says no-store, and the
// fetch asks for none) and is the caller's to show once and drop.
export async function mintNodeToken(node: string): Promise<MintedToken> {
  const r = await check(
    await fetch('/v1/node-tokens', {
      method: 'POST',
      credentials: 'same-origin',
      cache: 'no-store',
      headers: { Accept: 'application/json', 'Content-Type': 'application/json' },
      body: JSON.stringify({ node }),
    }),
  );
  return r.json() as Promise<MintedToken>;
}

export function revokeNodeToken(id: string): Promise<void> {
  return write('DELETE', `/v1/node-tokens/${encodeURIComponent(id)}`);
}

// ArtifactResult is a view of a node-only artifact: its text, held only
// in memory by whoever asked, or why there is none.
export type ArtifactResult =
  | { kind: 'text'; text: string }
  | { kind: 'unavailable'; message: string }
  | { kind: 'error'; status: number; message: string };

// fetchArtifact reads an artifact through the hub's view-through
// (GET /v1/tasks/{id}/artifacts?ref=) from the node that holds it. It
// is never cached by the browser (cache: 'no-store', as the hub's
// Cache-Control says) and never written anywhere: the caller keeps the
// text in component state and drops it with the component. A node that
// is offline answers 503, and then there is no copy to show.
export async function fetchArtifact(task: string, ref: string, signal?: AbortSignal): Promise<ArtifactResult> {
  const r = await fetch(`/v1/tasks/${encodeURIComponent(task)}/artifacts?ref=${encodeURIComponent(ref)}`, {
    credentials: 'same-origin',
    cache: 'no-store',
    signal,
  });
  const text = await r.text();
  if (r.status === 503) return { kind: 'unavailable', message: text.trim() || 'the node that holds it is offline' };
  if (!r.ok) return { kind: 'error', status: r.status, message: text.trim() || r.statusText };
  return { kind: 'text', text };
}

// Sessions (docs/hub.md §6.5): the submitter of an interactive task and
// the members of its team send it messages, which the hub keeps and
// sends down to its node.

// TaskMessage is a message as the hub keeps it.
export interface TaskMessage {
  task: string;
  id: string;
  text: string;
  attachments: Attachment[];
  sender: string;
  sent_at: string;
  delivered: boolean;
}

export async function listMessages(id: string, signal?: AbortSignal): Promise<TaskMessage[]> {
  const r = await getJSON<{ messages: TaskMessage[] }>(`/v1/tasks/${encodeURIComponent(id)}/messages`, signal);
  return r.messages ?? [];
}

// sendMessage posts text to the session. messageID names the message:
// the same ID again is the same message retried, which the hub keeps
// and sends once.
export function sendMessage(id: string, messageID: string, text: string): Promise<TaskMessage> {
  return write<TaskMessage>('POST', `/v1/tasks/${encodeURIComponent(id)}/messages`, { id: messageID, text });
}
