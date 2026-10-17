// A task's live events (GET /v1/tasks/{id}/events, docs/hub.md §7.1)
// as Server-Sent Events: the hub replays what it holds after the
// cursor, then follows, and ends with an "end" event once the task's
// events are over. A subscriber that falls behind is dropped without
// one and reconnects from its cursor, the last seq it saw. Seqs have
// gaps, so the cursor is a seq the client saw, never one it counted.
//
// Two transports carry the same stream:
//
//   - 'eventsource', the browser's EventSource, which sends only the
//     browser's own credentials (a cookie) and reconnects by itself,
//     naming its cursor in Last-Event-ID.
//   - 'fetch', a streamed fetch read here, which carries the client's
//     token and headers, and so works in Node and behind IAP with a
//     bearer token. It reconnects from its cursor with ?from=.
import { check } from './errors.js';
import { EVENT_TYPES, type HubEvent } from './models.js';

export type StreamState = 'connecting' | 'live' | 'reconnecting' | 'ended' | 'closed';

export type Transport = 'eventsource' | 'fetch';

export interface FollowOptions {
  // from resumes after this seq: only events after it are delivered.
  // 0, the start, by default.
  from?: number;
  onEvent: (e: HubEvent) => void;
  onState?: (s: StreamState) => void;
  // onError hears why a stream closed for good (an ApiError for a
  // refusal such as a 404), or a failure it will reconnect after.
  onError?: (err: unknown) => void;
  // signal closes the stream when it aborts, as close() does.
  signal?: AbortSignal;
  // transport overrides the client's choice (see HubClient).
  transport?: Transport;
}

// EventStream is one subscription. cursor is the last seq delivered;
// done settles once the stream has ended or closed.
export interface EventStream {
  readonly cursor: number;
  readonly state: StreamState;
  readonly done: Promise<void>;
  close(): void;
}

// addEvent puts e into events, ordered by seq, once: a reconnect may
// deliver a seq again.
export function addEvent(events: HubEvent[], e: HubEvent): HubEvent[] {
  if (events.length === 0 || events[events.length - 1].seq < e.seq) return [...events, e];
  if (events.some((x) => x.seq === e.seq)) return events;
  return [...events, e].sort((a, b) => a.seq - b.seq);
}

// SSEMessage is one Server-Sent Events message.
export interface SSEMessage {
  id?: string;
  event: string;
  data: string;
}

// SSEParser takes a stream's text in whatever pieces it arrives and
// answers the messages each piece completes (the WHATWG event-stream
// format: id, event and data fields, comments ignored).
export class SSEParser {
  private buf = '';
  private id: string | undefined;
  private event = '';
  private data: string[] = [];

  push(chunk: string): SSEMessage[] {
    this.buf += chunk;
    const out: SSEMessage[] = [];
    for (;;) {
      const nl = this.buf.search(/\r\n|\r|\n/);
      if (nl < 0) break;
      // A lone \r at the end of the buffer may be the first half of \r\n.
      if (this.buf[nl] === '\r' && nl === this.buf.length - 1) break;
      const line = this.buf.slice(0, nl);
      this.buf = this.buf.slice(nl + (this.buf.startsWith('\r\n', nl) ? 2 : 1));
      if (line === '') {
        if (this.data.length > 0) out.push({ id: this.id, event: this.event || 'message', data: this.data.join('\n') });
        this.event = '';
        this.data = [];
        continue;
      }
      if (line.startsWith(':')) continue;
      const colon = line.indexOf(':');
      const field = colon < 0 ? line : line.slice(0, colon);
      let value = colon < 0 ? '' : line.slice(colon + 1);
      if (value.startsWith(' ')) value = value.slice(1);
      if (field === 'id') this.id = value;
      else if (field === 'event') this.event = value;
      else if (field === 'data') this.data.push(value);
    }
    return out;
  }
}

// Connection is what a transport needs of the client.
export interface Connection {
  // url is the stream's absolute or same-origin URL, with from as its
  // cursor when it is given.
  url(task: string, from?: number): string;
  fetch(url: string, init: RequestInit): Promise<Response>;
  headers(): Promise<Record<string, string>>;
  withCredentials: boolean;
}

// retryMs is how long the fetch transport waits before it reconnects.
export const retryMs = 1000;

class Subscription implements EventStream {
  cursor: number;
  state: StreamState = 'connecting';
  readonly done: Promise<void>;
  private settle!: () => void;
  protected readonly abort = new AbortController();

