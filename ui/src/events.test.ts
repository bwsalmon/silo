import { describe, expect, it } from 'vitest';
import type { HubEvent } from './api';
import { addEvent, streamURL } from './events';
import { parseRoute, routePath } from './router';

const ev = (seq: number): HubEvent => ({ v: 1, seq, time: '', type: 'status', task: 't-1' });

describe('addEvent', () => {
  it('keeps events in seq order, once each', () => {
    let es: HubEvent[] = [];
    for (const s of [1, 3, 2, 3, 1, 5]) es = addEvent(es, ev(s));
    expect(es.map((e) => e.seq)).toEqual([1, 2, 3, 5]);
  });
});

describe('streamURL', () => {
  // A cursor in the URL would win over Last-Event-ID and replay the
  // whole stream at every reconnect.
  it('names no cursor', () => {
    expect(streamURL('t/1')).toBe('/v1/tasks/t%2F1/events');
  });
});

describe('routes', () => {
  it('round-trip', () => {
    for (const p of ['/', '/teams/infra', '/tasks/t-1', '/nodes']) expect(routePath(parseRoute(p))).toBe(p);
    expect(parseRoute('/nope/x')).toEqual({ page: 'notfound', path: '/nope/x' });
  });
});
