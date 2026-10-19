// A task: its prompt, where it is in its life, and its live events,
// with the node-only artifacts they name viewed through the hub; for
// its submitter or a team admin, cancel and requeue; and for a member of
// its team, approving or rejecting it while it is proposed, and
// answering its approval requests while it runs. An interactive task's
// page is its session: a "waiting for you" while its agent waits on a
// human, and a reply box for its submitter and the members of its team.
// A task a Grain filed says which, on whose behalf, and from which task;
// a task with dependencies links to each.
import { useState, type ReactNode } from 'react';
import { Alert, Box, Button, Chip, LinearProgress, Link as MuiLink, Paper, Stack, TextField, Typography } from '@mui/material';
import { ApiError, cancelTask, getTask, listApprovals, listMessages, requeueTask, type Me, type NodeEntry, type Task } from '../api';
import { useAction } from '../action';
import { managesTask, mayApprove, mayMessage, memberOf } from '../may';
import { ApprovalPrompt } from '../components/ApprovalPrompt';
import { ProposalActions } from '../components/ProposalActions';
import { ConfirmButton } from '../components/ConfirmButton';
import { awaiting, useEventStream, type StreamState } from '../events';
import { Session, WaitingForYou } from '../components/Session';
import { useLoad } from '../load';
import { Link, routePath } from '../router';
import { StateChip } from '../components/StateChip';
import Markdown from '../components/Markdown';
import { Timeline } from '../components/Timeline';
import { when } from './Backlog';
import { nodeUIHref } from './Nodes';

const finished = new Set(['done', 'failed', 'cancelled', 'rejected']);

// Never run: no node has sent it an event.
const unplaced = new Set(['proposed', 'rejected']);

const streamLabels: Record<StreamState, string> = {
  connecting: 'connecting',
  live: 'live',
  reconnecting: 'reconnecting',
  ended: 'ended',
  closed: 'closed',
};

function Field({ name, children }: { name: string; children: ReactNode }) {
  return (
    <Box sx={{ display: 'flex', gap: 1 }}>
      <Typography variant="body2" color="text.secondary" sx={{ minWidth: 110 }}>
        {name}
      </Typography>
      <Typography variant="body2" component="div" sx={{ wordBreak: 'break-word' }}>
        {children}
      </Typography>
    </Box>
  );
}

function Lifecycle({ t }: { t: Task }) {
  return (
    <Stack spacing={0.5}>
      <Field name="State">
        <StateChip state={t.state} cancelPending={t.cancel_pending} />
      </Field>
      {t.reason && <Field name="Reason">{t.reason}</Field>}
      <Field name="Team">
        <Link to={routePath({ page: 'backlog', team: t.team })}>{t.team}</Link>
      </Field>
      {t.repo && <Field name="Repository">{t.repo}</Field>}
      {t.kind === 'interactive' && <Field name="Kind">Interactive session</Field>}
      <Field name="Submitter">{t.submitter}</Field>
      {t.filed_by && (
        <Field name="Filed by">
          Grain {t.filed_by}
          {t.filed_by_owner ? ` (${t.filed_by_owner})` : ''} on behalf of {t.submitter}
          {t.origin_task && (
            <>
              , from <Link to={routePath({ page: 'task', id: t.origin_task })}>{t.origin_task}</Link>
            </>
          )}
        </Field>
      )}
      {t.depends_on?.length ? (
        <Field name="Depends on">
          {t.depends_on.map((id, i) => (
            <span key={id}>
              {i > 0 && ', '}
              <Link to={routePath({ page: 'task', id })}>{id}</Link>
            </span>
          ))}
        </Field>
      ) : null}
      {t.approved_by && (
        <Field name="Approved by">
          {t.approved_by}
          {t.approved_at ? `, ${when(t.approved_at)}` : ''}
        </Field>
      )}
      {t.node && (
        <Field name="Node">
          {t.node}
          {t.node_owner ? ` (${t.node_owner})` : ''}
          {t.node_task ? `, as ${t.node_task}` : ''}
        </Field>
      )}
      {t.excluded?.length ? <Field name="Excluded">{t.excluded.join(', ')}</Field> : null}
      <Field name="Attempts">{t.attempts}</Field>
      <Field name="Created">{when(t.created_at)}</Field>
      <Field name="Updated">{when(t.updated_at)}</Field>
    </Stack>
  );
}

// Actions are cancel and requeue (POST /v1/tasks/{id}/cancel and
// /requeue), shown to whoever /v1/me says manages the task: its
// submitter, a team admin or a hub admin. They are shown in every
// state: whether the state allows one is the API's to say, with a 409.
function Actions({ t, onDone }: { t: Task; onDone: () => void }) {
  const [reason, setReason] = useState('');
  const cancel = useAction(() => cancelTask(t.id, reason.trim() || undefined), onDone);
  const requeue = useAction(() => requeueTask(t.id), onDone);
  const busy = cancel.busy || requeue.busy;
  return (
    <Stack spacing={1} sx={{ mt: 2 }}>
      <Typography variant="subtitle2">Actions</Typography>
      <TextField size="small" label="Reason to cancel (optional)" value={reason} onChange={(e) => setReason(e.target.value)} />
      <Stack direction="row" spacing={1} sx={{ flexWrap: 'wrap' }}>
        <ConfirmButton confirm="Confirm cancel" disabled={busy} onConfirm={() => void cancel.run().then((ok) => ok && setReason(''))}>
          Cancel task
        </ConfirmButton>
        <Button size="small" disabled={busy} onClick={() => void requeue.run()}>
          Requeue
        </Button>
      </Stack>
      {cancel.error && <Alert severity="error">{cancel.error}</Alert>}
      {requeue.error && <Alert severity="error">{requeue.error}</Alert>}
    </Stack>
  );
}

