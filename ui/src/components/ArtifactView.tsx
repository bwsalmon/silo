// A node-only artifact, viewed through the hub (docs/ui.md §2, the
// trust boundary). Its bytes come from the node that holds it, at the
// moment someone asks, and live only in this component's state: they
// are not written to any storage, not cached (fetchArtifact), and gone
// when the view is closed. When the node is offline the hub answers
// 503, and the view says so and shows nothing.
import { useEffect, useState } from 'react';
import { Alert, Box, Button, CircularProgress } from '@mui/material';
import { fetchArtifact, type ArtifactResult } from '../api';
import { DiffPane, PlainText } from './DiffPane';

export function ArtifactView({ task, kind, artifactRef }: { task: string; kind: string; artifactRef: string }) {
  const [open, setOpen] = useState(false);
  const [result, setResult] = useState<ArtifactResult | null>(null);
  useEffect(() => {
    if (!open) {
      setResult(null);
      return;
    }
    const ctl = new AbortController();
    fetchArtifact(task, artifactRef, ctl.signal).then(setResult, (err: unknown) => {
      if (!ctl.signal.aborted) setResult({ kind: 'error', status: 0, message: String(err) });
    });
    return () => ctl.abort();
  }, [open, task, artifactRef]);

  return (
    <Box>
      <Button size="small" onClick={() => setOpen((o) => !o)}>
        {open ? 'Hide' : 'View'} {kind}
      </Button>
      {open && (
        <Box sx={{ mt: 1 }}>
          {result === null && <CircularProgress size={18} />}
          {result?.kind === 'unavailable' && (
            <Alert severity="warning">Unavailable: the node that holds this {kind} is offline. The hub keeps no copy.</Alert>
          )}
          {result?.kind === 'error' && <Alert severity="error">{result.message}</Alert>}
          {result?.kind === 'text' && (kind === 'diff' ? <DiffPane text={result.text} /> : <PlainText text={result.text} />)}
        </Box>
      )}
    </Box>
  );
}
