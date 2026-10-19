// Unified Inbox across the caller's selected Grain deployments
// (GET /v1/inbox, GET/PUT /v1/me/inbox): actionable items awaiting
// human input across deployments — interactive questions
// (awaiting_reply), in-run tool approvals (approval_request), task
// proposals awaiting team approval (proposal), and failed tasks
// (failed) — with inline quick actions and direct links into each
// deployment's proxied Grain UI.
import { useEffect, useState } from 'react';
import {
  Alert,
  Box,
  Button,
  Chip,
  LinearProgress,
  Link as MuiLink,
  Paper,
  Stack,
  Typography,
} from '@mui/material';
import {
  INBOX_KINDS,
  listInbox,
  requeueTask,
  setInboxPreferences,
  type InboxItem,
  type InboxKind,
  type Me,
  type NodeEntry,
  type Task,
} from '../api';
import { useAction } from '../action';
import { ApprovalPrompt } from '../components/ApprovalPrompt';
import Markdown from '../components/Markdown';
import { ProposalActions } from '../components/ProposalActions';
import { ReplyBox } from '../components/Session';
import { StateChip } from '../components/StateChip';
import { useLoad } from '../load';
import { managesTask, mayApprove, mayMessage, memberOf } from '../may';
import { Link, routePath } from '../router';
import { mono } from '../theme';
import { when } from './Backlog';
import { nodeUIHref, safeHref } from './Nodes';

const kindLabels: Record<InboxKind, string> = {
  awaiting_reply: 'awaiting_reply',
  approval_request: 'approval_request',
  proposal: 'proposal',
  failed: 'failed',
};

function defaultSelectedNodes(me?: Me, nodes?: NodeEntry[]): string[] {
  if (me?.inbox_configured) {
    return me.inbox_nodes ?? [];
  }
  if (me?.inbox_nodes && me.inbox_nodes.length > 0) {
    return me.inbox_nodes;
  }
  if (!nodes || nodes.length === 0) return [];
  const memberRoles = new Set(['owner', 'admin', 'team-admin', 'member']);
  const filtered = nodes.filter((n) => {
    if (n.viewer_role) return memberRoles.has(n.viewer_role);
    if (me?.email && n.owner && n.owner.toLowerCase() === me.email.toLowerCase()) return true;
    const team = n.ui?.team;
    if (team && (me?.teams?.[team] === 'admin' || me?.teams?.[team] === 'member')) return true;
    return false;
  });
  return (filtered.length > 0 ? filtered : nodes).map((n) => n.id);
}

function itemAsTask(item: InboxItem): Task {
  return {
    id: item.task_id,
    team: item.team,
    repo: item.repo,
    kind: item.task_kind === 'interactive' || item.kind === 'awaiting_reply' ? 'interactive' : undefined,
    title: item.title,
    prompt: item.summary ?? '',
    submitter: item.submitter ?? item.proposer ?? '',
    created_at: item.updated_at,
    updated_at: item.updated_at,
    state: item.state,
    node: item.node_id,
    reason: item.reason,
    attempts: 1,
  };
}

function FailedItemActions({ item, onDone }: { item: InboxItem; onDone: () => void }) {
  const requeue = useAction(() => requeueTask(item.task_id), onDone);
  return (
    <Stack spacing={1} sx={{ mt: 1 }}>
      <Stack direction="row" spacing={1}>
        <Button size="small" variant="outlined" disabled={requeue.busy} onClick={() => void requeue.run()}>
          Requeue
        </Button>
      </Stack>
      {requeue.error && <Alert severity="error">{requeue.error}</Alert>}
    </Stack>
  );
}

