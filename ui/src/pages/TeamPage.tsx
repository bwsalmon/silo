// One team (GET /v1/teams/{team}): its members and their roles, the
// repositories it owns, and its settings. A team admin, as /v1/me says,
// sees the edit actions: members and roles (PUT and DELETE
// /members/{principal}), releasing a repository, and the settings. A
// hub admin also assigns repositories and deletes the team. Every one
// of them is the API's to allow or refuse, and its refusal is shown.
import { useState } from 'react';
import {
  Alert,
  Box,
  Button,
  Checkbox,
  FormControlLabel,
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
  TextField,
  Typography,
} from '@mui/material';
import {
  ApiError,
  assignRepo,
  deleteTeam,
  getTeam,
  getTeamSettings,
  releaseRepo,
  removeMember,
  setMember,
  setTeamSettings,
  type Me,
  type Role,
  type Team,
} from '../api';
import { useAction } from '../action';
import { useLoad } from '../load';
import { adminOf } from '../may';
import { Link, navigate, routePath } from '../router';
import { ConfirmButton } from '../components/ConfirmButton';

const roles: Role[] = ['member', 'admin'];

function Members({ team, admin, onDone }: { team: Team; admin: boolean; onDone: () => void }) {
  const [principal, setPrincipal] = useState('');
  const [role, setRole] = useState<Role>('member');
  const set = useAction((p: string, r: Role) => setMember(team.id, p, r), onDone);
  const remove = useAction((p: string) => removeMember(team.id, p), onDone);
  const add = useAction(() => setMember(team.id, principal.trim(), role), () => {
    setPrincipal('');
    onDone();
  });
  const members = Object.entries(team.members ?? {}).sort(([a], [b]) => a.localeCompare(b));
  return (
    <Paper sx={{ p: 2 }}>
      <Typography variant="h6">Members</Typography>
      <Table size="small">
        <TableHead>
          <TableRow>
            <TableCell>Principal</TableCell>
            <TableCell>Role</TableCell>
            {admin && <TableCell />}
          </TableRow>
        </TableHead>
        <TableBody>
          {members.map(([p, r]) => (
            <TableRow key={p}>
              <TableCell>{p}</TableCell>
              <TableCell>
                {admin ? (
                  <Select size="small" value={r} inputProps={{ 'aria-label': `Role of ${p}` }} disabled={set.busy} onChange={(e) => void set.run(p, e.target.value)}>
                    {roles.map((x) => (
                      <MenuItem key={x} value={x}>
                        {x}
                      </MenuItem>
                    ))}
                  </Select>
                ) : (
                  r
                )}
              </TableCell>
              {admin && (
                <TableCell align="right">
                  <ConfirmButton confirm={`Remove ${p}`} disabled={remove.busy} onConfirm={() => void remove.run(p)}>
                    Remove
                  </ConfirmButton>
                </TableCell>
              )}
            </TableRow>
          ))}
          {members.length === 0 && (
            <TableRow>
              <TableCell colSpan={3}>
                <Typography variant="body2" color="text.secondary">
                  No members.
                </Typography>
              </TableCell>
            </TableRow>
          )}
        </TableBody>
      </Table>
      {set.error && <Alert severity="error">{set.error}</Alert>}
      {remove.error && <Alert severity="error">{remove.error}</Alert>}
      {admin && (
        <Box
          component="form"
          sx={{ mt: 2 }}
          onSubmit={(e) => {
            e.preventDefault();
            void add.run();
          }}
        >
          <Stack direction={{ xs: 'column', sm: 'row' }} spacing={1}>
            <TextField
              size="small"
              label="Principal"
              placeholder="user:EMAIL or group:NAME"
              value={principal}
              onChange={(e) => setPrincipal(e.target.value)}
              sx={{ minWidth: 300 }}
            />
            <Select size="small" value={role} inputProps={{ 'aria-label': 'New member’s role' }} onChange={(e) => setRole(e.target.value)}>
              {roles.map((x) => (
                <MenuItem key={x} value={x}>
                  {x}
                </MenuItem>
              ))}
            </Select>
            <Button type="submit" variant="contained" disabled={!principal.trim() || add.busy}>
              Add member
            </Button>
          </Stack>
          {add.error && (
            <Alert severity="error" sx={{ mt: 1 }}>
              {add.error}
            </Alert>
          )}
        </Box>
      )}
    </Paper>
  );
}

