// Filing a task (POST /v1/tasks): to one of the caller's teams, or to a
// repository one of them owns. The teams and their repositories come
// from the directory (GET /v1/teams), which lists only the teams the
// caller is in (every team, for a hub admin); the API checks the
// target again, and its refusal is shown as it says it.
import { useMemo, useState } from 'react';
import { Alert, Button, FormControl, InputLabel, LinearProgress, MenuItem, Paper, Select, Stack, TextField, Typography } from '@mui/material';
import { listTeams, submitTask, type Me, type NewTask } from '../api';
import { useAction } from '../action';
import { useLoad } from '../load';
import { navigate, routePath } from '../router';

// The repository select's value for "the team, no one repository".
const anyRepo = '';

export function FileTask({ me, team: initialTeam }: { me?: Me; team?: string }) {
  const teams = useLoad((signal) => listTeams(signal), []);
  const [team, setTeam] = useState(initialTeam ?? '');
  const [repo, setRepo] = useState(anyRepo);
  const [title, setTitle] = useState('');
  const [prompt, setPrompt] = useState('');
  const [node, setNode] = useState('');

  const options = useMemo(() => {
    const ts = (teams.data ?? []).map((t) => ({ id: t.id, name: t.name, repos: [...(t.repos ?? [])].sort() }));
    // A team /v1/me names that the directory did not list (it would
    // not, but the API is the one to say so) is still offered.
    for (const id of Object.keys(me?.teams ?? {})) if (!ts.some((t) => t.id === id)) ts.push({ id, name: id, repos: [] });
    return ts.sort((a, b) => a.id.localeCompare(b.id));
  }, [teams.data, me]);
  const chosen = options.find((t) => t.id === team);

  const submit = useAction(async () => {
    const req: NewTask = repo ? { repo, title, prompt } : { team, title, prompt };
    if (node.trim()) req.constraints = { node: node.trim() };
    const t = await submitTask(req);
    navigate(routePath({ page: 'task', id: t.id }));
  });

  return (
    <Stack spacing={2} sx={{ maxWidth: 900 }}>
      <Typography variant="h5">File a task</Typography>
      {teams.error && <Alert severity="error">{teams.error.message}</Alert>}
      <Paper
        component="form"
        sx={{ p: 2 }}
        onSubmit={(e) => {
          e.preventDefault();
          void submit.run();
        }}
      >
        {(teams.loading || submit.busy) && <LinearProgress sx={{ mb: 2 }} />}
        <Stack spacing={2}>
          <Stack direction={{ xs: 'column', sm: 'row' }} spacing={2}>
            <FormControl size="small" sx={{ minWidth: 220 }} required>
              <InputLabel id="file-team-label">Team</InputLabel>
              <Select
                labelId="file-team-label"
                label="Team"
                value={chosen ? team : ''}
                onChange={(e) => {
                  setTeam(e.target.value);
                  setRepo(anyRepo);
                }}
              >
                {options.map((t) => (
                  <MenuItem key={t.id} value={t.id}>
                    {t.name && t.name !== t.id ? `${t.name} (${t.id})` : t.id}
                  </MenuItem>
                ))}
              </Select>
            </FormControl>
            <FormControl size="small" sx={{ minWidth: 260 }} disabled={!chosen}>
              <InputLabel id="file-repo-label">Repository</InputLabel>
              <Select labelId="file-repo-label" label="Repository" value={repo} onChange={(e) => setRepo(e.target.value)}>
                <MenuItem value={anyRepo}>Any of the team’s</MenuItem>
                {chosen?.repos.map((r) => (
                  <MenuItem key={r} value={r}>
                    {r}
                  </MenuItem>
                ))}
              </Select>
            </FormControl>
            <TextField size="small" label="Node (optional)" value={node} onChange={(e) => setNode(e.target.value)} />
          </Stack>
          {!teams.loading && options.length === 0 && (
            <Alert severity="info">You are in no team, so there is nowhere to file a task. A hub admin adds you to one.</Alert>
          )}
          <TextField size="small" label="Title" value={title} onChange={(e) => setTitle(e.target.value)} />
          <TextField label="Prompt" required multiline minRows={8} value={prompt} onChange={(e) => setPrompt(e.target.value)} />
          {submit.error && <Alert severity="error">{submit.error}</Alert>}
          <Stack direction="row" spacing={1}>
            <Button type="submit" variant="contained" disabled={!chosen || !prompt.trim() || submit.busy}>
              File task
            </Button>
          </Stack>
        </Stack>
      </Paper>
    </Stack>
  );
}