  constructor(protected readonly o: FollowOptions) {
    this.cursor = o.from ?? 0;
    this.done = new Promise((r) => (this.settle = r));
    o.signal?.addEventListener('abort', () => this.close(), { once: true });
  }

  protected setState(s: StreamState) {
    if (this.state === 'ended' || this.state === 'closed') return;
    this.state = s;
    this.o.onState?.(s);
    if (s === 'ended' || s === 'closed') {
      this.abort.abort();
      this.settle();
    }
  }

  // deliver passes on an event's data once, after the cursor.
  protected deliver(data: string) {
    let e: HubEvent;
    try {
      e = JSON.parse(data) as HubEvent;
    } catch {
      return; // Not an event: nothing the hub sends.
    }
    if (typeof e.seq === 'number') {
      if (e.seq <= this.cursor) return;
      this.cursor = e.seq;
    }
    this.o.onEvent(e);
  }

  close() {
    this.setState('closed');
  }
}

// followEventSource follows with the browser's EventSource. A stream
// opened with a cursor names it in its URL, which wins over
// Last-Event-ID, so its reconnects replay from that cursor: the seqs
// already delivered are dropped here.
export function followEventSource(c: Connection, task: string, o: FollowOptions): EventStream {
  const s = new (class extends Subscription {
    es: EventSource;
    constructor() {
      super(o);
      this.es = new EventSource(c.url(task, o.from || undefined), c.withCredentials ? { withCredentials: true } : undefined);
      const onEvent = (m: MessageEvent<string>) => this.deliver(m.data);
      for (const t of EVENT_TYPES) this.es.addEventListener(t, onEvent);
      this.es.addEventListener('end', () => this.setState('ended'));
      this.es.onopen = () => this.setState('live');
      this.es.onerror = () => this.setState(this.es.readyState === EventSource.CLOSED ? 'closed' : 'reconnecting');
    }
    protected override setState(st: StreamState) {
      super.setState(st);
      if (st === 'ended' || st === 'closed') this.es.close();
    }
  })();
  if (o.signal?.aborted) s.close();
  return s;
}

// followFetch follows with a streamed fetch, reconnecting from its
// cursor until the stream ends, is closed, or is refused.
export function followFetch(c: Connection, task: string, o: FollowOptions): EventStream {
  const s = new (class extends Subscription {
    async run() {
      let first = true;
      while (this.state !== 'ended' && this.state !== 'closed') {
        if (!first) {
          this.setState('reconnecting');
          await sleep(retryMs, this.abort.signal);
          if (this.abort.signal.aborted) return;
        }
        first = false;
        try {
          const r = await c.fetch(c.url(task, this.cursor || undefined), {
            credentials: c.withCredentials ? 'include' : 'same-origin',
            cache: 'no-store',
            headers: { ...(await c.headers()), Accept: 'text/event-stream' },
            signal: this.abort.signal,
          });
          // A refusal (a 4xx: the task is not the caller's to see, a bad
          // cursor) is the same on every retry, and closes the stream
          // below; anything else is retried.
          await check(r);
          if (!r.body) throw new Error('silo: the event stream has no body');
          this.setState('live');
          await this.read(r.body);
        } catch (err) {
          if (this.abort.signal.aborted) return;
          o.onError?.(err);
          if (isRefusal(err)) {
            this.setState('closed');
            return;
          }
        }
      }
    }

    async read(body: ReadableStream<Uint8Array>) {
      const p = new SSEParser();
      const dec = new TextDecoder();
      const reader = body.getReader();
      try {
        for (;;) {
          const { done, value } = await reader.read();
          if (done) return;
          for (const m of p.push(dec.decode(value, { stream: true }))) {
            if (m.event === 'end') {
              this.setState('ended');
              return;
            }
            this.deliver(m.data);
          }
        }
      } finally {
        reader.cancel().catch(() => {});
      }
    }
  })(o);
  if (o.signal?.aborted) s.close();
  else void s.run();
  return s;
}

function isRefusal(err: unknown): boolean {
  const status = (err as { status?: unknown })?.status;
  return typeof status === 'number' && status >= 400 && status < 500;
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const t = setTimeout(resolve, ms);
    signal.addEventListener(
      'abort',
      () => {
        clearTimeout(t);
        resolve();
      },
      { once: true },
    );
  });
}
