// The teams the caller may see (GET /v1/teams): every team for a hub
// admin, their own for anyone else. A hub admin also creates teams
// here (POST /v1/teams).
import { useState } from 'react';
import { Alert, Button, LinearProgress, Paper, Stack, Table, TableBody, TableCell, TableHead, TableRow, TextField, Typography } from '@mui/material';
import { createTeam, listTeams, type Me } from '../api';
import { useAction } from '../action';
import { useLoad } from '../load';
import { Link, routePath } from '../router';

function CreateTeam({ onDone }: { onDone: () => void }) {
  const [id, setId] = useState('');
  const [name, setName] = useState('');
  const create = useAction(() => createTeam(id.trim(), name.trim()), () => {
    setId('');
    setName('');
    onDone();
  });
  return (
    <Paper
      component="form"
      sx={{ p: 2 }}
      onSubmit={(e) => {
        e.preventDefault();
        void create.run();
      }}
    >
      <Typography variant="subtitle1" sx={{ mb: 1 }}>
        New team
      </Typography>
      <Stack direction={{ xs: 'column', sm: 'row' }} spacing={1}>
        <TextField size="small" label="Team ID" value={id} onChange={(e) => setId(e.target.value)} />
        <TextField size="small" label="Name" value={name} onChange={(e) => setName(e.target.value)} />
        <Button type="submit" variant="contained" disabled={!id.trim() || create.busy}>
          Create team
        </Button>
      </Stack>
      {create.error && (
        <Alert severity="error" sx={{ mt: 1 }}>
          {create.error}
        </Alert>
      )}
    </Paper>
  );
}

export function Teams({ me }: { me?: Me }) {
  const teams = useLoad((signal) => listTeams(signal), []);
  return (
    <Stack spacing={2}>
      <Typography variant="h5">Teams</Typography>
      {teams.error && <Alert severity="error">{teams.error.message}</Alert>}
      {me?.hub_admin && <CreateTeam onDone={teams.reload} />}
      <Paper>
        {teams.loading && <LinearProgress />}
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Team</TableCell>
              <TableCell>Your role</TableCell>
              <TableCell>Members</TableCell>
              <TableCell>Repositories</TableCell>
              <TableCell>Backlog</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {teams.data?.map((t) => (
              <TableRow key={t.id}>
                <TableCell>
                  <Link to={routePath({ page: 'team', team: t.id })}>{t.name && t.name !== t.id ? `${t.name} (${t.id})` : t.id}</Link>
                </TableCell>
                <TableCell>{me?.teams?.[t.id] ?? (me?.hub_admin ? 'hub admin' : '')}</TableCell>
                <TableCell>{Object.keys(t.members ?? {}).length}</TableCell>
                <TableCell>{(t.repos ?? []).join(', ')}</TableCell>
                <TableCell>
                  <Link to={routePath({ page: 'backlog', team: t.id })}>tasks</Link>
                </TableCell>
              </TableRow>
            ))}
            {teams.data?.length === 0 && (
              <TableRow>
                <TableCell colSpan={5}>
                  <Typography variant="body2" color="text.secondary">
                    No teams.
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