function Events({ t, me }: { t: Task; me?: Me }) {
  const id = t.id;
  const stream = useEventStream(id);
  const asks = stream.events.some((e) => e.approval_request);
  const approvals = useLoad((signal) => (asks ? listApprovals(id, signal) : Promise.resolve([])), [id, asks], asks ? 10000 : undefined);
  const answers = new Map((approvals.data ?? []).map((a) => [a.request_id, a]));
  const mayAnswer = t.state === 'running' && memberOf(me, t.team);
  const session = t.kind === 'interactive';
  const open = !finished.has(t.state);
  const messages = useLoad((signal) => (session ? listMessages(id, signal) : Promise.resolve([])), [id, session], session && open ? 5000 : undefined);
  const question = session && open ? awaiting(stream.events) : undefined;
  return (
    <Paper sx={{ p: 2 }}>
      <Stack direction="row" spacing={1} sx={{ alignItems: 'center', mb: 1 }}>
        <Typography variant="h6">{session ? 'Session' : 'Events'}</Typography>
        <Chip size="small" variant="outlined" label={streamLabels[stream.state]} color={stream.state === 'live' ? 'primary' : 'default'} />
        {question !== undefined && <Chip size="small" color="warning" label="waiting for you" />}
      </Stack>
      {question !== undefined && <WaitingForYou question={question} />}
      {approvals.error && <Alert severity="error">{approvals.error.message}</Alert>}
      {messages.error && <Alert severity="error">{messages.error.message}</Alert>}
      <Timeline
        events={stream.events}
        answer={(request) => (
          <ApprovalPrompt task={id} request={request} answer={answers.get(request)} mayAnswer={mayAnswer} onDone={approvals.reload} />
        )}
      />
      {session && <Session t={t} events={stream.events} messages={messages.data ?? []} mayReply={mayMessage(me, t)} onSent={messages.reload} />}
    </Paper>
  );
}

export function TaskPage({ id, me, nodes }: { id: string; me?: Me; nodes?: NodeEntry[] }) {
  const task = useLoad((signal) => getTask(id, signal), [id], 10000);
  const t = task.data;
  if (task.error && !t) {
    const missing = task.error instanceof ApiError && task.error.status === 404;
    return <Alert severity={missing ? 'warning' : 'error'}>{missing ? `No task ${id} in your teams.` : task.error.message}</Alert>;
  }
  if (!t) return <LinearProgress />;
  const matchedNode = t.node ? nodes?.find((n) => n.id === t.node) : undefined;
  const uiHref = matchedNode ? nodeUIHref(matchedNode) : undefined;
  return (
    <Stack spacing={2}>
      <Stack direction="row" spacing={2} sx={{ alignItems: 'center', justifyContent: 'space-between', flexWrap: 'wrap', rowGap: 1 }}>
        <Box>
          <Typography variant="h5">{t.title || t.id}</Typography>
          <Typography variant="caption" color="text.secondary">
            {t.id}
            {finished.has(t.state) ? '' : ' · refreshing'}
          </Typography>
        </Box>
        {t.node && uiHref && (
          <MuiLink
            href={uiHref}
            target="_blank"
            rel="noopener noreferrer"
            underline="hover"
            data-testid="open-in-node-ui"
          >
            {`Open in ${t.node} Grain UI`}
          </MuiLink>
        )}
      </Stack>
      {task.error && <Alert severity="error">{task.error.message}</Alert>}
      <Stack direction={{ xs: 'column', md: 'row' }} spacing={2} sx={{ alignItems: 'flex-start' }}>
        <Paper sx={{ p: 2, flex: 2, minWidth: 0, width: '100%' }}>
          <Typography variant="h6">Prompt</Typography>
          <Markdown>{t.prompt}</Markdown>
        </Paper>
        <Paper sx={{ p: 2, flex: 1, minWidth: 280, width: '100%' }}>
          <Typography variant="h6">Lifecycle</Typography>
          <Lifecycle t={t} />
          {t.state === 'proposed' && mayApprove(me, t) && (
            <Stack spacing={1} sx={{ mt: 2 }}>
              <Typography variant="subtitle2">Proposal</Typography>
              <ProposalActions t={t} onDone={task.reload} />
            </Stack>
          )}
          {managesTask(me, t) && <Actions t={t} onDone={task.reload} />}
        </Paper>
      </Stack>
      {!unplaced.has(t.state) && <Events t={t} me={me} />}
    </Stack>
  );
}
