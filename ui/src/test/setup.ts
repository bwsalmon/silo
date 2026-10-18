import { afterEach, vi } from 'vitest';
import { cleanup } from '@testing-library/react';

// FakeEventSource stands in for the browser's EventSource, which jsdom
// does not have: a test finds the stream the UI opened in instances
// and sends it events.
export class FakeEventSource {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  static instances: FakeEventSource[] = [];

  readyState = FakeEventSource.CONNECTING;
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  private listeners = new Map<string, ((m: MessageEvent<string>) => void)[]>();

  constructor(readonly url: string) {
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, f: (m: MessageEvent<string>) => void) {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), f]);
  }

  open() {
    this.readyState = FakeEventSource.OPEN;
    this.onopen?.();
  }

  emit(type: string, data: unknown) {
    const m = new MessageEvent<string>(type, { data: JSON.stringify(data) });
    for (const f of this.listeners.get(type) ?? []) f(m);
  }

  close() {
    this.readyState = FakeEventSource.CLOSED;
  }
}

vi.stubGlobal('EventSource', FakeEventSource);

afterEach(() => {
  cleanup();
  FakeEventSource.instances = [];
  localStorage.clear();
  vi.restoreAllMocks();
  window.history.replaceState(null, '', '/');
});
