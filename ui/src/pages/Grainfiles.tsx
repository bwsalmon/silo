// Grainfiles lists indexed .task.md tasks and .process.md standing
// processes across watched repositories, filters them by kind,
// recipient, repo, and draft state, and inspects a selected Grainfile's
// resolved front-matter metadata, node subscriptions, and recent trigger
// or execution events.
import { useMemo, useState } from 'react';
import {
  Alert,
  Box,
  Button,
  Chip,
  Divider,
  LinearProgress,
  Link as MuiLink,
  MenuItem,
  Paper,
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
  getGrainfilesOverview,
  type EventFilter,
  type Grainfile,
  type GrainfileEvent,
  type GrainfileKind,
  type GrainfileQuery,
  type GrainfileSubscriptions,
  type GrainfileTriggers,
  type Me,
} from '../api';
import { safeUrl } from '../components/Markdown';
import { GrainfileKindChip } from '../components/StateChip';
import { useLoad } from '../load';
import { mono } from '../theme';
import { when } from './Backlog';

function linkableHref(raw?: string): string | undefined {
  if (!raw) return undefined;
  const clean = safeUrl(raw.trim());
  if (!clean || clean.startsWith('//') || clean.startsWith('/\\') || !/^(https?:|mailto:|\/[^/\\])/i.test(clean)) return undefined;
  return clean;
}

