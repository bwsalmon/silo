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

export type Role = 'admin' | 'member' | 'proposer' | 'viewer' | (string & {});

export type ViewerRole = 'owner' | 'admin' | 'member' | 'proposer' | 'viewer' | (string & {});

export type ResolvedTheme = 'light' | 'dark';

// Me is GET /v1/me: the caller, their teams and their role in each.
export interface Me {
  email: string;
  groups?: string[];
  hub_admin: boolean;
  can_hub_admin?: boolean;
  teams: Record<string, Role> | null;
  theme?: ResolvedTheme;
  inbox_nodes?: string[];
  inbox_configured?: boolean;
}

export type InboxKind = 'awaiting_reply' | 'approval_request' | 'proposal' | 'failed' | 'chat_mention';

export const INBOX_KINDS: InboxKind[] = [
  'awaiting_reply',
  'approval_request',
  'proposal',
  'failed',
  'chat_mention',
];

export interface InboxPreferences {
  nodes: string[];
  configured: boolean;
}

export interface InboxItem {
  id: string;
  kind: InboxKind;
  task_id: string;
  chat_id?: string;
  node_id?: string;
  node_ui_url?: string;
  team: string;
  repo?: string;
  task_kind?: string;
  title: string;
  state: TaskState;
  summary?: string;
  question?: string;
  reason?: string;
  submitter?: string;
  proposer?: string;
  approval_id?: string;
  tool?: string;
  viewer_role?: ViewerRole;
  can_act?: boolean;
  updated_at: string;
}

export interface InboxQuery {
  nodes?: string[];
  kind?: InboxKind | InboxKind[];
}

// NodeUI describes a node's tunneled local Grain web UI status on GET /v1/nodes.
export interface NodeUI {
  offered?: boolean;
  url?: string;
  team?: string;
  access?: string;
}

// NodeEntry is a node as the directory (GET /v1/nodes) lists it.
export interface NodeEntry {
  id: string;
  url?: string;
  recipient_url?: string;
  repos: string[];
  slots: number;
  version?: string;
  labels?: Record<string, string>;
  features?: string[];
  recipients?: string[];
  folders?: string[];
  grainfiles?: string[];
  ui?: NodeUI;
  ui_url?: string;
  viewer_role?: ViewerRole;
  require_approval?: boolean;
  owner: string;
  status: 'online' | 'stale' | 'offline' | string;
  free: number;
  claimed: number;
  connected_at: string;
  last_seen: string;
}

export type GrainfileKind = 'task' | 'process' | 'capability';

export const GRAINFILE_KINDS: GrainfileKind[] = ['task', 'process', 'capability'];

export interface ScheduleTrigger {
  cron: string;
  timezone?: string;
  schedules?: ScheduleTrigger[];
}

export type GrainfileScheduleTrigger = ScheduleTrigger;

export type GrainfileWorkflowEvent = 'triggered' | 'succeeded' | 'failed';

export interface GrainfileWorkflowFilter {
  workflow?: string;
  workflows?: string[];
  events?: GrainfileWorkflowEvent[];
  repo?: string;
  repos?: string[];
  branches?: string[];
}

export type WorkflowFilter = GrainfileWorkflowFilter;

export type GrainfileTriggerSource =
  | 'schedule'
  | 'push'
  | 'merge'
  | 'pull_request'
  | 'workflow'
  | 'manual'
  | 'scenario'
  | 'depends_on'
  | (string & {});

export const GRAINFILE_TRIGGER_SOURCES: GrainfileTriggerSource[] = [
  'schedule',
  'push',
  'merge',
  'pull_request',
  'workflow',
  'manual',
  'scenario',
  'depends_on',
];

export interface EventFilter {
  repo?: string;
  repos?: string[];
  branches?: string[];
  paths?: string[];
  ignore?: string[];
  types?: string[];
  action?: string;
  base?: string;
  filters?: EventFilter[];
}

export type EventFilterOrList = EventFilter | (EventFilter[] & Partial<EventFilter>);

export interface ManualTrigger {
  inputs?: Record<string, string>;
}

export interface ScenarioTrigger {
  description?: string;
  ref?: string;
  repo?: string;
  path?: string;
  ref_pin?: string;
}

export interface GrainfileTriggers {
  schedule?: GrainfileScheduleTrigger | GrainfileScheduleTrigger[];
  push?: EventFilterOrList;
  merge?: EventFilterOrList;
  pull_request?: EventFilterOrList;
  workflow?: GrainfileWorkflowFilter | GrainfileWorkflowFilter[];
  workflow_run?: EventFilterOrList;
  release?: EventFilterOrList;
  manual?: ManualTrigger;
  scenario?: ScenarioTrigger | ScenarioTrigger[];
}

