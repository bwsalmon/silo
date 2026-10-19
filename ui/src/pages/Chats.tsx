// Shared Chat Sessions (/chats and /chats/:id): multi-user and multi-agent
// collaborative chat rooms backed by a primary deployment's standing
// interactive session over the Hub's rendezvous tunnel.
import { useEffect, useMemo, useState } from 'react';
import {
  Alert,
  Box,
  Button,
  Chip,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Divider,
  LinearProgress,
  Link as MuiLink,
  MenuItem,
  Paper,
  Select,
  Stack,
  TextField,
  Typography,
} from '@mui/material';
import {
  approveTask,
  createChat,
  getChat,
  listChats,
  listDeployments,
  postChatMessage,
  subscribeChatEvents,
  updateChat,
  type ChatEvent,
  type ChatExternalBinding,
  type ChatMessage,
  type ChatParticipant,
  type ChatParticipantRole,
  type ChatReplyMode,
  type ChatSession,
  type Me,
  type Task,
} from '../api';
import { useAction } from '../action';
import { ApprovalPrompt } from '../components/ApprovalPrompt';
import Markdown from '../components/Markdown';
import { StateChip } from '../components/StateChip';
import { useLoad } from '../load';
import { Link, navigate, routePath } from '../router';
import { mono } from '../theme';
import { when } from './Backlog';
import { safeHref } from './Nodes';

function parseList(raw: string): string[] {
  return raw
    .split(/[\n,]+/)
    .map((s) => s.trim())
    .filter(Boolean);
}

function mergeChatMessage(list: ChatMessage[], msg: ChatMessage): ChatMessage[] {
  if (list.some((m) => m.id === msg.id)) return list;
  return [...list, msg];
}

function mergeProposedTask(list: Task[], t: Task): Task[] {
  const idx = list.findIndex((x) => x.id === t.id);
  if (idx >= 0) {
    const next = [...list];
    next[idx] = t;
    return next;
  }
  return [t, ...list];
}

function ProposedTaskCard({
  task,
  canApprove,
  onChanged,
}: {
  task: Task;
  canApprove: boolean;
  onChanged: () => void;
}) {
  const approve = useAction(() => approveTask(task.id), onChanged);
  return (
    <Paper variant="outlined" sx={{ p: 1.5 }} data-testid={`proposed-task-${task.id}`}>
      <Stack spacing={0.75}>
        <Stack direction="row" spacing={1} sx={{ alignItems: 'center', justifyContent: 'space-between', flexWrap: 'wrap' }}>
          <Stack direction="row" spacing={1} sx={{ alignItems: 'center', flexWrap: 'wrap' }}>
            <StateChip state={task.state} />
            <Chip size="small" label={`@${task.team}`} sx={{ fontFamily: mono }} variant="outlined" />
            <Typography variant="body2" sx={{ fontWeight: 600 }}>
              <Link to={routePath({ page: 'task', id: task.id })}>{task.title || task.id}</Link>
            </Typography>
          </Stack>
          <Stack direction="row" spacing={1} sx={{ alignItems: 'center' }}>
            {task.state === 'proposed' && canApprove && (
              <Button
                size="small"
                variant="contained"
                disabled={approve.busy}
                onClick={() => void approve.run()}
                data-testid={`approve-proposed-task-${task.id}`}
              >
                Approve
              </Button>
            )}
            <Button
              size="small"
              variant="outlined"
              onClick={() => navigate(routePath({ page: 'task', id: task.id }))}
            >
              View Task
            </Button>
          </Stack>
        </Stack>
        {approve.error && <Alert severity="error">{approve.error}</Alert>}
      </Stack>
    </Paper>
  );
}

