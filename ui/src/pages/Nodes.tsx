// Nodes shows the hub's node directory (GET /v1/nodes): connected Grain
// deployments, their recipient identities, subscribed repositories and
// processes, online/capacity status, tunneled local UI links when nodeui
// is offered, and the caller's node tokens.
import {
  Alert,
  Box,
  Chip,
  LinearProgress,
  Link as MuiLink,
  Paper,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  Typography,
} from '@mui/material';
import { listNodes, type Me, type NodeEntry } from '../api';
import { safeUrl } from '../components/Markdown';
import { NodeTokens } from '../components/NodeTokens';
import { NodeStatusChip } from '../components/StateChip';
import { useLoad } from '../load';
import { mono } from '../theme';
import { when } from './Backlog';

export function safeHref(raw?: string): string | undefined {
  if (!raw) return undefined;
  const clean = safeUrl(raw.trim());
  if (!clean || clean.startsWith('//') || clean.startsWith('/\\') || !/^(https?:|mailto:|\/[^/\\])/i.test(clean)) return undefined;
  return clean;
}

export function offersNodeUI(n: NodeEntry): boolean {
  return Boolean(n.ui?.offered || n.features?.includes('nodeui') || n.ui_url || n.ui?.url);
}

export function nodeUIHref(n: NodeEntry): string | undefined {
  if (!offersNodeUI(n) || n.status === 'offline') return undefined;
  const candidate = n.ui_url || n.ui?.url || `/nodes/${encodeURIComponent(n.id)}/ui`;
  return safeHref(candidate);
}

export function Nodes({ me }: { me?: Me }) {
  const nodes = useLoad((signal) => listNodes(signal), [], 15000);
  return (
    <Stack spacing={2}>
      <Box>
        <Typography variant="h5">Nodes</Typography>
        <Typography variant="body2" color="text.secondary">
          Connected Grain deployments, recipient identities, subscribed repositories and processes, and tunneled local UIs.
        </Typography>
      </Box>
      {nodes.error && <Alert severity="error">{nodes.error.message}</Alert>}
      <Paper>
        {nodes.loading && <LinearProgress />}
        <Table size="small">
          <TableHead>
            <TableRow>
              <TableCell>Node</TableCell>
              <TableCell>Status</TableCell>
              <TableCell>Owner</TableCell>
              <TableCell>Slots free</TableCell>
              <TableCell>Recipient(s)</TableCell>
              <TableCell>Repositories &amp; Subscriptions</TableCell>
              <TableCell>Local UI</TableCell>
              <TableCell>Version</TableCell>
              <TableCell>Last seen</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {nodes.data?.map((n) => {
              const uiHref = nodeUIHref(n);
              const hasNodeUI = offersNodeUI(n);
              return (
                <TableRow key={n.id}>
                  <TableCell sx={{ fontFamily: mono }}>{n.id}</TableCell>
                  <TableCell>
                    <NodeStatusChip status={n.status} />
                  </TableCell>
                  <TableCell>{n.owner}</TableCell>
                  <TableCell>
                    {Math.max(n.free - n.claimed, 0)} of {n.slots}
                  </TableCell>
                  <TableCell>
                    {n.recipients && n.recipients.length > 0 ? (
                      <Stack spacing={0.25}>
                        {n.recipients.map((rec) => {
                          const href = safeHref(rec);
                          return href ? (
                            <MuiLink
                              key={rec}
                              href={href}
                              target="_blank"
                              rel="noopener noreferrer"
                              underline="hover"
                              sx={{ fontFamily: mono, fontSize: 12 }}
                            >
                              {rec}
                            </MuiLink>
                          ) : (
                            <Typography key={rec} variant="body2" sx={{ fontFamily: mono, fontSize: 12 }}>
                              {rec}
                            </Typography>
                          );
                        })}
                      </Stack>
                    ) : (
                      <Typography variant="caption" color="text.secondary">
                        —
                      </Typography>
                    )}
                  </TableCell>
                  <TableCell>
                    <Stack spacing={0.5}>
                      {n.repos && n.repos.length > 0 && (
                        <Typography variant="body2">{n.repos.join(', ')}</Typography>
                      )}
                      {n.folders && n.folders.length > 0 && (
                        <Stack direction="row" spacing={0.5} flexWrap="wrap" useFlexGap>
                          {n.folders.map((folder) => (
                            <Chip
                              key={folder}
                              size="small"
                              label={`folder: ${folder.replace(/^https?:\/\/github\.com\//i, '')}`}
                              variant="outlined"
                            />
                          ))}
                        </Stack>
                      )}
                      {n.grainfiles && n.grainfiles.length > 0 && (
                        <Stack direction="row" spacing={0.5} flexWrap="wrap" useFlexGap>
                          {n.grainfiles.map((gf) => {
                            const href = safeHref(gf);
                            const label = gf.replace(/^https?:\/\/github\.com\//i, '');
                            return href ? (
                              <MuiLink
                                key={gf}
                                href={href}
                                target="_blank"
                                rel="noopener noreferrer"
                                underline="hover"
                                sx={{ fontFamily: mono, fontSize: 12 }}
                              >
                                {label}
                              </MuiLink>
                            ) : (
                              <Typography key={gf} variant="caption" sx={{ fontFamily: mono }}>
                                {label}
                              </Typography>
                            );
                          })}
                        </Stack>
                      )}
                    </Stack>
                  </TableCell>
                  <TableCell>
                    <Stack direction="row" spacing={1} sx={{ alignItems: 'center', flexWrap: 'wrap', rowGap: 0.5 }}>
                      {uiHref ? (
                        <MuiLink href={uiHref} target="_blank" rel="noopener noreferrer" underline="hover">
                          Open Grain UI
                        </MuiLink>
                      ) : hasNodeUI ? (
                        <Typography variant="caption" color="text.secondary">
                          Offline (nodeui)
                        </Typography>
                      ) : (
                        <Typography variant="caption" color="text.secondary">
                          —
                        </Typography>
                      )}
                      {n.viewer_role && <Chip size="small" label={n.viewer_role} variant="outlined" />}
                    </Stack>
                  </TableCell>
                  <TableCell>{n.version ?? ''}</TableCell>
                  <TableCell>{n.status === 'offline' ? '' : when(n.last_seen)}</TableCell>
                </TableRow>
              );
            })}
            {nodes.data?.length === 0 && (
              <TableRow>
                <TableCell colSpan={9}>
                  <Typography variant="body2" color="text.secondary">
                    No nodes.
                  </Typography>
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </Paper>
      <NodeTokens showOwner={!!me?.hub_admin} />
    </Stack>
  );
}