// Grainfile is an indexed .task.grain.md or .process.grain.md metadata record from GET /v1/grainfiles (hub.md §6.3).
export interface Grainfile {
  grainfile?: string;
  spec_summary_path?: string;
  spec_path?: string;
  repo: string;
  path: string;
  commit_sha?: string;
  sha?: string;
  ref?: string;
  url: string;
  kind: GrainfileKind;
  title: string;
  is_template?: boolean;
  template_vars?: string[];
  from_template?: string;
  recipient?: string;
  requestor?: string;
  draft?: boolean;
  not_before?: string;
  created_at?: string;
  references?: string[];
  authorized_references?: string[];
  depends_on?: string[];
  capabilities?: string[];
  when?: GrainfileTriggers;
  on?: GrainfileTriggers;
  on_spec?: GrainfileTriggers;
  gate?: boolean;
  subscribed_nodes?: string[];
  recent_events?: GrainfileEvent[];
  updated_at?: string;
}

// GrainfileSubscriptions is a node's registered Grainfile subscription record on GET /v1/grainfiles (hub.md §6.2).
export interface GrainfileSubscriptions {
  node_id: string;
  owner?: string;
  recipients?: string[];
  repos?: string[];
  folders?: string[];
  grainfiles?: string[];
  cursor?: number;
  revision?: number;
  updated_at?: string;
}

export type GrainfileEventType =
  | 'task_ready'
  | 'task_removed'
  | 'process_updated'
  | 'process_triggered'
  | 'process_removed'
  | 'capability_updated'
  | 'capability_removed'
  | 'dependency_completed'
  | (string & {});

export const GRAINFILE_EVENT_TYPES: GrainfileEventType[] = [
  'task_ready',
  'task_removed',
  'process_updated',
  'process_triggered',
  'process_removed',
  'capability_updated',
  'capability_removed',
  'dependency_completed',
];

// GrainfileEvent is a discoverable Grainfile trigger or execution event on GET /v1/grainfiles (hub.md §6.4, §6.5).
export interface GrainfileEvent {
  seq?: number;
  id?: string;
  type: GrainfileEventType;
  action?: string;
  grainfile?: string;
  spec_summary_path?: string;
  spec_path?: string;
  repo: string;
  path?: string;
  commit_sha?: string;
  ref?: string;
  grainfile_url: string;
  kind?: GrainfileKind;
  title?: string;
  is_template?: boolean;
  template_vars?: string[];
  from_template?: string;
  recipient?: string;
  requestor?: string;
  not_before?: string;
  references?: string[];
  authorized_references?: string[];
  depends_on?: string[];
  capabilities?: string[];
  when?: GrainfileTriggers;
  on?: GrainfileTriggers;
  on_spec?: GrainfileTriggers;
  gate?: boolean;
  trigger_source?: GrainfileTriggerSource;
  triggered_by?: string;
  trigger_repo?: string;
  trigger_commit_sha?: string;
  workflow_name?: string;
  workflow_path?: string;
  workflow_event?: GrainfileWorkflowEvent | string;
  workflow_url?: string;
  branch?: string;
  pr_number?: number;
  paths?: string[];
  changed_paths?: string[];
  dependency_url?: string;
  dependency_state?: string;
  reason?: string;
  created_at?: string;
}

// GrainfilesOverview is the full response of GET /v1/grainfiles.
export interface GrainfilesOverview {
  grainfiles: Grainfile[];
  subscriptions: GrainfileSubscriptions[];
  events: GrainfileEvent[];
}

// GrainfileInspection is the result of inspecting a single Grainfile's metadata, subscriptions, and recent trigger/execution events.
export interface GrainfileInspection {
  grainfile: Grainfile | null;
  events: GrainfileEvent[];
  subscriptions: GrainfileSubscriptions[];
}

// GrainfileQuery narrows GET /v1/grainfiles by kind, recipient, repo, draft state, gate, requestor, path, node, or Grainfile URL.
export interface GrainfileQuery {
  kind?: GrainfileKind;
  recipient?: string;
  repo?: string;
  draft?: boolean;
  include_drafts?: boolean;
  gate?: boolean;
  requestor?: string;
  path?: string;
  node?: string;
  url?: string;
}

// PutHostedGrainfileInput is the body of PUT /v1/grainfiles/personal/{path} or PUT /v1/grainfiles/team/{team}/{path}.
export interface PutHostedGrainfileInput {
  content: string;
  files?: Record<string, string>;
  commit_sha?: string;
  commit?: string;
}

