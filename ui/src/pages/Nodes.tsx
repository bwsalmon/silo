// The node directory (GET /v1/nodes): the nodes the caller may see,
// as the API filters them; and the caller's node tokens, minted and
// revoked here.
import { Alert, LinearProgress, Paper, Stack, Table, TableBody, TableCell, TableHead, TableRow, Typography } from '@mui/material';
import { listNodes, type Me } from '../api';
import { NodeTokens } from '../components/NodeTokens';
import { useLoad } from '../load';
import { NodeStatusChip } from '../components/StateChip';
import { when } from './Backlog';

export function Nodes({ me }: { me?: Me }) {
  const nodes = useLoad((signal) => listNodes(signal), [], 15000);
  return (
    <Stack spacing={2}>
      <Typography variant="h5">Nodes</Typography>
      {nodes.error && <Alert severity="error">{nodes.error.message}</Alert>}
      <Paper>
        {nodes.loading && <LinearProgress />}
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Node</TableCell>
              <TableCell>Status</TableCell>
              <TableCell>Owner</TableCell>
              <TableCell>Slots free</TableCell>
              <TableCell>Repositories</TableCell>
              <TableCell>Version</TableCell>
              <TableCell>Last seen</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {nodes.data?.map((n) => (
              <TableRow key={n.id}>
                <TableCell>{n.id}</TableCell>
                <TableCell>
                  <NodeStatusChip status={n.status} />
                </TableCell>
                <TableCell>{n.owner}</TableCell>
                <TableCell>
                  {Math.max(n.free - n.claimed, 0)} of {n.slots}
                </TableCell>
                <TableCell>{n.repos?.join(', ')}</TableCell>
                <TableCell>{n.version ?? ''}</TableCell>
                <TableCell>{n.status === 'offline' ? '' : when(n.last_seen)}</TableCell>
              </TableRow>
            ))}
            {nodes.data?.length === 0 && (
              <TableRow>
                <TableCell colSpan={7}>
                  <Typography variant="body2" color="text.secondary">
                    No nodes.
                  </Typography>
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </Paper>
      <NodeTokens showOwner={!!me?.hub_admin} />
    </Stack>
  );
}