function shortRepo(repo: string): string {
  return repo.replace(/^https?:\/\/github\.com\//i, '').replace(/\/+$/, '');
}

function shortTargetLabel(url: string): string {
  const trimmed = url.replace(/^https?:\/\/github\.com\//i, '').replace(/\/+$/, '');
  const blobMatch = /^([^/]+\/[^/]+)\/(?:blob|tree)\/[^/]+\/(.+)$/.exec(trimmed);
  if (blobMatch) {
    return `${blobMatch[1]}:${blobMatch[2]}`;
  }
  return trimmed;
}

function formatEventFilter(name: string, f?: EventFilter): string | null {
  if (!f) return null;
  const details: string[] = [];
  if (f.branches && f.branches.length > 0) details.push(`branches: ${f.branches.join(', ')}`);
  if (f.paths && f.paths.length > 0) details.push(`paths: ${f.paths.join(', ')}`);
  if (f.ignore && f.ignore.length > 0) details.push(`ignore: ${f.ignore.join(', ')}`);
  if (f.types && f.types.length > 0) details.push(`types: ${f.types.join(', ')}`);
  if (f.action) details.push(`action: ${f.action}`);
  if (f.base) details.push(`base: ${f.base}`);
  return details.length > 0 ? `${name} (${details.join('; ')})` : name;
}

export function summarizeTriggers(on?: GrainfileTriggers): string[] {
  if (!on) return [];
  const items: string[] = [];
  if (on.schedule) {
    const schedules = Array.isArray(on.schedule) ? on.schedule : [on.schedule];
    for (const s of schedules) {
      if (s?.cron) {
        items.push(`schedule: ${s.cron}${s.timezone ? ` (${s.timezone})` : ''}`);
      }
    }
  }
  for (const [label, filter] of [
    ['push', on.push],
    ['merge', on.merge],
    ['pull_request', on.pull_request],
    ['workflow_run', on.workflow_run],
    ['release', on.release],
  ] as const) {
    const formatted = formatEventFilter(label, filter);
    if (formatted) items.push(formatted);
  }
  if (on.manual) {
    const inputKeys = on.manual.inputs ? Object.keys(on.manual.inputs) : [];
    items.push(inputKeys.length > 0 ? `manual (${inputKeys.join(', ')})` : 'manual');
  }
  if (on.scenario) {
    const scenarios = Array.isArray(on.scenario) ? on.scenario : [on.scenario];
    for (const sc of scenarios) {
      const desc = sc?.description || sc?.ref || sc?.path || 'scenario';
      items.push(`scenario: ${desc}`);
    }
  }
  return items;
}

function SafeExternalLink({ url, label, sx }: { url: string; label?: string; sx?: object }) {
  const href = linkableHref(url);
  const text = label ?? url;
  if (!href) {
    return (
      <Typography component="span" variant="body2" sx={sx}>
        {text}
      </Typography>
    );
  }
  return (
    <MuiLink href={href} target="_blank" rel="noopener noreferrer" color="text.primary" underline="hover" sx={sx}>
      {text}
    </MuiLink>
  );
}

function matchesSubscription(sub: GrainfileSubscriptions, g: Grainfile): boolean {
  if (g.recipient && sub.recipients?.includes(g.recipient)) return true;
  const gRepo = shortRepo(g.repo).toLowerCase();
  if (sub.repos?.some((r) => shortRepo(r).toLowerCase() === gRepo)) return true;
  if (sub.grainfiles?.includes(g.url) || sub.grainfiles?.includes(g.path)) return true;
  if (
    sub.folders?.some((f) => {
      const prefix = f.replace(/\/+$/, '');
      return g.url.startsWith(prefix) || g.path.startsWith(prefix.replace(/^.*\/tree\/[^/]+\//, ''));
    })
  ) {
    return true;
  }
  return false;
}

function matchesEvent(ev: GrainfileEvent, g: Grainfile): boolean {
  return (
    ev.grainfile_url === g.url ||
    (ev.path === g.path && shortRepo(ev.repo).toLowerCase() === shortRepo(g.repo).toLowerCase()) ||
    ev.dependency_url === g.url ||
    ev.triggered_by === g.url
  );
}

export function Grainfiles({ me: _me }: { me?: Me }) {
  const [kindFilter, setKindFilter] = useState<'all' | GrainfileKind>('all');
  const [draftFilter, setDraftFilter] = useState<'all' | 'active' | 'draft'>('all');
  const [recipientFilter, setRecipientFilter] = useState('');
  const [repoFilter, setRepoFilter] = useState('');
  const [selectedUrl, setSelectedUrl] = useState<string | null>(null);

  const query = useMemo<GrainfileQuery>(() => {
    const q: GrainfileQuery = { include_drafts: true };
    if (kindFilter !== 'all') q.kind = kindFilter;
    if (draftFilter === 'active') q.draft = false;
    if (draftFilter === 'draft') q.draft = true;
    if (recipientFilter.trim()) q.recipient = recipientFilter.trim();
    if (repoFilter.trim()) q.repo = repoFilter.trim();
    return q;
  }, [kindFilter, draftFilter, recipientFilter, repoFilter]);

  const overview = useLoad(
    (signal) => getGrainfilesOverview(query, signal),
    [query.kind, query.draft, query.recipient, query.repo],
    15000,
  );

  const grainfiles = overview.data?.grainfiles ?? [];
  const subscriptions = overview.data?.subscriptions ?? [];
  const events = overview.data?.events ?? [];

  const selected = useMemo<Grainfile | null>(() => {
    if (grainfiles.length === 0) return null;
    if (selectedUrl) {
      const found = grainfiles.find((g) => g.url === selectedUrl);
      if (found) return found;
    }
    return grainfiles[0];
  }, [grainfiles, selectedUrl]);

  const selectedEvents = useMemo(
    () => (selected ? events.filter((e) => matchesEvent(e, selected)) : events),
    [events, selected],
  );

  const selectedSubscriptions = useMemo(
    () => (selected ? subscriptions.filter((s) => matchesSubscription(s, selected)) : []),
    [subscriptions, selected],
  );

  const selectedTriggers = useMemo(
    () => (selected ? summarizeTriggers(selected.when ?? selected.on ?? selected.on_spec) : []),
    [selected],
  );

  return (
    <Stack spacing={2.5}>
      <Stack direction="row" alignItems="center" justifyContent="space-between" flexWrap="wrap" gap={1}>
        <Box>
          <Typography variant="h5">Grainfiles &amp; Processes</Typography>
          <Typography variant="body2" color="text.secondary">
            Indexed <code>.task.grain.md</code> tasks and <code>.process.grain.md</code> standing processes across watched repositories.
          </Typography>
        </Box>
        <Button size="small" variant="outlined" onClick={overview.reload}>
          Refresh
        </Button>
      </Stack>

      <Paper sx={{ p: 2 }}>
        <Stack direction={{ xs: 'column', md: 'row' }} spacing={1.5} alignItems={{ md: 'center' }}>
          <TextField
            select
            size="small"
            label="Kind"
            value={kindFilter}
            onChange={(e) => setKindFilter(e.target.value as 'all' | GrainfileKind)}
            sx={{ minWidth: 160 }}
          >
            <MenuItem value="all">All kinds</MenuItem>
            <MenuItem value="task">Tasks (.task.grain.md)</MenuItem>
            <MenuItem value="process">Processes (.process.grain.md)</MenuItem>
          </TextField>
          <TextField
            select
            size="small"
            label="Draft status"
            value={draftFilter}
            onChange={(e) => setDraftFilter(e.target.value as 'all' | 'active' | 'draft')}
            sx={{ minWidth: 160 }}
          >
            <MenuItem value="all">All</MenuItem>
            <MenuItem value="active">Active only</MenuItem>
            <MenuItem value="draft">Drafts only</MenuItem>
          </TextField>
          <TextField
            size="small"
            label="Recipient deployment"
            placeholder="https://github.com/acme/grain-eu"
            value={recipientFilter}
            onChange={(e) => setRecipientFilter(e.target.value)}
            sx={{ flex: 1 }}
          />
          <TextField
            size="small"
            label="Repository"
            placeholder="acme/app"
            value={repoFilter}
            onChange={(e) => setRepoFilter(e.target.value)}
            sx={{ flex: 1 }}
          />
        </Stack>
      </Paper>

      {overview.error && <Alert severity="error">{overview.error.message}</Alert>}

      <Paper>
        {overview.loading && <LinearProgress />}
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Kind</TableCell>
              <TableCell>Title &amp; Status</TableCell>
              <TableCell>Source file</TableCell>
              <TableCell>Recipient &amp; Requestor</TableCell>
              <TableCell>Triggers &amp; Gate</TableCell>
              <TableCell>Dependencies</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {grainfiles.map((g) => {
              const triggers = summarizeTriggers(g.when ?? g.on ?? g.on_spec);
              const isSelected = selected?.url === g.url;
              return (
                <TableRow
                  key={g.url}
                  hover
                  selected={isSelected}
                  onClick={() => setSelectedUrl(g.url)}
                  sx={{ cursor: 'pointer' }}
                >
                  <TableCell>
                    <GrainfileKindChip kind={g.kind} />
                  </TableCell>
                  <TableCell>
                    <Stack spacing={0.5}>
                      <Typography variant="body2" sx={{ fontWeight: 600 }}>
                        {g.title}
                      </Typography>
                      <Stack direction="row" spacing={0.5} flexWrap="wrap" useFlexGap>
                        {g.draft && <Chip size="small" label="Draft" color="warning" />}
                        {g.not_before && (
                          <Chip
                            size="small"
                            label={`Not before ${g.not_before}`}
                            variant="outlined"
                          />
                        )}
                      </Stack>
                    </Stack>
                  </TableCell>
                  <TableCell>
                    <Stack spacing={0.25}>
                      <SafeExternalLink
                        url={g.url}
                        label={`${shortRepo(g.repo)}/${g.path}`}
                        sx={{ fontFamily: mono, fontSize: 12 }}
                      />
                      {g.ref && (
                        <Typography variant="caption" color="text.secondary" sx={{ fontFamily: mono }}>
                          ref: {g.ref}
                        </Typography>
                      )}
                    </Stack>
                  </TableCell>
                  <TableCell>
                    <Stack spacing={0.25}>
                      {g.recipient ? (
                        <SafeExternalLink url={g.recipient} sx={{ fontFamily: mono, fontSize: 12 }} />
                      ) : (
                        <Typography variant="caption" color="text.secondary">
                          —
                        </Typography>
                      )}
                      {g.requestor && (
                        <Typography variant="caption" color="text.secondary">
                          Requestor: {g.requestor}
                        </Typography>
                      )}
                    </Stack>
                  </TableCell>
                  <TableCell>
                    <Stack direction="row" spacing={0.5} flexWrap="wrap" useFlexGap alignItems="center">
                      {g.gate && <Chip size="small" label="Gate" color="success" />}
                      {triggers.map((t) => (
                        <Chip key={t} size="small" label={t} variant="outlined" />
                      ))}
                      {!g.gate && triggers.length === 0 && (
                        <Typography variant="caption" color="text.secondary">
                          —
                        </Typography>
                      )}
                    </Stack>
                  </TableCell>
                  <TableCell>
                    <Stack spacing={0.5}>
                      {g.depends_on?.map((dep) => (
                        <Box key={`dep-${dep}`}>
                          <Typography variant="caption" color="text.secondary">
                            depends_on:{' '}
                          </Typography>
                          <SafeExternalLink
                            url={dep}
                            label={shortTargetLabel(dep)}
                            sx={{ fontFamily: mono, fontSize: 12 }}
                          />
                        </Box>
                      ))}
                      {(!g.depends_on || g.depends_on.length === 0) && (
                        <Typography variant="caption" color="text.secondary">
                          —
                        </Typography>
                      )}
                    </Stack>
                  </TableCell>
                </TableRow>
              );
            })}
            {grainfiles.length === 0 && !overview.loading && (
              <TableRow>
                <TableCell colSpan={6}>
                  <Typography variant="body2" color="text.secondary">
                    No indexed Grainfiles match the current filters.
                  </Typography>
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </Paper>

      {selected && (
        <Paper data-testid="grainfile-detail" sx={{ p: 2.5 }}>
          <Stack spacing={2}>
            <Stack direction="row" alignItems="center" justifyContent="space-between" flexWrap="wrap" gap={1}>
              <Stack direction="row" spacing={1} alignItems="center" flexWrap="wrap" useFlexGap>
                <GrainfileKindChip kind={selected.kind} />
                <Typography variant="h6">{selected.title}</Typography>
                {selected.draft && <Chip size="small" label="Draft" color="warning" />}
                {selected.gate && <Chip size="small" label="Gate" color="success" />}
                {selected.not_before && (
                  <Chip size="small" label={`Not before ${selected.not_before}`} variant="outlined" />
                )}
              </Stack>
              <SafeExternalLink url={selected.url} label={selected.url} sx={{ fontFamily: mono, fontSize: 12 }} />
            </Stack>

            <Divider />

            <Stack direction={{ xs: 'column', md: 'row' }} spacing={3}>
              <Stack spacing={1} sx={{ flex: 1 }}>
                <Typography variant="subtitle2">Grainfile metadata</Typography>
                <Typography variant="body2">
                  <strong>Repository:</strong>{' '}
                  <SafeExternalLink url={selected.repo} label={shortRepo(selected.repo)} sx={{ fontFamily: mono }} />
                </Typography>
                <Typography variant="body2">
                  <strong>Path:</strong> <code>{selected.path}</code>
                </Typography>
                {selected.grainfile && (
                  <Typography variant="body2">
                    <strong>Format version:</strong> <code>{selected.grainfile}</code>
                    {selected.spec_summary_path ? (
                      <>
                        {' '}
                        (<code>{selected.spec_summary_path}</code>)
                      </>
                    ) : null}
                  </Typography>
                )}
                {selected.recipient && (
                  <Typography variant="body2">
                    <strong>Recipient:</strong>{' '}
                    <SafeExternalLink url={selected.recipient} sx={{ fontFamily: mono }} />
                  </Typography>
                )}
                {selected.requestor && (
                  <Typography variant="body2">
                    <strong>Requestor:</strong> {selected.requestor}
                  </Typography>
                )}
                {selected.created_at && (
                  <Typography variant="body2">
                    <strong>Created at:</strong> {selected.created_at}
                  </Typography>
                )}
                {selected.not_before && (
                  <Typography variant="body2">
                    <strong>Not before:</strong> {selected.not_before}
                  </Typography>
                )}
                {selected.commit_sha && (
                  <Typography variant="body2">
                    <strong>Commit:</strong> <code>{selected.commit_sha}</code>
                  </Typography>
                )}
                {selectedTriggers.length > 0 && (
                  <Box>
                    <Typography variant="body2" sx={{ mb: 0.5 }}>
                      <strong>Triggers (when):</strong>
                    </Typography>
                    <Stack direction="row" spacing={0.5} flexWrap="wrap" useFlexGap>
                      {selectedTriggers.map((t) => (
                        <Chip key={t} size="small" label={t} variant="outlined" />
                      ))}
                    </Stack>
                  </Box>
                )}
                {selectedSubscriptions.length > 0 && (
                  <Box>
                    <Typography variant="body2">
                      <strong>Subscribed deployments:</strong>{' '}
                      {selectedSubscriptions.map((s) => s.node_id).join(', ')}
                    </Typography>
                  </Box>
                )}
              </Stack>

              <Stack spacing={1} sx={{ flex: 1 }}>
                <Typography variant="subtitle2">References &amp; relationships</Typography>
                <Box>
                  <Typography variant="caption" color="text.secondary">
                    references
                  </Typography>
                  {selected.references && selected.references.length > 0 ? (
                    <Stack spacing={0.5}>
                      {selected.references.map((ref) => {
                        const isAuthorized = Boolean(
                          selected.authorized_references?.includes(ref) ||
                            ref.startsWith('/') ||
                            ref.startsWith('./') ||
                            shortRepo(ref).toLowerCase().startsWith(shortRepo(selected.repo).toLowerCase() + '/'),
                        );
                        return (
                          <Stack key={ref} direction="row" spacing={1} alignItems="center" flexWrap="wrap" useFlexGap>
                            <SafeExternalLink url={ref} sx={{ fontFamily: mono, fontSize: 12 }} />
                            <Chip
                              size="small"
                              label={isAuthorized ? 'Authorized (same-repo)' : 'External'}
                              color={isAuthorized ? 'success' : 'default'}
                              variant={isAuthorized ? 'filled' : 'outlined'}
                            />
                          </Stack>
                        );
                      })}
                    </Stack>
                  ) : (
                    <Typography variant="body2" color="text.secondary">
                      None
                    </Typography>
                  )}
                </Box>
                <Box>
                  <Typography variant="caption" color="text.secondary">
                    authorized_references (same-repo)
                  </Typography>
                  {selected.authorized_references && selected.authorized_references.length > 0 ? (
                    <Stack spacing={0.25}>
                      {selected.authorized_references.map((ref) => (
                        <SafeExternalLink key={ref} url={ref} sx={{ fontFamily: mono, fontSize: 12 }} />
                      ))}
                    </Stack>
                  ) : (
                    <Typography variant="body2" color="text.secondary">
                      None
                    </Typography>
                  )}
                </Box>
                {selected.depends_on && selected.depends_on.length > 0 && (
                  <Box>
                    <Typography variant="caption" color="text.secondary">
                      depends_on
                    </Typography>
                    <Stack spacing={0.25}>
                      {selected.depends_on.map((dep) => (
                        <SafeExternalLink key={dep} url={dep} sx={{ fontFamily: mono, fontSize: 12 }} />
                      ))}
                    </Stack>
                  </Box>
                )}
              </Stack>
            </Stack>

            <Divider />

            <Stack spacing={1}>
              <Typography variant="subtitle2">Recent trigger &amp; execution events</Typography>
              {selectedEvents.length > 0 ? (
                <Table size="small">
                  <TableHead>
                    <TableRow>
                      <TableCell>Event</TableCell>
                      <TableCell>Source / Details</TableCell>
                      <TableCell>Recipient</TableCell>
                      <TableCell>When</TableCell>
                    </TableRow>
                  </TableHead>
                  <TableBody>
                    {selectedEvents.map((ev, idx) => (
                      <TableRow key={ev.seq ?? ev.id ?? idx}>
                        <TableCell>
                          <Chip size="small" label={ev.type} variant="outlined" />
                        </TableCell>
                        <TableCell>
                          <Stack spacing={0.25}>
                            {ev.trigger_source && (
                              <Typography variant="body2">trigger: {ev.trigger_source}</Typography>
                            )}
                            {ev.triggered_by && (
                              <Typography variant="caption" sx={{ fontFamily: mono }}>
                                triggered_by: {shortTargetLabel(ev.triggered_by)}
                              </Typography>
                            )}
                            {ev.dependency_url && (
                              <Typography variant="caption" sx={{ fontFamily: mono }}>
                                dependency: {shortTargetLabel(ev.dependency_url)} ({ev.dependency_state ?? 'done'})
                              </Typography>
                            )}
                            {ev.reason && (
                              <Typography variant="caption" color="text.secondary">
                                {ev.reason}
                              </Typography>
                            )}
                            {!ev.trigger_source && !ev.triggered_by && !ev.dependency_url && !ev.reason && (
                              <Typography variant="body2" color="text.secondary">
                                {ev.title || ev.path}
                              </Typography>
                            )}
                          </Stack>
                        </TableCell>
                        <TableCell sx={{ fontFamily: mono, fontSize: 12 }}>{ev.recipient ?? '—'}</TableCell>
                        <TableCell>{ev.created_at ? when(ev.created_at) : '—'}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              ) : (
                <Typography variant="body2" color="text.secondary">
                  No recent events recorded for this Grainfile.
                </Typography>
              )}
            </Stack>
          </Stack>
        </Paper>
      )}
    </Stack>
  );
}