// HostedGrainfileResponse is returned when authoring or updating a Hub-hosted Grainfile.
export interface HostedGrainfileResponse {
  ok: boolean;
  repo: string;
  path: string;
  commit?: string;
  indexed?: Grainfile;
  events?: GrainfileEvent[];
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
  'chat_updated',
  'task_proposed',
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

// AcceptedUser is a user on the Hub's accepted users allowlist (GET /v1/users, hub.md §5.4).
export interface AcceptedUser {
  email: string;
  sys_admin: boolean;
  added_by?: string;
  created_at: string;
}

// Team is a deployment/team the caller may see, with its canonical Hub URL,
// its members (principals, user:EMAIL or group:NAME) and the repositories it owns.
export interface Team {
  id: string;
  name: string;
  url?: string;
  recipient_url?: string;
  members: Record<string, Role> | null;
  repos: string[] | null;
}

// Deployment is the unified deployment record returned by GET /v1/deployments.
export type Deployment = Team;

export interface TeamSettings {
  lost_requeue_after_seconds?: number;
  anyone_may_propose?: boolean;
  require_approval?: boolean;
}

export type DeploymentSettings = TeamSettings;

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

// MintedToken is POST /v1/node-tokens's and POST /v1/deployments/{id}/token's answer:
// the token itself, its record, and any host-replacement summary.
export interface MintedToken {
  token: string;
  node_token: NodeToken;
  replaced_host?: boolean;
  reclaimed_tasks?: string[];
  replayed_events?: number;
}

// ArtifactResult is a view of a node-only artifact: its text, held only
// in memory by whoever asked, or why there is none.
export type ArtifactResult =
  | { kind: 'text'; text: string }
  | { kind: 'unavailable'; message: string }
  | { kind: 'error'; status: number; message: string };

export type ChatReplyMode = 'mention' | 'all';

export type ChatParticipantRole = 'participant' | 'viewer' | (string & {});

export interface ChatParticipant {
  email: string;
  role: ChatParticipantRole;
  source?: 'deployment' | 'invited' | (string & {});
  added_by?: string;
  added_at?: string;
}

export interface ChatExternalBinding {
  provider: string;
  external_id: string;
  thread_id?: string;
  metadata?: Record<string, string>;
}

export interface ChatMessage {
  seq: number;
  id: string;
  chat_id?: string;
  sender: string;
  role: 'user' | 'agent' | (string & {});
  text: string;
  attachments?: Attachment[];
  triggered_agent?: boolean;
  created_at: string;
}

export interface ChatSession {
  id: string;
  task_id: string;
  title: string;
  deployment: string;
  linked_deployments: string[];
  reply_mode: ChatReplyMode;
  participants: ChatParticipant[];
  repos: string[];
  references: string[];
  external_bindings: ChatExternalBinding[];
  state?: TaskState;
  node?: string;
  awaiting_reply: boolean;
  question?: string;
  pending_approvals?: { id: string; tool: string; message?: string; deadline?: string }[];
  proposed_tasks?: Task[];
  messages?: ChatMessage[];
  created_by: string;
  created_at: string;
  updated_at: string;
  last_agent_seq?: number;
}

export interface ChatEvent {
  seq: number;
  type: string;
  time: string;
  chat_id: string;
  task_id?: string;
  message?: ChatMessage;
  transcript?: { kind: string; text?: string };
  status?: { phase: string; step?: string; state: string; message?: string };
  awaiting_reply?: { state: 'waiting' | 'resumed' | string; question?: string; message_id?: string };
  approval_request?: { id: string; tool: string; message?: string; deadline?: string };
  result?: {
    exit: { code: number; reason: string; detail?: string; cli_error?: { kind: string; message?: string } };
    final_text?: string;
    errors?: string[];
  };
  proposed_task?: Task;
  chat?: ChatSession;
}

export interface ChatQuery {
  deployment?: string;
}

export interface CreateChatInput {
  title: string;
  deployment: string;
  linked_deployments?: string[];
  reply_mode?: ChatReplyMode;
  participants?: ChatParticipant[];
  repos?: string[];
  references?: string[];
  prompt?: string;
  external_bindings?: ChatExternalBinding[];
}

export interface UpdateChatInput {
  title?: string;
  reply_mode?: ChatReplyMode;
  linked_deployments?: string[];
  participants?: ChatParticipant[];
  repos?: string[];
  references?: string[];
  external_bindings?: ChatExternalBinding[];
}

export interface PostChatMessageInput {
  id?: string;
  text: string;
  attachments?: Attachment[];
  trigger_agent?: boolean;
}

export interface GoogleChatBindingInput {
  space_name: string;
  thread_name?: string;
  space_display_name?: string;
}

