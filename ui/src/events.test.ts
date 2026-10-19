import { describe, expect, it } from 'vitest';
import { addEvent, type HubEvent } from './api';
import { parseRoute, routePath } from './router';

const ev = (seq: number): HubEvent => ({ v: 1, seq, time: '', type: 'status', task: 't-1' });

describe('addEvent', () => {
  it('keeps events in seq order, once each', () => {
    let es: HubEvent[] = [];
    for (const s of [1, 3, 2, 3, 1, 5]) es = addEvent(es, ev(s));
    expect(es.map((e) => e.seq)).toEqual([1, 2, 3, 5]);
  });
});

describe('routes', () => {
  it('round-trip', () => {
    for (const p of ['/', '/inbox', '/backlog', '/grainfiles', '/deployments', '/deployments/payments-prod', '/teams/infra', '/tasks/t-1', '/nodes'])
      expect(routePath(parseRoute(p))).toBe(p);
    expect(parseRoute('/nope/x')).toEqual({ page: 'notfound', path: '/nope/x' });
  });
});