function InboxCard({
  item,
  me,
  nodes,
  onDone,
}: {
  item: InboxItem;
  me?: Me;
  nodes?: NodeEntry[];
  onDone: () => void;
}) {
  const t = itemAsTask(item);
  const nodeId = item.node_id;
  const matchedNode = nodeId ? nodes?.find((n) => n.id === nodeId) : undefined;
  const uiHref = (matchedNode ? nodeUIHref(matchedNode) : undefined) || safeHref(item.node_ui_url);
  const canAct = item.can_act;

  return (
    <Paper sx={{ p: 2 }} data-testid={`inbox-item-${t.id}`}>
      <Stack spacing={1}>
        <Stack direction="row" spacing={1} sx={{ alignItems: 'center', justifyContent: 'space-between', flexWrap: 'wrap', rowGap: 0.5 }}>
          <Stack direction="row" spacing={1} sx={{ alignItems: 'center', flexWrap: 'wrap', rowGap: 0.5 }}>
            <Chip size="small" label={kindLabels[item.kind]} color="primary" variant="outlined" />
            <StateChip state={t.state} />
            <Typography variant="subtitle1" sx={{ fontWeight: 600 }}>
              <Link to={routePath({ page: 'task', id: t.id })}>{t.title || t.id}</Link>
            </Typography>
          </Stack>
          {nodeId && uiHref && (
            <MuiLink
              href={uiHref}
              target="_blank"
              rel="noopener noreferrer"
              underline="hover"
              data-testid="open-in-node-ui"
            >
              {`Open in ${nodeId} Grain UI`}
            </MuiLink>
          )}
        </Stack>

        <Stack direction="row" spacing={2} sx={{ flexWrap: 'wrap', rowGap: 0.5 }}>
          <Typography variant="caption" color="text.secondary">
            Team: <Link to={routePath({ page: 'backlog', team: t.team })}>{t.team}</Link>
          </Typography>
          {t.repo && (
            <Typography variant="caption" color="text.secondary">
              Repository: {t.repo}
            </Typography>
          )}
          {nodeId && (
            <Typography variant="caption" color="text.secondary" sx={{ fontFamily: mono }}>
              Deployment: {nodeId}
            </Typography>
          )}
          {t.submitter && (
            <Typography variant="caption" color="text.secondary">
              Submitter: {t.submitter}
            </Typography>
          )}
          <Typography variant="caption" color="text.secondary">
            Updated: {when(t.updated_at)}
          </Typography>
        </Stack>

        {item.kind === 'awaiting_reply' && (item.question || item.summary) && (
          <Box sx={{ pl: 1, borderLeft: 2, borderColor: 'warning.main' }}>
            <Markdown>{item.question || item.summary || ''}</Markdown>
          </Box>
        )}

        {item.kind === 'approval_request' && (item.tool || item.summary) && (
          <Box sx={{ pl: 1, borderLeft: 2, borderColor: 'warning.main' }}>
            <Typography variant="body2" sx={{ fontFamily: mono }}>
              {item.summary || `${item.tool ?? 'approval'}`}
            </Typography>
          </Box>
        )}

        {item.kind === 'proposal' && item.summary && (
          <Typography variant="body2" color="text.secondary">
            {item.summary}
          </Typography>
        )}

        {item.kind === 'failed' && (item.reason || item.summary) && (
          <Typography variant="body2" color="error">
            {item.reason || item.summary}
          </Typography>
        )}

        {item.kind === 'awaiting_reply' && (canAct ?? mayMessage(me, t)) && <ReplyBox task={t.id} onSent={onDone} />}

        {item.kind === 'approval_request' && item.approval_id && (
          <ApprovalPrompt
            task={t.id}
            request={item.approval_id}
            mayAnswer={canAct ?? memberOf(me, t.team)}
            onDone={onDone}
          />
        )}

        {item.kind === 'proposal' && (canAct ?? mayApprove(me, t)) && <ProposalActions t={t} onDone={onDone} />}

        {item.kind === 'failed' && (canAct ?? managesTask(me, t)) && <FailedItemActions item={item} onDone={onDone} />}
      </Stack>
    </Paper>
  );
}

