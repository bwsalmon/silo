import { afterEach, describe, expect, it, vi } from 'vitest';
import { addEvent, HubClient, NotFoundError, SSEParser, type HubEvent, type StreamState } from '../src/index.js';

const ev = (seq: number): HubEvent => ({ v: 1, seq, time: '', type: 'status', task: 't-1' });
const sse = (seq: number) => `id: ${seq}\nevent: status\ndata: ${JSON.stringify(ev(seq))}\n\n`;
const end = 'event: end\ndata: {"type":"end"}\n\n';

// body is a response body that sends chunks, then closes.
function body(...chunks: string[]): ReadableStream<Uint8Array> {
  const enc = new TextEncoder();
  return new ReadableStream({
    start(c) {
      for (const s of chunks) c.enqueue(enc.encode(s));
      c.close();
    },
  });
}

describe('addEvent', () => {
  it('keeps events in seq order, once each', () => {
    let es: HubEvent[] = [];
    for (const s of [1, 3, 2, 3, 1, 5]) es = addEvent(es, ev(s));
    expect(es.map((e) => e.seq)).toEqual([1, 2, 3, 5]);
  });
});

describe('SSEParser', () => {
  it('reads messages split anywhere, and skips comments', () => {
    const p = new SSEParser();
    const text = `: keepalive\n\n${sse(1)}id: 2\r\nevent: x\r\ndata: a\r\ndata: b\r\n\r\n`;
    const out = [];
    for (const ch of text) out.push(...p.push(ch));
    expect(out).toEqual([
      { id: '1', event: 'status', data: JSON.stringify(ev(1)) },
      { id: '2', event: 'x', data: 'a\nb' },
    ]);
  });
});

describe('follow over fetch', () => {
  afterEach(() => vi.useRealTimers());

  it('resumes from its cursor after a drop, delivers each seq once, and ends', async () => {
    vi.useFakeTimers();
    const urls: string[] = [];
    const replies = [body(sse(4), sse(5)), body(sse(5), sse(7), end)];
    const f = vi.fn(async (url: string) => {
      urls.push(url);
      return new Response(replies.shift(), { status: 200, headers: { 'Content-Type': 'text/event-stream' } });
    });
    const got: number[] = [];
    const states: StreamState[] = [];
    const s = new HubClient({ fetch: f, token: 't' }).follow('t 1', { from: 3, onEvent: (e) => got.push(e.seq), onState: (st) => states.push(st) });
    await vi.runAllTimersAsync();
    await s.done;
    expect(urls).toEqual(['/v1/tasks/t%201/events?from=3', '/v1/tasks/t%201/events?from=5']);
    expect(got).toEqual([4, 5, 7]);
    expect(s.cursor).toBe(7);
    expect(states).toEqual(['live', 'reconnecting', 'live', 'ended']);
  });

  it('closes on a refusal, with the API’s error', async () => {
    const errors: unknown[] = [];
    const s = new HubClient({ fetch: async () => new Response('no such task', { status: 404 }), token: 't' }).follow('t-1', {
      onEvent: () => {},
      onError: (e) => errors.push(e),
    });
    await s.done;
    expect(s.state).toBe('closed');
    expect(errors[0]).toBeInstanceOf(NotFoundError);
  });

  it('closes when its signal aborts', async () => {
    const ac = new AbortController();
    const f = vi.fn(
      async (_url: string, init?: RequestInit) =>
        new Response(
          new ReadableStream({
            start(c) {
              init?.signal?.addEventListener('abort', () => c.error(new DOMException('aborted', 'AbortError')));
            },
          }),
        ),
    );
    const s = new HubClient({ fetch: f }).follow('t-1', { onEvent: () => {}, signal: ac.signal });
    await vi.waitFor(() => expect(s.state).toBe('live'));
    ac.abort();
    await s.done;
    expect(s.state).toBe('closed');
    expect(f).toHaveBeenCalledTimes(1);
  });
});

describe('events', () => {
  it('iterates until the stream ends', async () => {
    const h = new HubClient({ fetch: async () => new Response(body(sse(1), sse(2), end)), token: 't' });
    const got: number[] = [];
    for await (const e of h.events('t-1')) got.push(e.seq);
    expect(got).toEqual([1, 2]);
  });

  it('throws a refusal', async () => {
    const h = new HubClient({ fetch: async () => new Response('forbidden', { status: 403 }), token: 't' });
    await expect(
      (async () => {
        for await (const _ of h.events('t-1')) void _;
      })(),
    ).rejects.toMatchObject({ status: 403 });
  });
});

// FakeEventSource stands in for the browser's.
class FakeEventSource {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  static last: FakeEventSource;
  readyState = 0;
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  listeners = new Map<string, ((m: { data: string }) => void)[]>();
  constructor(readonly url: string) {
    FakeEventSource.last = this;
  }
  addEventListener(t: string, f: (m: { data: string }) => void) {
    this.listeners.set(t, [...(this.listeners.get(t) ?? []), f]);
  }
  emit(t: string, data: unknown) {
    for (const f of this.listeners.get(t) ?? []) f({ data: JSON.stringify(data) });
  }
  close() {
    this.readyState = 2;
  }
}

describe('follow over EventSource', () => {
  afterEach(() => vi.unstubAllGlobals());

  it('is the browser’s default, names no cursor of its own, and drops what it has seen', async () => {
    vi.stubGlobal('EventSource', FakeEventSource);
    const got: number[] = [];
    const s = new HubClient().follow('t/1', { onEvent: (e) => got.push(e.seq) });
    const es = FakeEventSource.last;
    // A cursor in the URL would win over Last-Event-ID at every reconnect.
    expect(es.url).toBe('/v1/tasks/t%2F1/events');
    es.onopen?.();
    expect(s.state).toBe('live');
    for (const n of [1, 2, 2, 3]) es.emit('status', ev(n));
    es.emit('end', { type: 'end' });
    await s.done;
    expect(got).toEqual([1, 2, 3]);
    expect(s.state).toBe('ended');
    expect(es.readyState).toBe(FakeEventSource.CLOSED);
  });

  it('resumes from a cursor it is given', () => {
    vi.stubGlobal('EventSource', FakeEventSource);
    new HubClient().follow('t-1', { from: 9, onEvent: () => {} });
    expect(FakeEventSource.last.url).toBe('/v1/tasks/t-1/events?from=9');
  });

  it('is not used with a token, which EventSource cannot send', async () => {
    vi.stubGlobal('EventSource', FakeEventSource);
    FakeEventSource.last = undefined as unknown as FakeEventSource;
    const f = vi.fn(async () => new Response(body(end)));
    await new HubClient({ token: 't', fetch: f }).follow('t-1', { onEvent: () => {} }).done;
    expect(FakeEventSource.last).toBeUndefined();
    expect(f).toHaveBeenCalledOnce();
  });
});
