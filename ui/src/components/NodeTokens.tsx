// Node tokens (GET, POST and DELETE /v1/node-tokens): the caller's own,
// or every owner's for a hub admin. Minting one answers the token once.
// It is held in this component's state only, shown with a copy button
// until the caller dismisses it, and then dropped: it is never written
// to the browser's storage, and the hub keeps only its hash, so nothing
// can show it again. Minting again for the same node revokes the
// earlier token (docs/hub.md §5.2).
import { useState } from 'react';
import {
  Alert,
  Box,
  Button,
  LinearProgress,
  Paper,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  TextField,
  Typography,
} from '@mui/material';
import { listNodeTokens, mintNodeToken, revokeNodeToken, type MintedToken } from '../api';
import { useAction } from '../action';
import { useLoad } from '../load';
import { mono } from '../theme';
import { ConfirmButton } from './ConfirmButton';
import { when } from '../pages/Backlog';

function Minted({ minted, onDismiss }: { minted: MintedToken; onDismiss: () => void }) {
  const [copied, setCopied] = useState<string>();
  const copy = () => {
    const clip = navigator.clipboard;
    if (!clip) {
      setCopied('This browser gives the page no clipboard here: select the token and copy it.');
      return;
    }
    clip.writeText(minted.token).then(
      () => setCopied('Copied.'),
      () => setCopied('Could not copy: select the token and copy it.'),
    );
  };
  return (
    <Alert severity="success" onClose={onDismiss} sx={{ '& .MuiAlert-message': { minWidth: 0, flex: 1 } }}>
      <Typography variant="body2">
        A token for node <b>{minted.node_token.node}</b>. Copy it now: it is shown only this once, and the hub keeps no copy it could show
        again.
      </Typography>
      <Box
        data-testid="minted-token"
        sx={{ fontFamily: mono, my: 1, p: 1, bgcolor: 'background.paper', borderRadius: 1, wordBreak: 'break-all', userSelect: 'all' }}
      >
        {minted.token}
      </Box>
      <Stack direction="row" spacing={1} sx={{ alignItems: 'center' }}>
        <Button size="small" variant="contained" onClick={copy}>
          Copy
        </Button>
        <Button size="small" onClick={onDismiss}>
          Done
        </Button>
        {copied && <Typography variant="caption">{copied}</Typography>}
      </Stack>
    </Alert>
  );
}

export function NodeTokens({ showOwner }: { showOwner: boolean }) {
  const tokens = useLoad((signal) => listNodeTokens(signal), []);
  const [node, setNode] = useState('');
  const [minted, setMinted] = useState<MintedToken>();
  const mint = useAction(async () => {
    setMinted(undefined);
    setMinted(await mintNodeToken(node.trim()));
    setNode('');
  }, tokens.reload);
  const revoke = useAction((id: string) => revokeNodeToken(id), tokens.reload);

  return (
    <Stack spacing={2}>
      <Typography variant="h6">Node tokens</Typography>
      {minted && <Minted minted={minted} onDismiss={() => setMinted(undefined)} />}
      <Paper
        component="form"
        sx={{ p: 2 }}
        onSubmit={(e) => {
          e.preventDefault();
          void mint.run();
        }}
      >
        <Stack direction={{ xs: 'column', sm: 'row' }} spacing={1}>
          <TextField size="small" label="Node ID" value={node} onChange={(e) => setNode(e.target.value)} />
          <Button type="submit" variant="contained" disabled={!node.trim() || mint.busy}>
            Mint token
          </Button>
        </Stack>
        {mint.error && (
          <Alert severity="error" sx={{ mt: 1 }}>
            {mint.error}
          </Alert>
        )}
      </Paper>
      {tokens.error && <Alert severity="error">{tokens.error.message}</Alert>}
      {revoke.error && <Alert severity="error">{revoke.error}</Alert>}
      <Paper>
        {tokens.loading && <LinearProgress />}
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Node</TableCell>
              {showOwner && <TableCell>Owner</TableCell>}
              <TableCell>Token ID</TableCell>
              <TableCell>Minted</TableCell>
              <TableCell>Revoked</TableCell>
              <TableCell />
            </TableRow>
          </TableHead>
          <TableBody>
            {tokens.data?.map((t) => (
              <TableRow key={t.id}>
                <TableCell>{t.node}</TableCell>
                {showOwner && <TableCell>{t.owner}</TableCell>}
                <TableCell sx={{ fontFamily: mono }}>{t.id}</TableCell>
                <TableCell>{when(t.created)}</TableCell>
                <TableCell>{t.revoked ? when(t.revoked) + (t.released ? ' (released)' : '') : ''}</TableCell>
                <TableCell align="right">
                  {!t.revoked && (
                    <ConfirmButton confirm={`Revoke ${t.node}’s token`} disabled={revoke.busy} onConfirm={() => void revoke.run(t.id)}>
                      Revoke
                    </ConfirmButton>
                  )}
                </TableCell>
              </TableRow>
            ))}
            {tokens.data?.length === 0 && (
              <TableRow>
                <TableCell colSpan={showOwner ? 6 : 5}>
                  <Typography variant="body2" color="text.secondary">
                    No node tokens.
                  </Typography>
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </Paper>
    </Stack>
  );
}
