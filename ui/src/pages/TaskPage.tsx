// A task: its prompt, where it is in its life, and its live events,
// with the node-only artifacts they name viewed through the hub; and,
// for its submitter or a team admin, cancel and requeue.
import { useState, type ReactNode } from 'react';
import { Alert, Box, Button, Chip, LinearProgress, Paper, Stack, TextField, Typography } from '@mui/material';
import { ApiError, cancelTask, getTask, requeueTask, type Me, type Task } from '../api';
import { useAction } from '../action';
import { managesTask } from '../may';
import { ConfirmButton } from '../components/ConfirmButton';
import { useEventStream, type StreamState } from '../events';
import { useLoad } from '../load';
import { Link, routePath } from '../router';
import { StateChip } from '../components/StateChip';
import Markdown from '../components/Markdown';
import { Timeline } from '../components/Timeline';
import { when } from './Backlog';

const finished = new Set(['done', 'failed', 'cancelled']);

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
      <Field name="Submitter">{t.submitter}</Field>
      {t.approved_by && <Field name="Approved by">{t.approved_by}</Field>}
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

function Events({ id }: { id: string }) {
  const stream = useEventStream(id);
  return (
    <Paper sx={{ p: 2 }}>
      <Stack direction="row" spacing={1} sx={{ alignItems: 'center', mb: 1 }}>
        <Typography variant="h6">Events</Typography>
        <Chip size="small" variant="outlined" label={streamLabels[stream.state]} color={stream.state === 'live' ? 'primary' : 'default'} />
      </Stack>
      <Timeline events={stream.events} />
    </Paper>
  );
}

export function TaskPage({ id, me }: { id: string; me?: Me }) {
  const task = useLoad((signal) => getTask(id, signal), [id], 10000);
  const t = task.data;
  if (task.error && !t) {
    const missing = task.error instanceof ApiError && task.error.status === 404;
    return <Alert severity={missing ? 'warning' : 'error'}>{missing ? `No task ${id} in your teams.` : task.error.message}</Alert>;
  }
  if (!t) return <LinearProgress />;
  return (
    <Stack spacing={2}>
      <Box>
        <Typography variant="h5">{t.title || t.id}</Typography>
        <Typography variant="caption" color="text.secondary">
          {t.id}
          {finished.has(t.state) ? '' : ' · refreshing'}
        </Typography>
      </Box>
      {task.error && <Alert severity="error">{task.error.message}</Alert>}
      <Stack direction={{ xs: 'column', md: 'row' }} spacing={2} sx={{ alignItems: 'flex-start' }}>
        <Paper sx={{ p: 2, flex: 2, minWidth: 0, width: '100%' }}>
          <Typography variant="h6">Prompt</Typography>
          <Markdown>{t.prompt}</Markdown>
        </Paper>
        <Paper sx={{ p: 2, flex: 1, minWidth: 280, width: '100%' }}>
          <Typography variant="h6">Lifecycle</Typography>
          <Lifecycle t={t} />
          {managesTask(me, t) && <Actions t={t} onDone={task.reload} />}
        </Paper>
      </Stack>
      <Events id={t.id} />
    </Stack>
  );
}
