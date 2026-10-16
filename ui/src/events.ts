// A task's live events (GET /v1/tasks/{id}/events, docs/hub.md §7) over
// Server-Sent Events. The hub replays what it holds after the cursor,
// then follows, and ends with an "end" event once the task's events are
// over. A dropped stream reconnects by itself, from the last seq the
// browser saw (EventSource's Last-Event-ID), so the first request names
// no cursor: a ?from= in the URL would win over Last-Event-ID and
// replay from the start on every reconnect.
import { useEffect, useState } from 'react';
import type { HubEvent } from './api';

// The event types the hub relays: package events' types, and "artifact"
// for the reference it keeps of a node-only event. An EventSource
// delivers a named event only to a listener for its name.
export const EVENT_TYPES = ['status', 'transcript', 'tool_call', 'usage', 'approval_request', 'resumable', 'result', 'artifact'];

export function streamURL(task: string): string {
  return `/v1/tasks/${encodeURIComponent(task)}/events`;
}

// addEvent puts e into events, ordered by seq, once: a reconnect may
// deliver a seq again.
export function addEvent(events: HubEvent[], e: HubEvent): HubEvent[] {
  if (events.length === 0 || events[events.length - 1].seq < e.seq) return [...events, e];
  if (events.some((x) => x.seq === e.seq)) return events;
  return [...events, e].sort((a, b) => a.seq - b.seq);
}

export type StreamState = 'connecting' | 'live' | 'reconnecting' | 'ended' | 'closed';

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
    const es = new EventSource(streamURL(task));
    const onEvent = (m: MessageEvent<string>) => {
      try {
        const e = JSON.parse(m.data) as HubEvent;
        setEvents((prev) => addEvent(prev, e));
      } catch {
        // Not an event: nothing the hub sends.
      }
    };
    for (const t of EVENT_TYPES) es.addEventListener(t, onEvent);
    es.addEventListener('end', () => {
      setState('ended');
      es.close();
    });
    es.onopen = () => setState('live');
    es.onerror = () => setState(es.readyState === EventSource.CLOSED ? 'closed' : 'reconnecting');
    return () => es.close();
  }, [task]);
  return { events, state };
}