export function Chats({
  id,
  me,
  onInboxChanged,
}: {
  id?: string;
  me?: Me;
  onInboxChanged?: () => void;
}) {
  const chatsLoad = useLoad((signal) => listChats({}, signal), [me?.email ?? ''], 10000);
  const deploymentsLoad = useLoad((signal) => listDeployments(signal), [me?.email ?? '']);

  const chats = chatsLoad.data ?? [];
  const selectedId = id ?? chats[0]?.id;

  const activeChatLoad = useLoad(
    (signal) => (selectedId ? getChat(selectedId, signal) : Promise.resolve(null)),
    [selectedId ?? '', me?.email ?? ''],
  );

  const [liveChat, setLiveChat] = useState<ChatSession | null>(null);
  const [liveMessages, setLiveMessages] = useState<ChatMessage[]>([]);
  const [liveProposedTasks, setLiveProposedTasks] = useState<Task[]>([]);
  const [showDrawer, setShowDrawer] = useState<boolean>(true);

  useEffect(() => {
    if (activeChatLoad.data) {
      setLiveChat(activeChatLoad.data);
      setLiveMessages(activeChatLoad.data.messages ?? []);
      setLiveProposedTasks(activeChatLoad.data.proposed_tasks ?? []);
    } else if (!selectedId) {
      setLiveChat(null);
      setLiveMessages([]);
      setLiveProposedTasks([]);
    }
  }, [activeChatLoad.data, selectedId]);

  useEffect(() => {
    if (!selectedId) return;
    const sub = subscribeChatEvents(selectedId, {
      onEvent: (ev: ChatEvent) => {
        if (ev.message) {
          setLiveMessages((prev) => mergeChatMessage(prev, ev.message!));
        }
        if (ev.proposed_task) {
          setLiveProposedTasks((prev) => mergeProposedTask(prev, ev.proposed_task!));
        }
        if (ev.chat) {
          setLiveChat(ev.chat);
        }
        if (ev.awaiting_reply) {
          const waiting = ev.awaiting_reply.state === 'waiting';
          const question = ev.awaiting_reply.question;
          setLiveChat((prev) =>
            prev
              ? {
                  ...prev,
                  awaiting_reply: waiting,
                  question: waiting ? question : undefined,
                }
              : prev,
          );
          onInboxChanged?.();
        }
        if (ev.approval_request) {
          setLiveChat((prev) => {
            if (!prev) return prev;
            const existing = prev.pending_approvals ?? [];
            if (existing.some((a) => a.id === ev.approval_request!.id)) return prev;
            return {
              ...prev,
              pending_approvals: [...existing, ev.approval_request!],
            };
          });
          onInboxChanged?.();
        }
      },
    });
    return () => sub.close();
  }, [selectedId]);

  // Composer state
  const [draft, setDraft] = useState('');
  const [sendError, setSendError] = useState<string | undefined>(undefined);
  const [sending, setSending] = useState(false);

  // New Shared Chat Dialog state
  const [newOpen, setNewOpen] = useState(false);
  const [newTitle, setNewTitle] = useState('');
  const [newDeployment, setNewDeployment] = useState('');
  const [newLinkedDeployments, setNewLinkedDeployments] = useState('');
  const [newReplyMode, setNewReplyMode] = useState<ChatReplyMode>('mention');
  const [newParticipantsRaw, setNewParticipantsRaw] = useState('');
  const [newReposRaw, setNewReposRaw] = useState('');
  const [newRefsRaw, setNewRefsRaw] = useState('');
  const [newGoogleChatSpace, setNewGoogleChatSpace] = useState('');
  const [newError, setNewError] = useState<string | undefined>(undefined);
  const [creating, setCreating] = useState(false);

  // Drawer form state for editing active chat
  const [inviteEmail, setInviteEmail] = useState('');
  const [inviteRole, setInviteRole] = useState<ChatParticipantRole>('participant');
  const [linkedDraft, setLinkedDraft] = useState('');
  const [reposDraft, setReposDraft] = useState('');
  const [refsDraft, setRefsDraft] = useState('');
  const [bindingSpace, setBindingSpace] = useState('');
  const [bindingThread, setBindingThread] = useState('');
  const [drawerError, setDrawerError] = useState<string | undefined>(undefined);
  const [savingSettings, setSavingSettings] = useState(false);

  useEffect(() => {
    if (!liveChat) return;
    setLinkedDraft((liveChat.linked_deployments ?? []).join(', '));
    setReposDraft((liveChat.repos ?? []).join(', '));
    setRefsDraft((liveChat.references ?? []).join(', '));
    const gchat = (liveChat.external_bindings ?? []).find((b) => b.provider === 'google_chat');
    setBindingSpace(gchat?.external_id ?? '');
    setBindingThread(gchat?.thread_id ?? '');
  }, [liveChat?.id, liveChat?.updated_at]);

  const availableDeployments = useMemo(() => {
    const fromApi = (deploymentsLoad.data ?? []).map((d) => d.id);
    const fromMe = Object.keys(me?.teams ?? {});
    return Array.from(new Set([...fromApi, ...fromMe])).sort();
  }, [deploymentsLoad.data, me?.teams]);

  useEffect(() => {
    if (!newDeployment && availableDeployments.length > 0) {
      setNewDeployment(availableDeployments[0]);
    }
  }, [availableDeployments, newDeployment]);

  const myRole = useMemo(() => {
    if (!liveChat || !me) return 'viewer';
    if (me.hub_admin) return 'admin';
    const teamRole = me.teams?.[liveChat.deployment];
    if (teamRole === 'admin' || teamRole === 'member') return teamRole;
    const p = (liveChat.participants ?? []).find((x) => x.email.toLowerCase() === me.email.toLowerCase());
    if (p) return p.role;
    return 'viewer';
  }, [liveChat, me]);

  const canPost = myRole === 'admin' || myRole === 'member' || myRole === 'participant';
  const canApproveTeam = Boolean(
    me?.hub_admin || (liveChat && (me?.teams?.[liveChat.deployment] === 'admin' || me?.teams?.[liveChat.deployment] === 'member')),
  );

  const handleSendMessage = async (triggerAgent: boolean) => {
    if (!liveChat || !draft.trim()) return;
    setSending(true);
    setSendError(undefined);
    try {
      const msg = await postChatMessage(liveChat.id, {
        id: `m-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`,
        text: draft.trim(),
        trigger_agent: triggerAgent,
      });
      setLiveMessages((prev) => mergeChatMessage(prev, msg));
      setDraft('');
      activeChatLoad.reload();
      chatsLoad.reload();
      onInboxChanged?.();
    } catch (e) {
      setSendError(e instanceof Error ? e.message : String(e));
    } finally {
      setSending(false);
    }
  };

  const handleCreateChat = async () => {
    if (!newDeployment.trim()) {
      setNewError('Primary deployment is required.');
      return;
    }
    setCreating(true);
    setNewError(undefined);
    try {
      const participants: ChatParticipant[] = parseList(newParticipantsRaw).map((entry) => {
        const [emailPart, rolePart] = entry.split(':').map((s) => s.trim());
        const role: ChatParticipantRole = rolePart === 'viewer' ? 'viewer' : 'participant';
        return { email: emailPart, role };
      });
      const external_bindings: ChatExternalBinding[] = newGoogleChatSpace.trim()
        ? [{ provider: 'google_chat', external_id: newGoogleChatSpace.trim() }]
        : [];
      const created = await createChat({
        title: newTitle.trim() || `${newDeployment.trim()} Shared Chat`,
        deployment: newDeployment.trim(),
        linked_deployments: parseList(newLinkedDeployments),
        reply_mode: newReplyMode,
        participants,
        repos: parseList(newReposRaw),
        references: parseList(newRefsRaw),
        external_bindings,
      });
      setNewOpen(false);
      setNewTitle('');
      setNewLinkedDeployments('');
      setNewParticipantsRaw('');
      setNewReposRaw('');
      setNewRefsRaw('');
      setNewGoogleChatSpace('');
      chatsLoad.reload();
      navigate(routePath({ page: 'chat', id: created.id }));
    } catch (e) {
      setNewError(e instanceof Error ? e.message : String(e));
    } finally {
      setCreating(false);
    }
  };

  const handleToggleReplyMode = async (mode: ChatReplyMode) => {
    if (!liveChat || liveChat.reply_mode === mode) return;
    setSavingSettings(true);
    setDrawerError(undefined);
    try {
      const updated = await updateChat(liveChat.id, { reply_mode: mode });
      setLiveChat(updated);
      chatsLoad.reload();
    } catch (e) {
      setDrawerError(e instanceof Error ? e.message : String(e));
    } finally {
      setSavingSettings(false);
    }
  };

  const handleAddParticipant = async () => {
    if (!liveChat || !inviteEmail.trim()) return;
    setSavingSettings(true);
    setDrawerError(undefined);
    try {
      const currentInvited = (liveChat.participants ?? [])
        .filter((p) => p.source === 'invited')
        .filter((p) => p.email.toLowerCase() !== inviteEmail.trim().toLowerCase());
      const updated = await updateChat(liveChat.id, {
        participants: [...currentInvited, { email: inviteEmail.trim(), role: inviteRole }],
      });
      setLiveChat(updated);
      setInviteEmail('');
      chatsLoad.reload();
    } catch (e) {
      setDrawerError(e instanceof Error ? e.message : String(e));
    } finally {
      setSavingSettings(false);
    }
  };

  const handleRemoveParticipant = async (email: string) => {
    if (!liveChat) return;
    setSavingSettings(true);
    setDrawerError(undefined);
    try {
      const remaining = (liveChat.participants ?? [])
        .filter((p) => p.source === 'invited' && p.email.toLowerCase() !== email.toLowerCase())
        .map((p) => ({ email: p.email, role: p.role }));
      const updated = await updateChat(liveChat.id, { participants: remaining });
      setLiveChat(updated);
      chatsLoad.reload();
    } catch (e) {
      setDrawerError(e instanceof Error ? e.message : String(e));
    } finally {
      setSavingSettings(false);
    }
  };

  const handleSaveContextAndBindings = async () => {
    if (!liveChat) return;
    setSavingSettings(true);
    setDrawerError(undefined);
    try {
      const otherBindings = (liveChat.external_bindings ?? []).filter((b) => b.provider !== 'google_chat');
      const external_bindings: ChatExternalBinding[] = bindingSpace.trim()
        ? [
            ...otherBindings,
            {
              provider: 'google_chat',
              external_id: bindingSpace.trim(),
              thread_id: bindingThread.trim() || undefined,
            },
          ]
        : otherBindings;
      const updated = await updateChat(liveChat.id, {
        linked_deployments: parseList(linkedDraft),
        repos: parseList(reposDraft),
        references: parseList(refsDraft),
        external_bindings,
      });
      setLiveChat(updated);
      chatsLoad.reload();
    } catch (e) {
      setDrawerError(e instanceof Error ? e.message : String(e));
    } finally {
      setSavingSettings(false);
    }
  };

  const insertMention = (handle: string) => {
    const token = `@${handle} `;
    setDraft((prev) => (prev.endsWith(' ') || prev.length === 0 ? `${prev}${token}` : `${prev} ${token}`));
  };

  return (
    <Stack spacing={2} data-testid="shared-chats-page">
      <Stack direction="row" sx={{ alignItems: 'center', justifyContent: 'space-between', flexWrap: 'wrap', gap: 1 }}>
        <Box>
          <Typography variant="h5">Shared Chat Sessions</Typography>
          <Typography variant="body2" color="text.secondary">
            Collaborate with teammates and Grain deployments in shared multi-user, multi-agent rooms.
          </Typography>
        </Box>
        <Button
          variant="contained"
          onClick={() => setNewOpen(true)}
          data-testid="new-shared-chat-button"
        >
          New Shared Chat
        </Button>
      </Stack>

      {chatsLoad.loading && !chatsLoad.data && <LinearProgress />}
      {chatsLoad.error && <Alert severity="error">{chatsLoad.error.message}</Alert>}

      <Stack direction={{ xs: 'column', md: 'row' }} spacing={2} sx={{ alignItems: 'stretch', minHeight: 560 }}>
        {/* Left Sidebar: Room List */}
        <Paper sx={{ width: { xs: '100%', md: 290 }, p: 1.5, flexShrink: 0 }} data-testid="chats-sidebar">
          <Stack spacing={1}>
            <Typography variant="subtitle2" color="text.secondary">
              Rooms ({chats.length})
            </Typography>
            {chats.length === 0 ? (
              <Typography variant="body2" color="text.secondary">
                No shared chat rooms yet. Create one to start collaborating.
              </Typography>
            ) : (
              chats.map((c) => {
                const selected = c.id === selectedId;
                return (
                  <Paper
                    key={c.id}
                    variant={selected ? 'elevation' : 'outlined'}
                    sx={{
                      p: 1.25,
                      cursor: 'pointer',
                      borderColor: selected ? 'primary.main' : undefined,
                      bgcolor: selected ? 'action.selected' : undefined,
                    }}
                    onClick={() => navigate(routePath({ page: 'chat', id: c.id }))}
                    data-testid={`chat-room-item-${c.id}`}
                  >
                    <Stack spacing={0.5}>
                      <Stack direction="row" spacing={1} sx={{ alignItems: 'center', justifyContent: 'space-between' }}>
                        <Typography variant="subtitle2" sx={{ fontWeight: 600 }}>
                          {c.title}
                        </Typography>
                        {c.awaiting_reply && (
                          <Chip size="small" color="warning" label="Awaiting reply" data-testid={`chat-awaiting-${c.id}`} />
                        )}
                      </Stack>
                      <Stack direction="row" spacing={0.75} sx={{ alignItems: 'center', flexWrap: 'wrap', rowGap: 0.5 }}>
                        <Chip size="small" label={`@${c.deployment}`} sx={{ fontFamily: mono }} variant="outlined" />
                        <Chip
                          size="small"
                          label={c.reply_mode === 'all' ? 'All messages' : '@mention'}
                          variant="outlined"
                        />
                        <Typography variant="caption" color="text.secondary">
                          {(c.participants ?? []).length} participants
                        </Typography>
                      </Stack>
                    </Stack>
                  </Paper>
                );
              })
            )}
          </Stack>
        </Paper>

        {/* Main Chat Pane */}
        <Paper sx={{ flex: 1, p: 2, display: 'flex', flexDirection: 'column', justifyContent: 'space-between' }} data-testid="chat-main-pane">
          {!liveChat ? (
            <Box sx={{ py: 6, textAlign: 'center' }}>
              <Typography variant="body1" color="text.secondary">
                Select a shared chat room on the left or create a new one.
              </Typography>
            </Box>
          ) : (
            <Stack spacing={2} sx={{ flex: 1, justifyContent: 'space-between' }}>
              {/* Room Header */}
              <Stack spacing={1}>
                <Stack direction="row" spacing={1} sx={{ alignItems: 'center', justifyContent: 'space-between', flexWrap: 'wrap' }}>
                  <Stack direction="row" spacing={1} sx={{ alignItems: 'center', flexWrap: 'wrap' }}>
                    <Typography variant="h6" data-testid="active-chat-title">
                      {liveChat.title}
                    </Typography>
                    <Chip
                      size="small"
                      color="primary"
                      label={`@${liveChat.deployment}`}
                      sx={{ fontFamily: mono }}
                      data-testid="active-chat-deployment"
                    />
                    {(liveChat.linked_deployments ?? []).map((ld) => (
                      <Chip
                        key={ld}
                        size="small"
                        variant="outlined"
                        label={`@${ld}`}
                        sx={{ fontFamily: mono }}
                      />
                    ))}
                    {liveChat.state && <StateChip state={liveChat.state} />}
                  </Stack>
                  <Stack direction="row" spacing={1} sx={{ alignItems: 'center' }}>
                    <Button
                      size="small"
                      variant={liveChat.reply_mode === 'mention' ? 'contained' : 'outlined'}
                      disabled={!canPost || savingSettings}
                      onClick={() => void handleToggleReplyMode('mention')}
                      data-testid="reply-mode-mention-btn"
                    >
                      @mention only
                    </Button>
                    <Button
                      size="small"
                      variant={liveChat.reply_mode === 'all' ? 'contained' : 'outlined'}
                      disabled={!canPost || savingSettings}
                      onClick={() => void handleToggleReplyMode('all')}
                      data-testid="reply-mode-all-btn"
                    >
                      All messages
                    </Button>
                    <Button
                      size="small"
                      variant="outlined"
                      onClick={() => setShowDrawer((v) => !v)}
                      data-testid="toggle-chat-drawer-btn"
                    >
                      {showDrawer ? 'Hide Context' : 'Participants & Context'}
                    </Button>
                  </Stack>
                </Stack>
                <Typography variant="caption" color="text.secondary">
                  Backing session:{' '}
                  <Link to={routePath({ page: 'task', id: liveChat.task_id })}>{liveChat.task_id}</Link>
                  {' · '}
                  {liveChat.reply_mode === 'mention'
                    ? `Agent wakes when mentioned (@${liveChat.deployment} or @agent) or when you click "Ask @${liveChat.deployment}".`
                    : `Agent wakes on every message posted in the room.`}
                </Typography>
                <Divider />
              </Stack>

              {/* Pending in-run approvals */}
              {(liveChat.pending_approvals ?? []).length > 0 && (
                <Stack spacing={1}>
                  {(liveChat.pending_approvals ?? []).map((ar) => (
                    <Paper key={ar.id} variant="outlined" sx={{ p: 1.5 }}>
                      <Stack spacing={1}>
                        <Typography variant="subtitle2" sx={{ fontFamily: mono }}>
                          Approval requested: {ar.tool} {ar.message ? `— ${ar.message}` : ''}
                        </Typography>
                        <ApprovalPrompt
                          task={liveChat.task_id}
                          request={ar.id}
                          mayAnswer={canApproveTeam}
                          onDone={() => {
                            activeChatLoad.reload();
                            onInboxChanged?.();
                          }}
                        />
                      </Stack>
                    </Paper>
                  ))}
                </Stack>
              )}

              {/* Proposed Tasks from this Chat Room */}
              {liveProposedTasks.length > 0 && (
                <Stack spacing={1} data-testid="chat-proposed-tasks">
                  <Typography variant="subtitle2" color="text.secondary">
                    Tasks Proposed from Room ({liveProposedTasks.length})
                  </Typography>
                  {liveProposedTasks.map((pt) => (
                    <ProposedTaskCard
                      key={pt.id}
                      task={pt}
                      canApprove={Boolean(
                        me?.hub_admin || me?.teams?.[pt.team] === 'admin' || me?.teams?.[pt.team] === 'member',
                      )}
                      onChanged={() => {
                        activeChatLoad.reload();
                        onInboxChanged?.();
                      }}
                    />
                  ))}
                </Stack>
              )}

              {/* Message Transcript */}
              <Stack spacing={1.5} sx={{ flex: 1, overflowY: 'auto', py: 1 }} data-testid="chat-transcript">
                {liveMessages.length === 0 ? (
                  <Typography variant="body2" color="text.secondary">
                    No messages yet. Start the discussion below or ask @{liveChat.deployment}.
                  </Typography>
                ) : (
                  liveMessages.map((m) => {
                    const isAgent = m.role === 'agent';
                    return (
                      <Paper
                        key={m.id}
                        variant="outlined"
                        sx={{
                          p: 1.5,
                          borderLeft: 3,
                          borderColor: isAgent ? 'primary.main' : 'divider',
                        }}
                        data-testid={`chat-message-${m.id}`}
                      >
                        <Stack spacing={0.5}>
                          <Stack direction="row" spacing={1} sx={{ alignItems: 'center', justifyContent: 'space-between', flexWrap: 'wrap' }}>
                            <Stack direction="row" spacing={1} sx={{ alignItems: 'center' }}>
                              <Chip
                                size="small"
                                color={isAgent ? 'primary' : 'default'}
                                label={isAgent ? `@${m.sender}` : m.sender}
                                sx={{ fontFamily: isAgent ? mono : undefined, fontWeight: 600 }}
                              />
                              {m.triggered_agent && !isAgent && (
                                <Chip size="small" variant="outlined" color="info" label={`Woke @${liveChat.deployment}`} />
                              )}
                            </Stack>
                            <Typography variant="caption" color="text.secondary">
                              {when(m.created_at)}
                            </Typography>
                          </Stack>
                          <Box sx={{ pt: 0.5 }}>
                            <Markdown>{m.text}</Markdown>
                          </Box>
                        </Stack>
                      </Paper>
                    );
                  })
                )}
              </Stack>

              {/* Composer */}
              <Stack spacing={1} sx={{ pt: 1 }}>
                <Stack direction="row" spacing={0.75} sx={{ alignItems: 'center', flexWrap: 'wrap', rowGap: 0.5 }}>
                  <Typography variant="caption" color="text.secondary">
                    Quick @mention:
                  </Typography>
                  <Chip
                    size="small"
                    label={`@${liveChat.deployment}`}
                    sx={{ fontFamily: mono }}
                    onClick={() => insertMention(liveChat.deployment)}
                    data-testid={`mention-pill-${liveChat.deployment}`}
                  />
                  {(liveChat.linked_deployments ?? []).map((ld) => (
                    <Chip
                      key={ld}
                      size="small"
                      variant="outlined"
                      label={`@${ld}`}
                      sx={{ fontFamily: mono }}
                      onClick={() => insertMention(ld)}
                      data-testid={`mention-pill-${ld}`}
                    />
                  ))}
                  {(liveChat.participants ?? []).map((p) => (
                    <Chip
                      key={p.email}
                      size="small"
                      variant="outlined"
                      label={`@${p.email}`}
                      onClick={() => insertMention(p.email)}
                    />
                  ))}
                </Stack>

                {!canPost ? (
                  <Alert severity="info">You are a viewer in this shared chat room.</Alert>
                ) : (
                  <Stack spacing={1}>
                    <TextField
                      multiline
                      minRows={2}
                      maxRows={8}
                      size="small"
                      fullWidth
                      placeholder={
                        liveChat.reply_mode === 'mention'
                          ? `Message teammates freely, or mention @${liveChat.deployment} to wake the agent…`
                          : `Send a message to the room and @${liveChat.deployment}…`
                      }
                      value={draft}
                      onChange={(e) => setDraft(e.target.value)}
                      inputProps={{ 'aria-label': 'Chat message', 'data-testid': 'chat-message-input' }}
                    />
                    <Stack direction="row" spacing={1} sx={{ justifyContent: 'flex-end' }}>
                      <Button
                        variant="outlined"
                        disabled={sending || !draft.trim()}
                        onClick={() => void handleSendMessage(false)}
                        data-testid="send-chat-message-btn"
                      >
                        Send Message
                      </Button>
                      <Button
                        variant="contained"
                        disabled={sending || !draft.trim()}
                        onClick={() => void handleSendMessage(true)}
                        data-testid="ask-agent-btn"
                      >
                        {`Ask @${liveChat.deployment}`}
                      </Button>
                    </Stack>
                  </Stack>
                )}
                {sendError && <Alert severity="error">{sendError}</Alert>}
              </Stack>
            </Stack>
          )}
        </Paper>

        {/* Right Drawer / Panel: Participants, Context & External Chat Bindings */}
        {liveChat && showDrawer && (
          <Paper sx={{ width: { xs: '100%', md: 330 }, p: 2, flexShrink: 0 }} data-testid="chat-context-drawer">
            <Stack spacing={2}>
              <Typography variant="subtitle1" sx={{ fontWeight: 600 }}>
                Participants & Context
              </Typography>

              {drawerError && <Alert severity="error">{drawerError}</Alert>}

              {/* Participants list */}
              <Stack spacing={1}>
                <Typography variant="subtitle2" color="text.secondary">
                  Participants ({(liveChat.participants ?? []).length})
                </Typography>
                {(liveChat.participants ?? []).map((p) => (
                  <Stack
                    key={p.email}
                    direction="row"
                    spacing={1}
                    sx={{ alignItems: 'center', justifyContent: 'space-between' }}
                    data-testid={`participant-row-${p.email}`}
                  >
                    <Box sx={{ minWidth: 0 }}>
                      <Typography variant="body2" sx={{ wordBreak: 'break-all' }}>
                        {p.email}
                      </Typography>
                      <Typography variant="caption" color="text.secondary">
                        {p.role} ({p.source ?? 'invited'})
                      </Typography>
                    </Box>
                    {canPost && p.source === 'invited' && (
                      <Button
                        size="small"
                        color="error"
                        disabled={savingSettings}
                        onClick={() => void handleRemoveParticipant(p.email)}
                      >
                        Remove
                      </Button>
                    )}
                  </Stack>
                ))}

                {canPost && (
                  <Stack spacing={1} sx={{ pt: 1 }}>
                    <TextField
                      size="small"
                      label="Invite accepted Hub user (email)"
                      value={inviteEmail}
                      onChange={(e) => setInviteEmail(e.target.value)}
                      inputProps={{ 'data-testid': 'invite-participant-email' }}
                    />
                    <Stack direction="row" spacing={1}>
                      <Select
                        size="small"
                        value={inviteRole}
                        onChange={(e) => setInviteRole(e.target.value as ChatParticipantRole)}
                        sx={{ flex: 1 }}
                        inputProps={{ 'aria-label': 'Participant role' }}
                      >
                        <MenuItem value="participant">participant</MenuItem>
                        <MenuItem value="viewer">viewer</MenuItem>
                      </Select>
                      <Button
                        size="small"
                        variant="outlined"
                        disabled={savingSettings || !inviteEmail.trim()}
                        onClick={() => void handleAddParticipant()}
                        data-testid="invite-participant-btn"
                      >
                        Add
                      </Button>
                    </Stack>
                  </Stack>
                )}
              </Stack>

              <Divider />

              {/* Attached Context & Linked Deployments */}
              <Stack spacing={1}>
                <Typography variant="subtitle2" color="text.secondary">
                  Linked Deployments & Context
                </Typography>
                <TextField
                  size="small"
                  label="Linked deployments (comma-separated)"
                  value={linkedDraft}
                  disabled={!canPost}
                  onChange={(e) => setLinkedDraft(e.target.value)}
                  inputProps={{ 'data-testid': 'drawer-linked-deployments' }}
                />
                <TextField
                  size="small"
                  label="Repositories (owner/name)"
                  value={reposDraft}
                  disabled={!canPost}
                  onChange={(e) => setReposDraft(e.target.value)}
                  inputProps={{ 'data-testid': 'drawer-repos' }}
                />
                <TextField
                  size="small"
                  label="Reference URLs"
                  value={refsDraft}
                  disabled={!canPost}
                  onChange={(e) => setRefsDraft(e.target.value)}
                  inputProps={{ 'data-testid': 'drawer-references' }}
                />
                {(liveChat.references ?? []).map((refUrl) => {
                  const href = safeHref(refUrl);
                  return href ? (
                    <MuiLink
                      key={refUrl}
                      href={href}
                      target="_blank"
                      rel="noopener noreferrer"
                      variant="caption"
                    >
                      {refUrl}
                    </MuiLink>
                  ) : null;
                })}
              </Stack>

              <Divider />

              {/* External Chat Integration Slot */}
              <Stack spacing={1} data-testid="external-chat-bindings-section">
                <Typography variant="subtitle2" color="text.secondary">
                  External Chat Integration (Google Chat)
                </Typography>
                <TextField
                  size="small"
                  label="Google Chat Space ID (e.g. spaces/AAA...)"
                  value={bindingSpace}
                  disabled={!canPost}
                  onChange={(e) => setBindingSpace(e.target.value)}
                  inputProps={{ 'data-testid': 'drawer-google-chat-space' }}
                />
                <TextField
                  size="small"
                  label="Thread ID (optional)"
                  value={bindingThread}
                  disabled={!canPost}
                  onChange={(e) => setBindingThread(e.target.value)}
                  inputProps={{ 'data-testid': 'drawer-google-chat-thread' }}
                />
                {canPost && (
                  <Button
                    size="small"
                    variant="contained"
                    disabled={savingSettings}
                    onClick={() => void handleSaveContextAndBindings()}
                    data-testid="save-chat-settings-btn"
                  >
                    Save Context & Bindings
                  </Button>
                )}
              </Stack>
            </Stack>
          </Paper>
        )}
      </Stack>

      {/* New Shared Chat Modal */}
      <Dialog open={newOpen} onClose={() => setNewOpen(false)} fullWidth maxWidth="sm">
        <DialogTitle>New Shared Chat</DialogTitle>
        <DialogContent>
          <Stack spacing={2} sx={{ mt: 1 }}>
            {newError && <Alert severity="error">{newError}</Alert>}
            <TextField
              label="Room Title"
              placeholder="e.g. Payments Ops Room"
              value={newTitle}
              onChange={(e) => setNewTitle(e.target.value)}
              fullWidth
              size="small"
              inputProps={{ 'data-testid': 'new-chat-title' }}
            />
            <TextField
              label="Primary Backing Deployment"
              placeholder="e.g. ops-agent"
              value={newDeployment}
              onChange={(e) => setNewDeployment(e.target.value)}
              fullWidth
              size="small"
              helperText={
                availableDeployments.length > 0
                  ? `Available deployments: ${availableDeployments.join(', ')}`
                  : 'Enter the primary Grain deployment ID'
              }
              inputProps={{ 'data-testid': 'new-chat-deployment' }}
            />
            <TextField
              label="Linked Deployments (optional, comma-separated)"
              placeholder="e.g. design-agent, checkout-agent"
              value={newLinkedDeployments}
              onChange={(e) => setNewLinkedDeployments(e.target.value)}
              fullWidth
              size="small"
              inputProps={{ 'data-testid': 'new-chat-linked-deployments' }}
            />
            <Select
              size="small"
              value={newReplyMode}
              onChange={(e) => setNewReplyMode(e.target.value as ChatReplyMode)}
              inputProps={{ 'aria-label': 'Reply mode', 'data-testid': 'new-chat-reply-mode' }}
            >
              <MenuItem value="mention">@mention only (wake agent on @mention or Ask button)</MenuItem>
              <MenuItem value="all">All messages (wake agent on every message)</MenuItem>
            </Select>
            <TextField
              label="Invite Participants (optional, comma-separated emails)"
              placeholder="e.g. pm@example.com, viewer@example.com:viewer"
              value={newParticipantsRaw}
              onChange={(e) => setNewParticipantsRaw(e.target.value)}
              fullWidth
              size="small"
              inputProps={{ 'data-testid': 'new-chat-participants' }}
            />
            <TextField
              label="Attached Repositories (optional, comma-separated owner/name)"
              placeholder="e.g. bwsalmon/silo"
              value={newReposRaw}
              onChange={(e) => setNewReposRaw(e.target.value)}
              fullWidth
              size="small"
              inputProps={{ 'data-testid': 'new-chat-repos' }}
            />
            <TextField
              label="Reference URLs (optional, comma-separated https://...)"
              placeholder="e.g. https://docs.example.com/spec"
              value={newRefsRaw}
              onChange={(e) => setNewRefsRaw(e.target.value)}
              fullWidth
              size="small"
              inputProps={{ 'data-testid': 'new-chat-references' }}
            />
            <TextField
              label="Google Chat Space Binding (optional)"
              placeholder="e.g. spaces/AAA..."
              value={newGoogleChatSpace}
              onChange={(e) => setNewGoogleChatSpace(e.target.value)}
              fullWidth
              size="small"
              inputProps={{ 'data-testid': 'new-chat-google-space' }}
            />
          </Stack>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setNewOpen(false)}>Cancel</Button>
          <Button
            variant="contained"
            disabled={creating}
            onClick={() => void handleCreateChat()}
            data-testid="create-shared-chat-submit"
          >
            Create Room
          </Button>
        </DialogActions>
      </Dialog>
    </Stack>
  );
}
