// The backlog: one team's tasks, or those of every team the caller is
// in (GET /v1/tasks?team=&state=). Which tasks come back is the API's
// decision; the team list from /v1/me only fills the selector.
import { useState } from 'react';
import {
  Alert,
  Box,
  Chip,
  FormControl,
  InputLabel,
  LinearProgress,
  MenuItem,
  Paper,
  Select,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  Typography,
} from '@mui/material';
import { listTasks, TASK_STATES, type Me, type TaskState } from '../api';
import { useLoad } from '../load';
import { Link, navigate, routePath } from '../router';
import { StateChip } from '../components/StateChip';

export function when(t: string): string {
  const d = new Date(t);
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleString();
}

export function Backlog({ me, team }: { me?: Me; team?: string }) {
  const [states, setStates] = useState<TaskState[]>([]);
  const tasks = useLoad((signal) => listTasks({ team, states }, signal), [team, states.join(',')], 15000);
  const teams = Object.keys(me?.teams ?? {}).sort();
  if (team && !teams.includes(team)) teams.push(team);

  const toggle = (s: TaskState) => setStates((cur) => (cur.includes(s) ? cur.filter((x) => x !== s) : [...cur, s]));

  return (
    <Stack spacing={2}>
      <Stack direction="row" spacing={2} sx={{ alignItems: 'center', flexWrap: 'wrap', rowGap: 1 }}>
        <Typography variant="h5">Backlog</Typography>
        <FormControl size="small" sx={{ minWidth: 200 }}>
          <InputLabel id="team-label">Team</InputLabel>
          <Select
            labelId="team-label"
            label="Team"
            value={team ?? ''}
            onChange={(e) => navigate(routePath({ page: 'backlog', team: e.target.value || undefined }))}
          >
            <MenuItem value="">{me?.hub_admin ? 'All teams' : 'All my teams'}</MenuItem>
            {teams.map((t) => (
              <MenuItem key={t} value={t}>
                {t}
              </MenuItem>
            ))}
          </Select>
        </FormControl>
        <Box sx={{ display: 'flex', gap: 0.5, flexWrap: 'wrap' }}>
          {TASK_STATES.map((s) => (
            <Chip
              key={s}
              size="small"
              label={s}
              color={states.includes(s) ? 'primary' : 'default'}
              variant={states.includes(s) ? 'filled' : 'outlined'}
              onClick={() => toggle(s)}
            />
          ))}
        </Box>
      </Stack>
      {tasks.error && <Alert severity="error">{tasks.error.message}</Alert>}
      <Paper>
        {tasks.loading && <LinearProgress />}
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Task</TableCell>
              <TableCell>State</TableCell>
              <TableCell>Team</TableCell>
              <TableCell>Repository</TableCell>
              <TableCell>Node</TableCell>
              <TableCell>Submitter</TableCell>
              <TableCell>Updated</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {tasks.data?.map((t) => (
              <TableRow key={t.id} hover>
                <TableCell>
                  <Link to={routePath({ page: 'task', id: t.id })}>{t.title || t.id}</Link>
                </TableCell>
                <TableCell>
                  <StateChip state={t.state} cancelPending={t.cancel_pending} />
                </TableCell>
                <TableCell>{t.team}</TableCell>
                <TableCell>{t.repo ?? ''}</TableCell>
                <TableCell>{t.node ?? ''}</TableCell>
                <TableCell>{t.submitter}</TableCell>
                <TableCell>{when(t.updated_at)}</TableCell>
              </TableRow>
            ))}
            {tasks.data?.length === 0 && (
              <TableRow>
                <TableCell colSpan={7}>
                  <Typography variant="body2" color="text.secondary">
                    No tasks.
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
