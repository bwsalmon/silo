// The answer to one of a running task's approval requests: who allowed
// or denied it, once answered (GET /v1/tasks/{id}/approvals), or allow
// and deny for a member of the task's team (POST
// /v1/tasks/{id}/approvals/{request_id}, docs/hub.md §6.1). Whether the
// request can still be answered is the API's to say: one answered
// already, past its deadline, or of a task no longer running is its 409.
import { useState } from 'react';
import { Alert, Button, Stack, TextField, Typography } from '@mui/material';
import { answerApproval, type Approval, type Decision } from '../api';
import { useAction } from '../action';

export function ApprovalPrompt({
  task,
  request,
  answer,
  mayAnswer,
  onDone,
}: {
  task: string;
  request: string;
  answer?: Approval;
  mayAnswer: boolean;
  onDone: () => void;
}) {
  const [reason, setReason] = useState('');
  const send = useAction((d: Decision) => answerApproval(task, request, d, reason.trim() || undefined), onDone);
  if (answer) {
    return (
      <Typography variant="body2" color={answer.decision === 'allow' ? 'success.main' : 'error'}>
        {answer.decision === 'allow' ? 'Allowed' : 'Denied'} by {answer.answered_by}
        {answer.reason ? `: ${answer.reason}` : ''}
        {answer.delivered ? '' : ' (not yet sent to the node)'}
      </Typography>
    );
  }
  if (!mayAnswer) return null;
  return (
    <Stack spacing={1} sx={{ mt: 1 }}>
      <TextField
        size="small"
        label="Reason (optional)"
        value={reason}
        onChange={(e) => setReason(e.target.value)}
        inputProps={{ 'aria-label': `Reason for ${request}` }}
      />
      <Stack direction="row" spacing={1}>
        <Button size="small" variant="contained" color="success" disabled={send.busy} onClick={() => void send.run('allow')}>
          Allow
        </Button>
        <Button size="small" variant="outlined" color="error" disabled={send.busy} onClick={() => void send.run('deny')}>
          Deny
        </Button>
      </Stack>
      {send.error && <Alert severity="error">{send.error}</Alert>}
    </Stack>
  );
}
