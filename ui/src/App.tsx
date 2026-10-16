// The UI's shell: a bar with the pages and who is signed in, and the
// page the URL names. /v1/me is asked only to fill the team selector
// and say who the caller is; it decides nothing.
import { Alert, AppBar, Box, Button, Container, Toolbar, Typography } from '@mui/material';
import { ApiError, getMe } from './api';
import { useLoad } from './load';
import { navigate, useRoute } from './router';
import { Backlog } from './pages/Backlog';
import { TaskPage } from './pages/TaskPage';
import { Nodes } from './pages/Nodes';

export function App() {
  const route = useRoute();
  const me = useLoad((signal) => getMe(signal), []);
  const signedOut = me.error instanceof ApiError && me.error.status === 401;

  return (
    <Box sx={{ minHeight: '100vh', bgcolor: 'background.default' }}>
      <AppBar position="static" elevation={0}>
        <Toolbar variant="dense" sx={{ gap: 1 }}>
          <Typography variant="h6" sx={{ mr: 2 }}>
            Silo
          </Typography>
          <Button color="inherit" onClick={() => navigate('/')}>
            Backlog
          </Button>
          <Button color="inherit" onClick={() => navigate('/nodes')}>
            Nodes
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
            {route.page === 'task' && <TaskPage id={route.id} />}
            {route.page === 'nodes' && <Nodes />}
            {route.page === 'notfound' && <Alert severity="warning">No page at {route.path}.</Alert>}
          </>
        )}
      </Container>
    </Box>
  );
}
