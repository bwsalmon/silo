// Unified Deployments (/deployments and /deployments/:id) and Accepted Users
// governance. Any accepted user may create a deployment with an open DNS-safe
// slug and become its admin; deployment admins manage members, claim open
// repositories, configure settings, mint connection tokens, and replace the
// deployment's Grain host. Sys admins also manage the Hub's accepted users
// allowlist and sys-admin roles.
import { useState } from 'react';
import {
  Alert,
  Box,
  Button,
  Checkbox,
  Chip,
  FormControlLabel,
  LinearProgress,
  Link as MuiLink,
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
  assignDeploymentRepo,
  createDeployment,
  deleteDeployment,
  deleteUser,
  getDeployment,
  getDeploymentSettings,
  listDeployments,
  listUsers,
  mintDeploymentToken,
  releaseDeploymentRepo,
  removeDeploymentMember,
  replaceDeploymentHost,
  setDeploymentMember,
  setDeploymentSettings,
  setUser,
  type Deployment,
  type Me,
  type MintedToken,
  type NodeEntry,
  type Role,
} from '../api';
import { useAction } from '../action';
import { ConfirmButton } from '../components/ConfirmButton';
import { NodeStatusChip } from '../components/StateChip';
import { useLoad } from '../load';
import { adminOf } from '../may';
import { Link, navigate, routePath } from '../router';
import { mono } from '../theme';
import { nodeUIHref, offersNodeUI } from './Nodes';

const roles: Role[] = ['member', 'admin', 'proposer', 'viewer'];

export function canonicalDeploymentUrl(d: Pick<Deployment, 'id' | 'url' | 'recipient_url'>): string {
  if (d.recipient_url) return d.recipient_url;
  if (d.url) return d.url;
  return `${window.location.origin}/deployments/${encodeURIComponent(d.id)}`;
}

function CopyButton({ text, label = 'Copy URL', ariaLabel }: { text: string; label?: string; ariaLabel?: string }) {
  const [copied, setCopied] = useState<string>();
  const copy = () => {
    const clip = navigator.clipboard;
    if (!clip) {
      setCopied('Select and copy manually.');
      return;
    }
    clip.writeText(text).then(
      () => setCopied('Copied.'),
      () => setCopied('Could not copy.'),
    );
  };
  return (
    <Stack direction="row" spacing={1} sx={{ alignItems: 'center', display: 'inline-flex' }}>
      <Button size="small" variant="outlined" aria-label={ariaLabel ?? label} onClick={copy}>
        {label}
      </Button>
      {copied && <Typography variant="caption">{copied}</Typography>}
    </Stack>
  );
}

