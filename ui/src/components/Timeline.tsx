// A task's events, as the task interface defines them (docs/hub.md
// §7.1): lifecycle, conversation, the token stream, and references to
// what only the node holds. A customised Grain's events look the same
// as a stock one's; a type the UI does not know is shown by its name.
import { Alert, Box, Chip, Stack, Typography } from '@mui/material';
import type { HubEvent } from '../api';
import { Markdown } from './Markdown';
import { ArtifactView } from './ArtifactView';

function time(t: string): string {
  const d = new Date(t);
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleTimeString();
}

export function EventRow({ e }: { e: HubEvent }) {
  return (
    <Box sx={{ display: 'flex', gap: 1.5, py: 0.75, borderBottom: 1, borderColor: 'divider' }}>
      <Typography variant="caption" color="text.secondary" sx={{ minWidth: 72, pt: 0.25 }}>
        {time(e.time)}
      </Typography>
      <Box sx={{ flex: 1, minWidth: 0 }}>
        <EventBody e={e} />
      </Box>
    </Box>
  );
}

function EventBody({ e }: { e: HubEvent }) {
  if (e.status) {
    const s = e.status;
    return (
      <Typography variant="body2" color={s.state === 'failed' ? 'error' : s.state === 'warning' ? 'warning.main' : 'text.secondary'}>
        {s.phase}
        {s.step ? ` · ${s.step}` : ''} · {s.state}
        {s.message ? `: ${s.message}` : ''}
      </Typography>
    );
  }
  if (e.transcript) {
    const t = e.transcript;
    if (t.kind === 'error') return <Alert severity="error">{t.text}</Alert>;
    if (t.kind === 'thinking')
      return (
        <Box sx={{ color: 'text.secondary', fontStyle: 'italic' }}>
          <Markdown text={t.text ?? ''} />
        </Box>
      );
    return <Markdown text={t.text ?? ''} />;
  }
  if (e.approval_request) {
    const a = e.approval_request;
    return (
      <Alert severity="info">
        Waiting for approval of <b>{a.tool}</b>
        {a.message ? `: ${a.message}` : ''}
      </Alert>
    );
  }
  if (e.usage) {
    const u = e.usage;
    return (
      <Typography variant="body2" color="text.secondary">
        usage{u.model ? ` (${u.model})` : ''}: {u.input_tokens} in, {u.output_tokens} out
      </Typography>
    );
  }
  if (e.resumable) {
    return (
      <Typography variant="body2" color="text.secondary">
        resumable ({e.resumable.cli})
      </Typography>
    );
  }
  if (e.result) {
    const r = e.result;
    return (
      <Stack spacing={1}>
        <Box>
          <Chip size="small" color={r.exit.code === 0 ? 'success' : 'error'} label={`${r.exit.reason}, exit ${r.exit.code}`} />
          {r.exit.cli_error && (
            <Typography variant="body2" color="error" component="span" sx={{ ml: 1 }}>
              {r.exit.cli_error.kind}
              {r.exit.cli_error.message ? `: ${r.exit.cli_error.message}` : ''}
            </Typography>
          )}
        </Box>
        {r.final_text && <Markdown text={r.final_text} />}
        {r.errors?.map((err, i) => (
          <Alert key={i} severity="error">
            {err}
          </Alert>
        ))}
      </Stack>
    );
  }
  if (e.artifact) {
    const a = e.artifact;
    const size = a.size ? ` (${a.size} bytes)` : '';
    if (!a.ref) {
      return (
        <Typography variant="body2" color="text.secondary">
          {a.kind}
          {size}, held by the node; it offers no view of it
        </Typography>
      );
    }
    return (
      <Box>
        <Typography variant="body2" color="text.secondary">
          {a.kind}
          {size}, held by the node
        </Typography>
        <ArtifactView task={e.task} kind={a.kind} artifactRef={a.ref} />
      </Box>
    );
  }
  return (
    <Typography variant="body2" color="text.secondary">
      {e.type}
    </Typography>
  );
}

export function Timeline({ events }: { events: HubEvent[] }) {
  if (events.length === 0) {
    return (
      <Typography variant="body2" color="text.secondary">
        No events yet.
      </Typography>
    );
  }
  return (
    <Box>
      {events.map((e) => (
        <EventRow key={e.seq} e={e} />
      ))}
    </Box>
  );
}
