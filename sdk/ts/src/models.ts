// The hub's public API's models (docs/hub.md §5-§7), as its JSON
// carries them. Fields the hub may add later are not an error: a
// client reads what it knows.

export type TaskState = 'proposed' | 'rejected' | 'queued' | 'offered' | 'running' | 'done' | 'failed' | 'cancelled' | 'lost';

export const TASK_STATES: TaskState[] = ['proposed', 'queued', 'offered', 'running', 'done', 'failed', 'cancelled', 'rejected', 'lost'];

export type TaskKind = 'interactive';

export interface Constraints {
  labels?: Record<string, string>;
  node?: string;
  node_owner?: string;
}

// Task is a task as GET /v1/tasks/{id} answers it (hub.md §6.1).
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
  // depends_on are the hub tasks it waits for (docs/hub.md §6.6).
  depends_on?: string[];
  // filed_by is the Grain that filed it on behalf of its submitter,
  // filed_by_owner that Grain's owner, and origin_task the hub task
  // whose run filed it: all absent for a task a human filed.
  filed_by?: string;
  filed_by_owner?: string;
  origin_task?: string;
  constraints?: Constraints;
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

// NewTask is what POST /v1/tasks takes: exactly one of repo and team.
export interface NewTask {
  repo?: string;
  team?: string;
  kind?: TaskKind;
  title: string;
  prompt: string;
  constraints?: Constraints;
  // depends_on: up to 16 hub task IDs this one waits for (hub.md §6.6).
  depends_on?: string[];
}

// TaskQuery narrows GET /v1/tasks: one team (every team the caller is
// in by default), some states, and at most limit tasks, newest first.
export interface TaskQuery {
  team?: string;
  states?: TaskState[];
  limit?: number;
}

export type Role = 'member' | 'admin' | string;

// Me is GET /v1/me: the caller, their teams and their role in each.
export interface Me {
  email: string;
  groups?: string[];
  hub_admin: boolean;
  teams: Record<string, Role> | null;
}

// NodeEntry is a node as the directory (GET /v1/nodes) lists it.
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

// The event types the hub relays: package events' types, and "artifact"
// for the reference it keeps of a node-only event.
export const EVENT_TYPES = [
  'status',
  'transcript',
  'tool_call',
  'usage',
  'approval_request',
  'resumable',
  'result',
  'awaiting_reply',
  'message',
  'artifact',
];

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

// TaskMessage is a message sent to an interactive task, as the hub
// keeps it (hub.md §6.5).
export interface TaskMessage {
  task: string;
  id: string;
  text: string;
  attachments: Attachment[];
  sender: string;
  sent_at: string;
  delivered: boolean;
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

// NodeToken is a node token's record; the hub keeps only a hash of the
// token itself (hub.md §5.2).
export interface NodeToken {
  id: string;
  owner: string;
  node: string;
  created: string;
  revoked?: string;
  released?: string;
}

// MintedToken is POST /v1/node-tokens's answer: the token itself and
// its record.
export interface MintedToken {
  token: string;
  node_token: NodeToken;
}

// ArtifactResult is a view of a node-only artifact: its text, held only
// in memory by whoever asked, or why there is none.
export type ArtifactResult =
  | { kind: 'text'; text: string }
  | { kind: 'unavailable'; message: string }
  | { kind: 'error'; status: number; message: string };