export function Inbox({
  me,
  nodes,
  onInboxChanged,
}: {
  me?: Me;
  nodes?: NodeEntry[];
  onInboxChanged?: () => void;
}) {
  const [kind, setKind] = useState<InboxKind | undefined>(undefined);
  const [configuredOverride, setConfiguredOverride] = useState<string[] | undefined>(undefined);
  const [prefError, setPrefError] = useState<string | undefined>(undefined);

  useEffect(() => {
    setConfiguredOverride(undefined);
  }, [me?.email, me?.inbox_configured, me?.inbox_nodes?.join(',')]);

  const selectedNodes = configuredOverride ?? defaultSelectedNodes(me, nodes);
  const queryNodes = configuredOverride !== undefined ? configuredOverride : undefined;

  const inbox = useLoad(
    (signal) => listInbox({ kind, nodes: queryNodes }, signal),
    [kind ?? '', queryNodes ? queryNodes.join(',') : 'default', me?.email ?? ''],
    10000,
  );

  const handleDone = () => {
    inbox.reload();
    onInboxChanged?.();
  };

  const handleToggleDeployment = async (nodeId: string) => {
    setPrefError(undefined);
    const current = new Set(selectedNodes);
    if (current.has(nodeId)) {
      current.delete(nodeId);
    } else {
      current.add(nodeId);
    }
    const next = Array.from(current).sort();
    setConfiguredOverride(next);
    try {
      const saved = await setInboxPreferences(next);
      setConfiguredOverride(saved.nodes);
      inbox.reload();
      onInboxChanged?.();
    } catch (e) {
      setPrefError(e instanceof Error ? e.message : String(e));
    }
  };

  return (
    <Stack spacing={2}>
      <Stack direction="row" spacing={2} sx={{ alignItems: 'center', justifyContent: 'space-between', flexWrap: 'wrap', rowGap: 1 }}>
        <Box>
          <Typography variant="h5">Unified Inbox</Typography>
          <Typography variant="body2" color="text.secondary">
            Questions, tool approvals, task proposals, and failed tasks across your selected Grain deployments.
          </Typography>
        </Box>
        <Box sx={{ display: 'flex', gap: 0.5, flexWrap: 'wrap' }}>
          <Chip
            size="small"
            label="all"
            color={kind === undefined ? 'primary' : 'default'}
            variant={kind === undefined ? 'filled' : 'outlined'}
            onClick={() => setKind(undefined)}
            data-testid="inbox-kind-all"
          />
          {INBOX_KINDS.map((k) => (
            <Chip
              key={k}
              size="small"
              label={kindLabels[k]}
              color={kind === k ? 'primary' : 'default'}
              variant={kind === k ? 'filled' : 'outlined'}
              onClick={() => setKind(kind === k ? undefined : k)}
              data-testid={`inbox-kind-${k}`}
            />
          ))}
        </Box>
      </Stack>

      <Paper sx={{ p: 1.5 }} data-testid="inbox-deployment-selector">
        <Stack direction="row" spacing={1} sx={{ alignItems: 'center', flexWrap: 'wrap', rowGap: 0.5 }}>
          <Typography variant="body2" color="text.secondary">
            Deployments:
          </Typography>
          {nodes && nodes.length > 0 ? (
            nodes.map((n) => {
              const selected = selectedNodes.includes(n.id);
              const label = n.viewer_role ? `${n.id} (${n.viewer_role})` : n.id;
              return (
                <Chip
                  key={n.id}
                  size="small"
                  label={label}
                  color={selected ? 'primary' : 'default'}
                  variant={selected ? 'filled' : 'outlined'}
                  onClick={() => void handleToggleDeployment(n.id)}
                  data-testid={`inbox-deployment-${n.id}`}
                />
              );
            })
          ) : (
            <Typography variant="caption" color="text.secondary">
              All accessible deployments
            </Typography>
          )}
        </Stack>
      </Paper>

      {prefError && <Alert severity="error">{prefError}</Alert>}
      {inbox.error && <Alert severity="error">{inbox.error.message}</Alert>}
      {inbox.loading && <LinearProgress />}

      <Stack spacing={1.5}>
        {inbox.data?.map((item) => (
          <InboxCard key={item.id} item={item} me={me} nodes={nodes} onDone={handleDone} />
        ))}
        {inbox.data?.length === 0 && (
          <Paper sx={{ p: 2 }}>
            <Typography variant="body2" color="text.secondary">
              No actionable items in your inbox.
            </Typography>
          </Paper>
        )}
      </Stack>
    </Stack>
  );
}
