// A task's live events (GET /v1/tasks/{id}/events, docs/hub.md §7), as
// the SDK follows them: the browser's EventSource, which reconnects by
// itself from the last seq it saw (Last-Event-ID), and delivers each
// seq once. The stream ends with the task's events.
import { useEffect, useState } from 'react';
import { addEvent, hub, type HubEvent, type StreamState } from './api';

export type { StreamState };

// awaiting is the question an interactive session is waiting on a
// human for ('' when it asked none), or undefined when it is not
// waiting: its last awaiting_reply event, unless that was its resume.
export function awaiting(events: HubEvent[]): string | undefined {
  for (let i = events.length - 1; i >= 0; i--) {
    const a = events[i].awaiting_reply;
    if (a) return a.state === 'waiting' ? (a.question ?? '') : undefined;
  }
  return undefined;
}

export interface Stream {
  events: HubEvent[];
  state: StreamState;
}

export function useEventStream(task: string): Stream {
  const [events, setEvents] = useState<HubEvent[]>([]);
  const [state, setState] = useState<StreamState>('connecting');
  useEffect(() => {
    setEvents([]);
    setState('connecting');
    const s = hub.follow(task, {
      onEvent: (e) => setEvents((prev) => addEvent(prev, e)),
      onState: setState,
    });
    return () => s.close();
  }, [task]);
  return { events, state };
}
