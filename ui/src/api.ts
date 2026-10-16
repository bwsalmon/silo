// The hub's public API, as the UI uses it (docs/hub.md §5-§7). The UI
// has no other: no private endpoint, and no decision of its own about
// who may see what. A task the caller may not see answers 404 here as
// anywhere.
//
// Every request is same-origin and carries only the browser's own
// credentials (IAP's cookie, or the OIDC session's): no token is read,
// stored or forwarded.

export type TaskState = 'queued' | 'offered' | 'running' | 'done' | 'failed' | 'cancelled' | 'lost';

export const TASK_STATES: TaskState[] = ['queued', 'offered', 'running', 'done', 'failed', 'cancelled', 'lost'];

export interface Task {
  id: string;
  team: string;
  repo?: string;
  title: string;
  prompt: string;
  submitter: string;
  approved_by?: string;
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
