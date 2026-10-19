// A task's lifecycle state (hub/tasks.State) as a chip. The hub's own,
// not a copy: grain draws its task states with style.css's .badge and
// .state-pill classes over its own states (proposed, awaiting_reply,
// completed, ...), not with a component, and its StateDot is only the
// brand mark's animation for a running task. The colours are the
// copied theme's palette (theme.ts).
import { Chip, type ChipProps } from '@mui/material';
import type { GrainfileKind, TaskState } from '../api';

const colors: Record<TaskState, ChipProps['color']> = {
  proposed: 'secondary',
  rejected: 'default',
  queued: 'default',
  offered: 'info',
  running: 'primary',
  done: 'success',
  failed: 'error',
  cancelled: 'default',
  lost: 'warning',
};

export function StateChip({ state, cancelPending }: { state: TaskState; cancelPending?: boolean }) {
  return (
    <Chip
      size="small"
      label={cancelPending ? `${state}, cancelling` : state}
      color={colors[state] ?? 'default'}
      variant={state === 'cancelled' || state === 'rejected' ? 'outlined' : 'filled'}
    />
  );
}

const nodeColors: Record<string, ChipProps['color']> = { online: 'success', stale: 'warning', offline: 'default' };

export function NodeStatusChip({ status }: { status: string }) {
  return <Chip size="small" label={status} color={nodeColors[status] ?? 'default'} />;
}

const grainfileKindColors: Record<GrainfileKind, ChipProps['color']> = {
  task: 'info',
  process: 'primary',
  capability: 'secondary',
};

export function GrainfileKindChip({ kind }: { kind: GrainfileKind }) {
  return <Chip size="small" label={kind} color={grainfileKindColors[kind] ?? 'default'} />;
}