function Repos({ team, admin, hubAdmin, onDone }: { team: Team; admin: boolean; hubAdmin: boolean; onDone: () => void }) {
  const [repo, setRepo] = useState('');
  const [history, setHistory] = useState(false);
  const release = useAction((r: string) => releaseRepo(team.id, r), onDone);
  const assign = useAction(() => assignRepo(team.id, repo.trim(), history), () => {
    setRepo('');
    setHistory(false);
    onDone();
  });
  const repos = [...(team.repos ?? [])].sort();
  return (
    <Paper sx={{ p: 2 }}>
      <Typography variant="h6">Repositories</Typography>
      <Table size="small">
        <TableBody>
          {repos.map((r) => (
            <TableRow key={r}>
              <TableCell>{r}</TableCell>
              {admin && (
                <TableCell align="right">
                  <ConfirmButton confirm={`Release ${r}`} disabled={release.busy} onConfirm={() => void release.run(r)}>
                    Release
                  </ConfirmButton>
                </TableCell>
              )}
            </TableRow>
          ))}
          {repos.length === 0 && (
            <TableRow>
              <TableCell>
                <Typography variant="body2" color="text.secondary">
                  The team owns no repository.
                </Typography>
              </TableCell>
            </TableRow>
          )}
        </TableBody>
      </Table>
      {release.error && <Alert severity="error">{release.error}</Alert>}
      {hubAdmin && (
        <Box
          component="form"
          sx={{ mt: 2 }}
          onSubmit={(e) => {
            e.preventDefault();
            void assign.run();
          }}
        >
          <Stack direction={{ xs: 'column', sm: 'row' }} spacing={1} sx={{ alignItems: { sm: 'center' } }}>
            <TextField size="small" label="Repository" placeholder="owner/name" value={repo} onChange={(e) => setRepo(e.target.value)} />
            <FormControlLabel
              control={<Checkbox checked={history} onChange={(e) => setHistory(e.target.checked)} />}
              label="With its history"
            />
            <Button type="submit" variant="contained" disabled={!repo.trim() || assign.busy}>
              Assign
            </Button>
          </Stack>
          {assign.error && (
            <Alert severity="error" sx={{ mt: 1 }}>
              {assign.error}
            </Alert>
          )}
        </Box>
      )}
    </Paper>
  );
}

function Settings({ team, admin }: { team: string; admin: boolean }) {
  const settings = useLoad((signal) => getTeamSettings(team, signal), [team]);
  const [value, setValue] = useState<string>();
  const shown = value ?? String(settings.data?.lost_requeue_after_seconds ?? 0);
  const save = useAction(() => setTeamSettings(team, { lost_requeue_after_seconds: Number(shown) }), () => {
    setValue(undefined);
    settings.reload();
  });
  return (
    <Paper sx={{ p: 2 }}>
      <Typography variant="h6">Settings</Typography>
      {settings.error && <Alert severity="error">{settings.error.message}</Alert>}
      {settings.data &&
        (admin ? (
          <Box
            component="form"
            sx={{ mt: 1 }}
            onSubmit={(e) => {
              e.preventDefault();
              void save.run();
            }}
          >
            <Stack direction="row" spacing={1}>
              <TextField
                size="small"
                type="number"
                label="Requeue a lost task after (seconds, 0 for never)"
                value={shown}
                onChange={(e) => setValue(e.target.value)}
                sx={{ minWidth: 340 }}
              />
              <Button type="submit" variant="contained" disabled={save.busy || value === undefined}>
                Save
              </Button>
            </Stack>
            {save.error && (
              <Alert severity="error" sx={{ mt: 1 }}>
                {save.error}
              </Alert>
            )}
          </Box>
        ) : (
          <Typography variant="body2">
            A lost task is requeued {settings.data.lost_requeue_after_seconds ? `after ${settings.data.lost_requeue_after_seconds}s` : 'never'}.
          </Typography>
        ))}
    </Paper>
  );
}

export function TeamPage({ me, team }: { me?: Me; team: string }) {
  const t = useLoad((signal) => getTeam(team, signal), [team]);
  const del = useAction(() => deleteTeam(team), () => navigate(routePath({ page: 'teams' })));
  if (t.error && !t.data) {
    const missing = t.error instanceof ApiError && t.error.status === 404;
    return <Alert severity={missing ? 'warning' : 'error'}>{missing ? `No team ${team} in your teams.` : t.error.message}</Alert>;
  }
  if (!t.data) return <LinearProgress />;
  const admin = adminOf(me, team);
  const hubAdmin = !!me?.hub_admin;
  return (
    <Stack spacing={2}>
      <Stack direction="row" spacing={2} sx={{ alignItems: 'center', flexWrap: 'wrap' }}>
        <Typography variant="h5">{t.data.name && t.data.name !== t.data.id ? `${t.data.name} (${t.data.id})` : t.data.id}</Typography>
        <Link to={routePath({ page: 'backlog', team })}>Backlog</Link>
        <Link to={routePath({ page: 'file', team })}>File a task</Link>
        <Box sx={{ flex: 1 }} />
        {hubAdmin && (
          <ConfirmButton confirm={`Delete ${team}`} disabled={del.busy} onConfirm={() => void del.run()}>
            Delete team
          </ConfirmButton>
        )}
      </Stack>
      {del.error && <Alert severity="error">{del.error}</Alert>}
      {t.error && <Alert severity="error">{t.error.message}</Alert>}
      <Members team={t.data} admin={admin} onDone={t.reload} />
      <Repos team={t.data} admin={admin} hubAdmin={hubAdmin} onDone={t.reload} />
      <Settings team={team} admin={admin} />
    </Stack>
  );
}
