// Approve or reject a proposed task (POST /v1/tasks/{id}/approve and
// /reject, docs/hub.md §6.1), shown to a member of the task's team who
// did not submit it, as /v1/me says. Whether the task can still be
// approved is the API's to say: a second approval is its 409.
import { useState } from 'react';
import { Alert, Button, Stack, TextField } from '@mui/material';
import { approveTask, rejectTask, type Task } from '../api';
import { useAction } from '../action';
import { ConfirmButton } from './ConfirmButton';

export function ProposalActions({ t, onDone }: { t: Task; onDone: () => void }) {
  const [reason, setReason] = useState('');
  const approve = useAction(() => approveTask(t.id), onDone);
  const reject = useAction(() => rejectTask(t.id, reason.trim() || undefined), onDone);
  const busy = approve.busy || reject.busy;
  return (
    <Stack spacing={1}>
      <TextField
        size="small"
        label="Reason to reject (optional)"
        value={reason}
        onChange={(e) => setReason(e.target.value)}
        inputProps={{ 'aria-label': `Reason to reject ${t.title || t.id}` }}
      />
      <Stack direction="row" spacing={1} sx={{ flexWrap: 'wrap' }}>
        <Button size="small" variant="contained" disabled={busy} onClick={() => void approve.run()}>
          Approve
        </Button>
        <ConfirmButton confirm="Confirm reject" disabled={busy} onConfirm={() => void reject.run().then((ok) => ok && setReason(''))}>
          Reject
        </ConfirmButton>
      </Stack>
      {approve.error && <Alert severity="error">{approve.error}</Alert>}
      {reject.error && <Alert severity="error">{reject.error}</Alert>}
    </Stack>
  );
}
