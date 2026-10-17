// An interactive task's session (docs/hub.md §6.5): a clear "waiting
// for you" when its agent is waiting on a human (its last
// awaiting_reply event), the messages sent that the session has not
// yet taken in, and, for its submitter or a member of its team, a box
// to reply in (POST /v1/tasks/{id}/messages). Whether the session takes
// the message is the API's to say: one that is over answers 409.
import { useState } from 'react';
import { Alert, Box, Button, Stack, TextField, Typography } from '@mui/material';
import { sendMessage, type HubEvent, type Task, type TaskMessage } from '../api';
import { useAction } from '../action';
import Markdown from './Markdown';
import { MessageTurn } from './Timeline';

const over = new Set(['done', 'failed', 'cancelled', 'rejected']);

// newID names a draft: kept until the hub has it, so that sending it
// again after an error is the same message retried, not a second one.
function newID(): string {
  const c = globalThis.crypto;
  if (c?.randomUUID) return `m-${c.randomUUID()}`;
  return `m-${Date.now().toString(36)}-${Math.random().toString(36).slice(2)}`;
}

export function WaitingForYou({ question }: { question: string }) {
  return (
    <Alert severity="warning" variant="filled" sx={{ mb: 1 }}>
      <b>Waiting for you.</b> The session has stopped until someone replies.
      {question && (
        <Box sx={{ mt: 0.5 }}>
          <Markdown>{question}</Markdown>
        </Box>
      )}
    </Alert>
  );
}

export function ReplyBox({ task, onSent }: { task: string; onSent: () => void }) {
  const [text, setText] = useState('');
  const [id, setID] = useState(newID);
  const send = useAction(() => sendMessage(task, id, text), onSent);
  const submit = async () => {
    if (!text.trim() || send.busy) return;
    if (await send.run()) {
      setText('');
      setID(newID());
    }
  };
  return (
    <Stack
      component="form"
      spacing={1}
      sx={{ mt: 2 }}
      onSubmit={(e) => {
        e.preventDefault();
        void submit();
      }}
    >
      <TextField
        label="Reply"
        multiline
        minRows={2}
        value={text}
        onChange={(e) => setText(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
            e.preventDefault();
            void submit();
          }
        }}
        inputProps={{ 'aria-label': 'Reply to the session' }}
      />
      <Stack direction="row" spacing={1} sx={{ alignItems: 'center' }}>
        <Button type="submit" variant="contained" size="small" disabled={!text.trim() || send.busy}>
          Send
        </Button>
        <Typography variant="caption" color="text.secondary">
          Ctrl+Enter sends
        </Typography>
      </Stack>
      {send.error && <Alert severity="error">{send.error}</Alert>}
    </Stack>
  );
}

// Session is what an interactive task's page shows under its events:
// the messages not yet in them, and the reply box.
export function Session({
  t,
  events,
  messages,
  mayReply,
  onSent,
}: {
  t: Task;
  events: HubEvent[];
  messages: TaskMessage[];
  mayReply: boolean;
  onSent: () => void;
}) {
  const seen = new Set(events.flatMap((e) => (e.message ? [e.message.id] : [])));
  const pending = messages.filter((m) => !seen.has(m.id));
  return (
    <Box>
      {pending.map((m) => (
        <Box key={m.id} sx={{ py: 0.75, borderBottom: 1, borderColor: 'divider', opacity: 0.75 }}>
          <MessageTurn m={m} note={m.delivered ? 'sent to the session' : 'waiting to be sent to the session'} />
        </Box>
      ))}
      {mayReply && !over.has(t.state) && <ReplyBox task={t.id} onSent={onSent} />}
    </Box>
  );
}
