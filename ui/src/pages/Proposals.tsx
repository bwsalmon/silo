// A team's proposals: the tasks filed in its repositories by people
// outside it (GET /v1/tasks?team=&state=proposed), each with approve
// and reject for the team's members (docs/hub.md §6.1). Which tasks
// come back, and whether an approval is allowed, is the API's to say.
import { Alert, LinearProgress, Paper, Stack, Table, TableBody, TableCell, TableHead, TableRow, Typography } from '@mui/material';
import { listTasks, type Me } from '../api';
import { useLoad } from '../load';
import { mayApprove } from '../may';
import { Link, routePath } from '../router';
import { ProposalActions } from '../components/ProposalActions';
import { when } from './Backlog';

export function Proposals({ me, team }: { me?: Me; team: string }) {
  const tasks = useLoad((signal) => listTasks({ team, states: ['proposed'] }, signal), [team], 15000);
  return (
    <Stack spacing={2}>
      <Stack direction="row" spacing={2} sx={{ alignItems: 'center' }}>
        <Typography variant="h5">Proposals for {team}</Typography>
        <Link to={routePath({ page: 'backlog', team })}>Backlog</Link>
      </Stack>
      {tasks.error && <Alert severity="error">{tasks.error.message}</Alert>}
      <Paper>
        {tasks.loading && <LinearProgress />}
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Task</TableCell>
              <TableCell>Repository</TableCell>
              <TableCell>Proposed by</TableCell>
              <TableCell>Filed</TableCell>
              <TableCell>Decide</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {tasks.data?.map((t) => (
              <TableRow key={t.id}>
                <TableCell>
                  <Link to={routePath({ page: 'task', id: t.id })}>{t.title || t.id}</Link>
                </TableCell>
                <TableCell>{t.repo ?? ''}</TableCell>
                <TableCell>{t.submitter}</TableCell>
                <TableCell>{when(t.created_at)}</TableCell>
                <TableCell sx={{ minWidth: 260 }}>{mayApprove(me, t) ? <ProposalActions t={t} onDone={tasks.reload} /> : null}</TableCell>
              </TableRow>
            ))}
            {tasks.data?.length === 0 && (
              <TableRow>
                <TableCell colSpan={5}>
                  <Typography variant="body2" color="text.secondary">
                    No proposals.
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