function CreateDeploymentForm({ onCreated }: { onCreated: (d: Deployment) => void }) {
  const [id, setId] = useState('');
  const [name, setName] = useState('');
  const create = useAction(
    async () => {
      const trimmedName = name.trim();
      const created = await createDeployment(id.trim(), trimmedName || undefined);
      setId('');
      setName('');
      onCreated(created);
    },
  );
  return (
    <Paper
      component="form"
      sx={{ p: 2 }}
      onSubmit={(e) => {
        e.preventDefault();
        void create.run();
      }}
    >
      <Typography variant="subtitle1" sx={{ mb: 0.5 }}>
        New deployment
      </Typography>
      <Typography variant="body2" color="text.secondary" sx={{ mb: 1.5 }}>
        Any accepted user may claim an open DNS-safe deployment name and administer it.
      </Typography>
      <Stack direction={{ xs: 'column', sm: 'row' }} spacing={1}>
        <TextField
          size="small"
          label="Deployment ID"
          placeholder="payments-prod"
          value={id}
          onChange={(e) => setId(e.target.value)}
        />
        <TextField
          size="small"
          label="Display name (optional)"
          placeholder="Payments Production"
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
        <Button type="submit" variant="contained" disabled={!id.trim() || create.busy}>
          Create deployment
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

function AcceptedUsersSection() {
  const users = useLoad((signal) => listUsers(signal), []);
  const [email, setEmail] = useState('');
  const [sysAdmin, setSysAdmin] = useState(false);

  const add = useAction(
    () => setUser(email.trim(), sysAdmin),
    () => {
      setEmail('');
      setSysAdmin(false);
      users.reload();
    },
  );
  const toggleSysAdmin = useAction((uEmail: string, nextSysAdmin: boolean) => setUser(uEmail, nextSysAdmin), users.reload);
  const remove = useAction((uEmail: string) => deleteUser(uEmail), users.reload);

  return (
    <Paper sx={{ p: 2 }} data-testid="accepted-users-section">
      <Typography variant="h6">Accepted users &amp; sys admins</Typography>
      <Typography variant="body2" color="text.secondary" sx={{ mb: 1.5 }}>
        Only users on the accepted users list may sign in to the Hub. Sys admins can add or remove accepted users and grant sys-admin privileges.
      </Typography>
      {users.loading && <LinearProgress sx={{ mb: 1 }} />}
      {users.error && <Alert severity="error" sx={{ mb: 1 }}>{users.error.message}</Alert>}
      {toggleSysAdmin.error && <Alert severity="error" sx={{ mb: 1 }}>{toggleSysAdmin.error}</Alert>}
      {remove.error && <Alert severity="error" sx={{ mb: 1 }}>{remove.error}</Alert>}
      <Table size="small">
        <TableHead>
          <TableRow>
            <TableCell>Email</TableCell>
            <TableCell>Sys admin</TableCell>
            <TableCell>Added by</TableCell>
            <TableCell />
          </TableRow>
        </TableHead>
        <TableBody>
          {users.data?.map((u) => (
            <TableRow key={u.email}>
              <TableCell sx={{ fontFamily: mono }}>{u.email}</TableCell>
              <TableCell>
                <Checkbox
                  size="small"
                  checked={u.sys_admin}
                  disabled={toggleSysAdmin.busy}
                  inputProps={{ 'aria-label': `Sys admin for ${u.email}` }}
                  onChange={(e) => void toggleSysAdmin.run(u.email, e.target.checked)}
                />
              </TableCell>
              <TableCell>{u.added_by || '—'}</TableCell>
              <TableCell align="right">
                <ConfirmButton
                  confirm={`Remove ${u.email}`}
                  disabled={remove.busy}
                  onConfirm={() => void remove.run(u.email)}
                >
                  Remove user
                </ConfirmButton>
              </TableCell>
            </TableRow>
          ))}
          {users.data?.length === 0 && (
            <TableRow>
              <TableCell colSpan={4}>
                <Typography variant="body2" color="text.secondary">
                  No accepted users configured.
                </Typography>
              </TableCell>
            </TableRow>
          )}
        </TableBody>
      </Table>
      <Box
        component="form"
        sx={{ mt: 2 }}
        onSubmit={(e) => {
          e.preventDefault();
          void add.run();
        }}
      >
        <Stack direction={{ xs: 'column', sm: 'row' }} spacing={1} sx={{ alignItems: { sm: 'center' } }}>
          <TextField
            size="small"
            label="User email"
            placeholder="dev@example.com"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            sx={{ minWidth: 260 }}
          />
          <FormControlLabel
            control={<Checkbox checked={sysAdmin} onChange={(e) => setSysAdmin(e.target.checked)} />}
            label="Grant sys admin"
          />
          <Button type="submit" variant="contained" disabled={!email.trim() || add.busy}>
            Add accepted user
          </Button>
        </Stack>
        {add.error && (
          <Alert severity="error" sx={{ mt: 1 }}>
            {add.error}
          </Alert>
        )}
      </Box>
    </Paper>
  );
}

function findNodeForDeployment(nodes: NodeEntry[] | undefined, id: string): NodeEntry | undefined {
  if (!nodes) return undefined;
  return nodes.find((n) => n.id === id) ?? nodes.find((n) => n.ui?.team === id);
}

export function Deployments({
  me,
  nodes,
  onChanged,
}: {
  me?: Me;
  nodes?: NodeEntry[];
  onChanged?: () => void;
}) {
  const deployments = useLoad((signal) => listDeployments(signal), []);
  const handleCreated = (created: Deployment) => {
    deployments.reload();
    onChanged?.();
    navigate(routePath({ page: 'deployment', id: created.id }));
  };
  return (
    <Stack spacing={2}>
      <Box>
        <Typography variant="h5">Deployments</Typography>
        <Typography variant="body2" color="text.secondary">
          Self-service Grain deployments, canonical Hub URLs for .task.grain.md recipients, and repository ownership.
        </Typography>
      </Box>
      {deployments.error && <Alert severity="error">{deployments.error.message}</Alert>}
      {me && <CreateDeploymentForm onCreated={handleCreated} />}
      <Paper>
        {deployments.loading && <LinearProgress />}
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Deployment</TableCell>
              <TableCell>Canonical URL / Recipient</TableCell>
              <TableCell>Host status</TableCell>
              <TableCell>Your role</TableCell>
              <TableCell>Members</TableCell>
              <TableCell>Repositories</TableCell>
              <TableCell>Backlog</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {deployments.data?.map((d) => {
              const url = canonicalDeploymentUrl(d);
              const node = findNodeForDeployment(nodes, d.id);
              const uiHref = node ? nodeUIHref(node) : undefined;
              return (
                <TableRow key={d.id}>
                  <TableCell>
                    <Link to={routePath({ page: 'deployment', id: d.id })}>
                      {d.name && d.name !== d.id ? `${d.name} (${d.id})` : d.id}
                    </Link>
                  </TableCell>
                  <TableCell>
                    <Stack direction="row" spacing={1} sx={{ alignItems: 'center', flexWrap: 'wrap' }}>
                      <Typography variant="body2" sx={{ fontFamily: mono, fontSize: 12 }}>
                        {url}
                      </Typography>
                      <CopyButton text={url} label="Copy URL" ariaLabel={`Copy URL for ${d.id}`} />
                    </Stack>
                  </TableCell>
                  <TableCell>
                    <Stack direction="row" spacing={1} sx={{ alignItems: 'center' }}>
                      <NodeStatusChip status={node?.status ?? 'offline'} />
                      {uiHref && (
                        <MuiLink href={uiHref} target="_blank" rel="noopener noreferrer" underline="hover">
                          Open Grain UI
                        </MuiLink>
                      )}
                    </Stack>
                  </TableCell>
                  <TableCell>{me?.teams?.[d.id] ?? (me?.hub_admin ? 'hub admin' : '')}</TableCell>
                  <TableCell>{Object.keys(d.members ?? {}).length}</TableCell>
                  <TableCell>{(d.repos ?? []).join(', ')}</TableCell>
                  <TableCell>
                    <Link to={routePath({ page: 'backlog', team: d.id })}>tasks</Link>
                  </TableCell>
                </TableRow>
              );
            })}
            {deployments.data?.length === 0 && (
              <TableRow>
                <TableCell colSpan={7}>
                  <Typography variant="body2" color="text.secondary">
                    No deployments.
                  </Typography>
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </Paper>
      {me?.hub_admin && <AcceptedUsersSection />}
    </Stack>
  );
}

function DeploymentHostSection({
  deploymentId,
  admin,
  onDone,
}: {
  deploymentId: string;
  admin: boolean;
  onDone: () => void;
}) {
  const [replaceHost, setReplaceHost] = useState(false);
  const [minted, setMinted] = useState<MintedToken>();
  const [copied, setCopied] = useState<string>();

  const mint = useAction(
    async () => {
      setMinted(undefined);
      setCopied(undefined);
      setMinted(await mintDeploymentToken(deploymentId, replaceHost ? { replace_host: true } : undefined));
    },
    onDone,
  );

  const replace = useAction(
    async () => {
      setMinted(undefined);
      setCopied(undefined);
      setMinted(await replaceDeploymentHost(deploymentId));
    },
    onDone,
  );

  if (!admin) return null;

  const copyToken = () => {
    if (!minted) return;
    const clip = navigator.clipboard;
    if (!clip) {
      setCopied('This browser gives the page no clipboard here: select the token and copy it.');
      return;
    }
    clip.writeText(minted.token).then(
      () => setCopied('Copied.'),
      () => setCopied('Could not copy: select the token and copy it.'),
    );
  };

  return (
    <Paper sx={{ p: 2 }} data-testid="deployment-host-section">
      <Typography variant="h6">Connect or replace Grain host</Typography>
      <Typography variant="body2" color="text.secondary" sx={{ mb: 1.5 }}>
        Mint a connection token to assign a Grain host to <b>{deploymentId}</b>, or replace an existing/lost host to revoke its token, reclaim in-flight tasks back to queued, and reset Grainfile delivery cursors for replay.
      </Typography>
      {minted && (
        <Alert
          severity="success"
          onClose={() => setMinted(undefined)}
          sx={{ mb: 2, '& .MuiAlert-message': { minWidth: 0, flex: 1 } }}
        >
          <Typography variant="body2">
            Connection token for deployment <b>{minted.node_token.node}</b>. Copy it now: it is shown only this once, and the hub keeps no copy it could show again.
          </Typography>
          {minted.replaced_host && (
            <Typography variant="body2" sx={{ mt: 0.5 }} data-testid="replace-host-summary">
              Host replaced: {(minted.reclaimed_tasks ?? []).length} in-flight task(s) reclaimed
              {(minted.reclaimed_tasks ?? []).length > 0 ? ` (${minted.reclaimed_tasks!.join(', ')})` : ''} and{' '}
              {minted.replayed_events ?? 0} Grainfile event(s) queued for replay.
            </Typography>
          )}
          <Box
            data-testid="minted-token"
            sx={{ fontFamily: mono, my: 1, p: 1, bgcolor: 'background.paper', borderRadius: 1, wordBreak: 'break-all', userSelect: 'all' }}
          >
            {minted.token}
          </Box>
          <Typography variant="caption" color="text.secondary" sx={{ display: 'block', mb: 1, fontFamily: mono }} data-testid="host-setup-command">
            SILO_HUB_TOKEN={minted.token} silo grain --hub {window.location.origin} --id {deploymentId}
          </Typography>
          <Stack direction="row" spacing={1} sx={{ alignItems: 'center' }}>
            <Button size="small" variant="contained" onClick={copyToken}>
              Copy
            </Button>
            <Button size="small" onClick={() => setMinted(undefined)}>
              Done
            </Button>
            {copied && <Typography variant="caption">{copied}</Typography>}
          </Stack>
        </Alert>
      )}
      <Stack direction={{ xs: 'column', sm: 'row' }} spacing={1.5} sx={{ alignItems: { sm: 'center' } }}>
        <FormControlLabel
          control={<Checkbox checked={replaceHost} onChange={(e) => setReplaceHost(e.target.checked)} />}
          label="Replace existing host and reclaim in-flight tasks"
        />
        <Button variant="contained" disabled={mint.busy || replace.busy} onClick={() => void mint.run()}>
          Mint connection token
        </Button>
        <Button variant="outlined" color="warning" disabled={mint.busy || replace.busy} onClick={() => void replace.run()}>
          Replace host
        </Button>
      </Stack>
      {mint.error && <Alert severity="error" sx={{ mt: 1 }}>{mint.error}</Alert>}
      {replace.error && <Alert severity="error" sx={{ mt: 1 }}>{replace.error}</Alert>}
    </Paper>
  );
}

function DeploymentMembers({
  deployment,
  admin,
  onDone,
}: {
  deployment: Deployment;
  admin: boolean;
  onDone: () => void;
}) {
  const [principal, setPrincipal] = useState('');
  const [role, setRole] = useState<Role>('member');
  const set = useAction((p: string, r: Role) => setDeploymentMember(deployment.id, p, r), onDone);
  const remove = useAction((p: string) => removeDeploymentMember(deployment.id, p), onDone);
  const add = useAction(
    () => setDeploymentMember(deployment.id, principal.trim(), role),
    () => {
      setPrincipal('');
      onDone();
    },
  );
  const members = Object.entries(deployment.members ?? {}).sort(([a], [b]) => a.localeCompare(b));
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
                  <Select
                    size="small"
                    value={r}
                    inputProps={{ 'aria-label': `Role of ${p}` }}
                    disabled={set.busy}
                    onChange={(e) => void set.run(p, e.target.value)}
                  >
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
            <Select
              size="small"
              value={role}
              inputProps={{ 'aria-label': 'New member role' }}
              onChange={(e) => setRole(e.target.value)}
            >
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

function DeploymentRepos({
  deployment,
  admin,
  onDone,
}: {
  deployment: Deployment;
  admin: boolean;
  onDone: () => void;
}) {
  const [repo, setRepo] = useState('');
  const [history, setHistory] = useState(false);
  const release = useAction((r: string) => releaseDeploymentRepo(deployment.id, r), onDone);
  const assign = useAction(
    () => assignDeploymentRepo(deployment.id, repo.trim(), history),
    () => {
      setRepo('');
      setHistory(false);
      onDone();
    },
  );
  const repos = [...(deployment.repos ?? [])].sort();
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
                  The deployment owns no repository.
                </Typography>
              </TableCell>
            </TableRow>
          )}
        </TableBody>
      </Table>
      {release.error && <Alert severity="error">{release.error}</Alert>}
      {admin && (
        <Box
          component="form"
          sx={{ mt: 2 }}
          onSubmit={(e) => {
            e.preventDefault();
            void assign.run();
          }}
        >
          <Stack direction={{ xs: 'column', sm: 'row' }} spacing={1} sx={{ alignItems: { sm: 'center' } }}>
            <TextField
              size="small"
              label="Repository"
              placeholder="owner/name"
              value={repo}
              onChange={(e) => setRepo(e.target.value)}
            />
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

function DeploymentSettingsSection({ id, admin }: { id: string; admin: boolean }) {
  const settings = useLoad((signal) => getDeploymentSettings(id, signal), [id]);
  const [value, setValue] = useState<string>();
  const [anyoneMayPropose, setAnyoneMayPropose] = useState<boolean>();
  const [requireApproval, setRequireApproval] = useState<boolean>();
  const shown = value ?? String(settings.data?.lost_requeue_after_seconds ?? 0);
  const shownAnyoneMayPropose = anyoneMayPropose ?? (settings.data?.anyone_may_propose ?? true);
  const shownRequireApproval = requireApproval ?? (settings.data?.require_approval ?? false);
  const dirty = value !== undefined || anyoneMayPropose !== undefined || requireApproval !== undefined;
  const save = useAction(
    () =>
      setDeploymentSettings(id, {
        lost_requeue_after_seconds: Number(shown),
        anyone_may_propose: shownAnyoneMayPropose,
        require_approval: shownRequireApproval,
      }),
    () => {
      setValue(undefined);
      setAnyoneMayPropose(undefined);
      setRequireApproval(undefined);
      settings.reload();
    },
  );
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
            <Stack spacing={1}>
              <Stack direction={{ xs: 'column', sm: 'row' }} spacing={1} sx={{ alignItems: { sm: 'center' } }}>
                <TextField
                  size="small"
                  type="number"
                  label="Requeue a lost task after (seconds, 0 for never)"
                  value={shown}
                  onChange={(e) => setValue(e.target.value)}
                  sx={{ minWidth: 340 }}
                />
                <Button type="submit" variant="contained" disabled={save.busy || !dirty}>
                  Save
                </Button>
              </Stack>
              <FormControlLabel
                control={<Checkbox checked={shownAnyoneMayPropose} onChange={(e) => setAnyoneMayPropose(e.target.checked)} />}
                label="Anyone may propose"
              />
              <FormControlLabel
                control={<Checkbox checked={shownRequireApproval} onChange={(e) => setRequireApproval(e.target.checked)} />}
                label="Require two-person approval for member tasks"
              />
            </Stack>
            {save.error && (
              <Alert severity="error" sx={{ mt: 1 }}>
                {save.error}
              </Alert>
            )}
          </Box>
        ) : (
          <Stack spacing={0.5}>
            <Typography variant="body2">
              A lost task is requeued {settings.data.lost_requeue_after_seconds ? `after ${settings.data.lost_requeue_after_seconds}s` : 'never'}.
            </Typography>
            <Typography variant="body2">
              {(settings.data.anyone_may_propose ?? true) ? 'Anyone may propose.' : 'Only deployment members, proposers, and admins may submit tasks.'}
            </Typography>
            <Typography variant="body2">
              {settings.data.require_approval ? 'Require two-person approval for member tasks.' : 'Member tasks are queued without approval.'}
            </Typography>
          </Stack>
        ))}
    </Paper>
  );
}

export function DeploymentPage({
  id,
  me,
  nodes,
  onChanged,
}: {
  id: string;
  me?: Me;
  nodes?: NodeEntry[];
  onChanged?: () => void;
}) {
  const d = useLoad((signal) => getDeployment(id, signal), [id]);
  const [embedUI, setEmbedUI] = useState(false);
  const del = useAction(
    () => deleteDeployment(id),
    () => {
      onChanged?.();
      navigate(routePath({ page: 'deployments' }));
    },
  );
  if (d.error && !d.data) {
    const missing = d.error instanceof ApiError && d.error.status === 404;
    return (
      <Alert severity={missing ? 'warning' : 'error'}>
        {missing ? `No deployment ${id} in your deployments.` : d.error.message}
      </Alert>
    );
  }
  if (!d.data) return <LinearProgress />;

  const admin = adminOf(me, id);
  const canonicalUrl = canonicalDeploymentUrl(d.data);
  const node = findNodeForDeployment(nodes, id);
  const uiHref = node ? nodeUIHref(node) : undefined;
  const hasNodeUI = node ? offersNodeUI(node) : false;

  const handleUpdated = () => {
    d.reload();
    onChanged?.();
  };

  return (
    <Stack spacing={2}>
      <Stack direction="row" spacing={2} sx={{ alignItems: 'center', flexWrap: 'wrap' }}>
        <Typography variant="h5">
          {d.data.name && d.data.name !== d.data.id ? `${d.data.name} (${d.data.id})` : d.data.id}
        </Typography>
        <NodeStatusChip status={node?.status ?? 'offline'} />
        <Link to={routePath({ page: 'deployments' })}>All deployments</Link>
        <Link to={routePath({ page: 'backlog', team: id })}>Backlog</Link>
        <Link to={routePath({ page: 'proposals', team: id })}>Proposals</Link>
        <Link to={routePath({ page: 'file', team: id })}>File a task</Link>
        <Box sx={{ flex: 1 }} />
        {admin && (
          <ConfirmButton confirm={`Delete ${id}`} disabled={del.busy} onConfirm={() => void del.run()}>
            Delete deployment
          </ConfirmButton>
        )}
      </Stack>
      {del.error && <Alert severity="error">{del.error}</Alert>}
      {d.error && <Alert severity="error">{d.error.message}</Alert>}

      <Paper sx={{ p: 2 }}>
        <Typography variant="h6">Canonical deployment URL &amp; Grainfile recipient</Typography>
        <Typography variant="body2" color="text.secondary" sx={{ mb: 1 }}>
          This URL both opens <b>{d.data.id}</b> in the Hub and serves as the canonical <code>recipient:</code> URL in <code>.task.grain.md</code> files.
        </Typography>
        <Stack direction={{ xs: 'column', sm: 'row' }} spacing={1.5} sx={{ alignItems: { sm: 'center' } }}>
          <Box
            data-testid="canonical-deployment-url"
            sx={{ fontFamily: mono, p: 1, bgcolor: 'background.default', borderRadius: 1, wordBreak: 'break-all' }}
          >
            {canonicalUrl}
          </Box>
          <CopyButton text={canonicalUrl} label="Copy URL" />
          <Chip size="small" variant="outlined" label={`recipient: ${canonicalUrl}`} sx={{ fontFamily: mono }} />
        </Stack>
      </Paper>

      {(uiHref || hasNodeUI) && (
        <Paper sx={{ p: 2 }} data-testid="deployment-grain-ui-section">
          <Stack direction="row" spacing={1.5} sx={{ alignItems: 'center', flexWrap: 'wrap' }}>
            <Typography variant="h6">Grain deployment UI</Typography>
            {uiHref ? (
              <>
                <MuiLink
                  href={uiHref}
                  target="_blank"
                  rel="noopener noreferrer"
                  underline="hover"
                  data-testid="open-deployment-grain-ui"
                >
                  Open Grain UI
                </MuiLink>
                <Button size="small" variant="outlined" onClick={() => setEmbedUI((v) => !v)}>
                  {embedUI ? 'Hide embedded Grain UI' : 'Embed Grain UI'}
                </Button>
              </>
            ) : (
              <Typography variant="body2" color="text.secondary">
                Host is currently offline (tunneled Grain UI unavailable).
              </Typography>
            )}
          </Stack>
          {uiHref && embedUI && (
            <Box sx={{ mt: 2, border: 1, borderColor: 'divider', borderRadius: 1, overflow: 'hidden', height: 520 }}>
              <iframe
                title={`${d.data.id} Grain UI`}
                src={uiHref}
                data-testid="deployment-grain-ui-frame"
                style={{ width: '100%', height: '100%', border: 0 }}
              />
            </Box>
          )}
        </Paper>
      )}

      <DeploymentHostSection deploymentId={id} admin={admin} onDone={handleUpdated} />
      <DeploymentMembers deployment={d.data} admin={admin} onDone={handleUpdated} />
      <DeploymentRepos deployment={d.data} admin={admin} onDone={handleUpdated} />
      <DeploymentSettingsSection id={id} admin={admin} />
    </Stack>
  );
}
