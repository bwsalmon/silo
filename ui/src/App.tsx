// The UI's shell: a bar with the pages and who is signed in, and the
// page the URL names. /v1/me is asked only to fill the team selector,
// say who the caller is and choose which edit actions to show; it
// decides nothing, and the API answers every action itself.
import { Alert, AppBar, Box, Button, Container, Toolbar, Typography } from '@mui/material';
import { ApiError, getMe } from './api';
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

export function App() {
  const route = useRoute();
  const me = useLoad((signal) => getMe(signal), []);
  const signedOut = me.error instanceof ApiError && me.error.status === 401;

  return (
    <Box sx={{ minHeight: '100vh', bgcolor: 'background.default' }}>
      <AppBar position="static" elevation={0}>
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
          {me.data && (
            <Typography variant="body2">
              {me.data.email}
              {me.data.hub_admin ? ' (hub admin)' : ''}
            </Typography>
          )}
        </Toolbar>
      </AppBar>
      <Container maxWidth="xl" sx={{ py: 3 }}>
        {signedOut ? (
          <Alert severity="warning">You are not signed in to this hub.</Alert>
        ) : (
          <>
            {me.error && <Alert severity="error">{me.error.message}</Alert>}
            {route.page === 'backlog' && <Backlog me={me.data} team={route.team} />}
            {route.page === 'task' && <TaskPage id={route.id} me={me.data} />}
            {route.page === 'grainfiles' && <Grainfiles me={me.data} />}
            {route.page === 'nodes' && <Nodes me={me.data} />}
            {route.page === 'file' && <FileTask me={me.data} team={route.team} />}
            {route.page === 'teams' && <Teams me={me.data} />}
            {route.page === 'team' && <TeamPage me={me.data} team={route.team} />}
            {route.page === 'proposals' && <Proposals me={me.data} team={route.team} />}
            {route.page === 'notfound' && <Alert severity="warning">No page at {route.path}.</Alert>}
          </>
        )}
      </Container>
    </Box>
  );
}
