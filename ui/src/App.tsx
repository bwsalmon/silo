// The UI's shell: a bar with the Deployment Switcher, top-level Hub
// pages (Inbox, Grainfiles, Teams, Nodes), theme switcher (light / dark
// / auto -> resolved light or dark synced to /v1/me/theme), and who is
// signed in. /v1/me is asked only to fill selectors, say who the caller
// is and choose which edit actions to show; it decides nothing, and the
// API answers every action itself.
import { useEffect, useMemo, useRef, useState, type HTMLAttributes } from 'react';
import {
  Alert,
  AppBar,
  Box,
  Button,
  Chip,
  Container,
  CssBaseline,
  FormControlLabel,
  MenuItem,
  Select,
  Stack,
  Switch,
  ThemeProvider,
  Toolbar,
  Typography,
  useMediaQuery,
} from '@mui/material';
import {
  ApiError,
  getMe,
  listInbox,
  listNodes,
  setAdminMode,
  setTheme,
  type ResolvedTheme,
} from './api';
import { NodeStatusChip } from './components/StateChip';
import { useLoad } from './load';
import { navigate, useRoute } from './router';
import { darkTheme, darkThemeReducedMotion, lightTheme, lightThemeReducedMotion, mono } from './theme';
import { Backlog } from './pages/Backlog';
import { Inbox } from './pages/Inbox';
import { TaskPage } from './pages/TaskPage';
import { Grainfiles } from './pages/Grainfiles';
import { Nodes, nodeUIHref } from './pages/Nodes';
import { FileTask } from './pages/FileTask';
import { Teams } from './pages/Teams';
import { TeamPage } from './pages/TeamPage';
import { Deployments, DeploymentPage } from './pages/Deployments';
import { Proposals } from './pages/Proposals';

export const ADMIN_MODE_STORAGE_KEY = 'silo.adminMode';
export const THEME_MODE_STORAGE_KEY = 'silo.themeMode';

export type ThemeMode = 'light' | 'dark' | 'auto';

function readStoredAdminMode(): boolean {
  try {
    const v = localStorage.getItem(ADMIN_MODE_STORAGE_KEY);
    return v === 'true' || v === 'on' || v === '1';
  } catch {
    return false;
  }
}

function readStoredThemeMode(): ThemeMode {
  try {
    const v = localStorage.getItem(THEME_MODE_STORAGE_KEY);
    if (v === 'light' || v === 'dark' || v === 'auto') return v;
    return 'auto';
  } catch {
    return 'auto';
  }
}

