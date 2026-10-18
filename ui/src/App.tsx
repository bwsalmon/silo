// The UI's shell: a bar with the pages and who is signed in, and the
// page the URL names. /v1/me is asked only to fill the team selector,
// say who the caller is and choose which edit actions to show; it
// decides nothing, and the API answers every action itself.
import { useState } from 'react';
import { Alert, AppBar, Box, Button, Chip, Container, FormControlLabel, Switch, Toolbar, Typography } from '@mui/material';
import { ApiError, getMe, setAdminMode } from './api';
import { useLoad } from './load';
import { navigate, useRoute } from './router';
import { Backlog } from './pages/Backlog';
import { TaskPage } from './pages/TaskPage';
import { Grainfiles } from './pages/Grainfiles';
import { Nodes } from './pages/Nodes';
import { FileTask } from './pages/FileTask';
import { Teams } from './pages/Teams';
import { TeamPage } from './pages/TeamPage';
import { Proposals } from './pages/Proposals';

export const ADMIN_MODE_STORAGE_KEY = 'silo.adminMode';

function readStoredAdminMode(): boolean {
  try {
    const v = localStorage.getItem(ADMIN_MODE_STORAGE_KEY);
    return v === 'true' || v === 'on' || v === '1';
  } catch {
    return false;
  }
}

export function App() {
  const route = useRoute();
  const [adminMode, setAdminModeState] = useState<boolean>(() => {
    const initial = readStoredAdminMode();
    setAdminMode(initial);
    return initial;
  });
  const me = useLoad((signal) => getMe(signal), [adminMode]);
  const signedOut = me.error instanceof ApiError && me.error.status === 401;
  const canHubAdmin = Boolean(me.data?.can_hub_admin || me.data?.hub_admin);
  const activeAdminMode = canHubAdmin && (me.data?.can_hub_admin !== undefined ? adminMode : Boolean(me.data?.hub_admin));
  const effectiveMe = me.data ? { ...me.data, hub_admin: activeAdminMode } : undefined;

  const handleToggleAdminMode = (_: unknown, next: boolean) => {
    try {
      localStorage.setItem(ADMIN_MODE_STORAGE_KEY, next ? 'true' : 'false');
    } catch {
      // Ignore storage errors in restricted environments.
    }
    setAdminMode(next);
    setAdminModeState(next);
  };

  return (
    <Box sx={{ minHeight: '100vh', bgcolor: 'background.default' }}>
      <AppBar
        position="static"
        elevation={0}
        data-admin-mode={activeAdminMode ? 'on' : 'off'}
        sx={
          activeAdminMode
            ? {
                borderTop: '3px solid #f59e0b',
                borderBottom: '2px solid #f59e0b',
              }
            : undefined
        }
      >
        <Toolbar variant="dense" sx={{ gap: 1 }}>
          <Typography variant="h6" sx={{ mr: 2 }}>
            Grain Hub
          </Typography>
          <Button color="inherit" onClick={() => navigate('/')}>
            Backlog
          </Button>
          <Button color="inherit" onClick={() => navigate('/grainfiles')}>
            Grainfiles
          </Button>
          <Button color="inherit" onClick={() => navigate('/teams')}>
            Teams
          </Button>
          <Button color="inherit" onClick={() => navigate('/nodes')}>
            Nodes
          </Button>
          <Button color="inherit" onClick={() => navigate('/file')}>
            File a task
          </Button>
          <Box sx={{ flex: 1 }} />
          {canHubAdmin && (
            <FormControlLabel
              sx={{ color: 'inherit', ml: 0, mr: 0.5 }}
              control={
                <Switch
                  size="small"
                  color="warning"
                  checked={activeAdminMode}
                  onChange={handleToggleAdminMode}
                  inputProps={{ 'aria-label': 'Admin mode' }}
                />
              }
              label={
                <Typography variant="body2" sx={{ color: 'inherit' }}>
                  Admin mode
                </Typography>
              }
            />
          )}
          {activeAdminMode && (
            <Chip
              size="small"
              label="Admin mode active"
              data-testid="admin-mode-marker"
              sx={{
                bgcolor: '#fef3c7',
                color: '#451a03',
                border: '1px solid #f59e0b',
                fontWeight: 600,
              }}
            />
          )}
          {me.data && (
            <Typography variant="body2">
              {me.data.email}
              {activeAdminMode ? ' (hub admin)' : ''}
            </Typography>
          )}
        </Toolbar>
      </AppBar>
      <Container maxWidth="xl" sx={{ py: 3 }}>
        {signedOut ? (
          <Alert severity="warning">You are not signed in to this hub.</Alert>
        ) : (
          <Box key={adminMode ? 'admin-on' : 'admin-off'}>
            {me.error && <Alert severity="error">{me.error.message}</Alert>}
            {route.page === 'backlog' && <Backlog me={effectiveMe} team={route.team} />}
            {route.page === 'task' && <TaskPage id={route.id} me={effectiveMe} />}
            {route.page === 'grainfiles' && <Grainfiles me={effectiveMe} />}
            {route.page === 'nodes' && <Nodes me={effectiveMe} />}
            {route.page === 'file' && <FileTask me={effectiveMe} team={route.team} />}
            {route.page === 'teams' && <Teams me={effectiveMe} />}
            {route.page === 'team' && <TeamPage me={effectiveMe} team={route.team} />}
            {route.page === 'proposals' && <Proposals me={effectiveMe} team={route.team} />}
            {route.page === 'notfound' && <Alert severity="warning">No page at {route.path}.</Alert>}
          </Box>
        )}
      </Container>
    </Box>
  );
}
