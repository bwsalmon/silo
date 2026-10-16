// A task's lifecycle state (hub/tasks.State) as a chip.
import { Chip, type ChipProps } from '@mui/material';
import type { TaskState } from '../api';

const colors: Record<TaskState, ChipProps['color']> = {
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
      variant={state === 'cancelled' ? 'outlined' : 'filled'}
    />
  );
}

const nodeColors: Record<string, ChipProps['color']> = { online: 'success', stale: 'warning', offline: 'default' };

export function NodeStatusChip({ status }: { status: string }) {
  return <Chip size="small" label={status} color={nodeColors[status] ?? 'default'} />;
}