export function App() {
  const route = useRoute();
  const [adminMode, setAdminModeState] = useState<boolean>(() => {
    const initial = readStoredAdminMode();
    setAdminMode(initial);
    return initial;
  });
  const [themeMode, setThemeModeState] = useState<ThemeMode>(readStoredThemeMode);
  const prefersDark = useMediaQuery('(prefers-color-scheme: dark)', { noSsr: true });
  const reduceMotion = useMediaQuery('(prefers-reduced-motion: reduce)', { noSsr: true });
  const resolvedTheme: ResolvedTheme = themeMode === 'auto' ? (prefersDark ? 'dark' : 'light') : themeMode;
  const muiTheme = useMemo(
    () =>
      resolvedTheme === 'dark'
        ? reduceMotion
          ? darkThemeReducedMotion
          : darkTheme
        : reduceMotion
          ? lightThemeReducedMotion
          : lightTheme,
    [resolvedTheme, reduceMotion],
  );

  const me = useLoad((signal) => getMe(signal), [adminMode]);
  const nodes = useLoad((signal) => listNodes(signal), [adminMode], 15000);
  const inbox = useLoad((signal) => listInbox({}, signal), [adminMode], 15000);

  const prevPrefersDark = useRef<boolean>(prefersDark);
  const lastSyncedTheme = useRef<ResolvedTheme | undefined>(undefined);

  useEffect(() => {
    const mediaChanged = prevPrefersDark.current !== prefersDark;
    prevPrefersDark.current = prefersDark;
    if (mediaChanged && themeMode === 'auto') {
      lastSyncedTheme.current = resolvedTheme;
      void setTheme(resolvedTheme).catch(() => {});
      return;
    }
    if (me.data?.theme !== undefined) {
      const currentServerTheme = lastSyncedTheme.current ?? me.data.theme;
      if (currentServerTheme !== resolvedTheme) {
        lastSyncedTheme.current = resolvedTheme;
        void setTheme(resolvedTheme).catch(() => {});
      } else {
        lastSyncedTheme.current = currentServerTheme;
      }
    }
  }, [prefersDark, themeMode, resolvedTheme, me.data?.theme]);

  const signedOut = me.error instanceof ApiError && me.error.status === 401;
  const canHubAdmin = Boolean(me.data?.can_hub_admin || me.data?.hub_admin);
  const activeAdminMode = canHubAdmin && (me.data?.can_hub_admin !== undefined ? adminMode : Boolean(me.data?.hub_admin));
  const effectiveMe = me.data ? { ...me.data, hub_admin: activeAdminMode } : undefined;
  const inboxCount = inbox.data?.length ?? 0;

  const handleToggleAdminMode = (_: unknown, next: boolean) => {
    try {
      localStorage.setItem(ADMIN_MODE_STORAGE_KEY, next ? 'true' : 'false');
    } catch {
      // Ignore storage errors in restricted environments.
    }
    setAdminMode(next);
    setAdminModeState(next);
  };

  const handleChangeThemeMode = (next: ThemeMode) => {
    try {
      localStorage.setItem(THEME_MODE_STORAGE_KEY, next);
    } catch {
      // Ignore storage errors in restricted environments.
    }
    setThemeModeState(next);
    const nextResolved: ResolvedTheme = next === 'auto' ? (prefersDark ? 'dark' : 'light') : next;
    lastSyncedTheme.current = nextResolved;
    void setTheme(nextResolved).catch(() => {});
  };

  const handleInboxChanged = () => {
    inbox.reload();
    me.reload();
  };

  return (
    <ThemeProvider theme={muiTheme}>
      <CssBaseline />
      <Box
        data-theme={resolvedTheme}
        data-theme-mode={themeMode}
        sx={{ minHeight: '100vh', bgcolor: 'background.default' }}
      >
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
          <Toolbar variant="dense" sx={{ gap: 1, flexWrap: 'wrap' }}>
            <Typography variant="h6" sx={{ mr: 1 }}>
              Grain Hub
            </Typography>
            <Select
              size="small"
              value=""
              displayEmpty
              renderValue={() => 'All deployments'}
              onChange={(e) => {
                const targetId = String(e.target.value);
                if (!targetId) return;
                const targetNode = nodes.data?.find((n) => n.id === targetId);
                const href = targetNode ? nodeUIHref(targetNode) : undefined;
                if (href) {
                  try {
                    window.location.assign(href);
                  } catch {
                    window.location.href = href;
                  }
                }
              }}
              inputProps={{ 'aria-label': 'Deployment switcher' }}
              SelectDisplayProps={
                {
                  'data-testid': 'deployment-switcher',
                  'aria-label': 'Deployment switcher',
                } as HTMLAttributes<HTMLDivElement>
              }
              sx={{
                color: 'inherit',
                minWidth: 210,
                mr: 1,
                '.MuiSelect-select': { py: 0.5, fontSize: 13 },
                '.MuiSelect-icon': { color: 'inherit' },
                '.MuiOutlinedInput-notchedOutline': { borderColor: 'currentColor' },
                '&:hover .MuiOutlinedInput-notchedOutline': { borderColor: 'currentColor' },
              }}
            >
              <MenuItem value="">Grain Hub (All deployments)</MenuItem>
              {(nodes.data ?? []).map((n) => {
                const href = nodeUIHref(n);
                return (
                  <MenuItem
                    key={n.id}
                    value={n.id}
                    disabled={!href}
                    data-testid={`deployment-option-${n.id}`}
                  >
                    <Stack direction="row" spacing={1} sx={{ alignItems: 'center', width: '100%' }}>
                      <Typography variant="body2" sx={{ fontFamily: mono }}>
                        {n.id}
                      </Typography>
                      <NodeStatusChip status={n.status} />
                      {n.ui?.team && (
                        <Typography variant="caption" color="text.secondary">
                          {n.ui.team}
                        </Typography>
                      )}
                      {n.viewer_role && <Chip size="small" label={n.viewer_role} variant="outlined" />}
                    </Stack>
                  </MenuItem>
                );
              })}
            </Select>
            <Button color="inherit" aria-label="Inbox" onClick={() => navigate('/inbox')}>
              Inbox
              {inboxCount > 0 && (
                <Chip
                  size="small"
                  label={String(inboxCount)}
                  data-testid="inbox-count-badge"
                  aria-hidden="true"
                  sx={{
                    ml: 0.75,
                    height: 18,
                    fontSize: 11,
                    bgcolor: 'background.paper',
                    color: 'text.primary',
                    fontWeight: 600,
                  }}
                />
              )}
            </Button>
            <Button color="inherit" onClick={() => navigate('/grainfiles')}>
              Grainfiles
            </Button>
            <Button color="inherit" onClick={() => navigate('/deployments')}>
              Deployments
            </Button>
            <Button color="inherit" onClick={() => navigate('/teams')}>
              Teams
            </Button>
            <Button color="inherit" onClick={() => navigate('/nodes')}>
              Nodes
            </Button>
            <Box sx={{ flex: 1 }} />
            <Select
              size="small"
              value={themeMode}
              onChange={(e) => handleChangeThemeMode(e.target.value as ThemeMode)}
              inputProps={{ 'aria-label': 'Theme mode' }}
              SelectDisplayProps={
                {
                  'data-testid': 'theme-mode-select',
                  'aria-label': 'Theme mode',
                } as HTMLAttributes<HTMLDivElement>
              }
              sx={{
                color: 'inherit',
                minWidth: 88,
                '.MuiSelect-select': { py: 0.5, fontSize: 13 },
                '.MuiSelect-icon': { color: 'inherit' },
                '.MuiOutlinedInput-notchedOutline': { borderColor: 'currentColor' },
                '&:hover .MuiOutlinedInput-notchedOutline': { borderColor: 'currentColor' },
              }}
            >
              <MenuItem value="auto">auto</MenuItem>
              <MenuItem value="light">light</MenuItem>
              <MenuItem value="dark">dark</MenuItem>
            </Select>
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
              {route.page === 'inbox' && (
                <Inbox me={effectiveMe} nodes={nodes.data} onInboxChanged={handleInboxChanged} />
              )}
              {route.page === 'backlog' && <Backlog me={effectiveMe} team={route.team} />}
              {route.page === 'task' && <TaskPage id={route.id} me={effectiveMe} nodes={nodes.data} />}
              {route.page === 'grainfiles' && <Grainfiles me={effectiveMe} />}
              {route.page === 'nodes' && <Nodes me={effectiveMe} />}
              {route.page === 'file' && <FileTask me={effectiveMe} team={route.team} />}
              {route.page === 'teams' && <Teams me={effectiveMe} />}
              {route.page === 'team' && <TeamPage me={effectiveMe} team={route.team} />}
              {route.page === 'deployments' && (
                <Deployments me={effectiveMe} nodes={nodes.data} onChanged={() => { me.reload(); nodes.reload(); }} />
              )}
              {route.page === 'deployment' && (
                <DeploymentPage id={route.id} me={effectiveMe} nodes={nodes.data} onChanged={() => { me.reload(); nodes.reload(); }} />
              )}
              {route.page === 'proposals' && <Proposals me={effectiveMe} team={route.team} />}
              {route.page === 'notfound' && <Alert severity="warning">No page at {route.path}.</Alert>}
            </Box>
          )}
        </Container>
      </Box>
    </ThemeProvider>
  );
}
