// useLoad runs a read against the API, again whenever its deps change
// and every refreshMs if given, and keeps the last answer while the
// next is on its way.
import { useCallback, useEffect, useRef, useState } from 'react';

export interface Loaded<T> {
  data?: T;
  error?: Error;
  loading: boolean;
  reload: () => void;
}

export function useLoad<T>(load: (signal: AbortSignal) => Promise<T>, deps: unknown[], refreshMs?: number): Loaded<T> {
  const [data, setData] = useState<T>();
  const [error, setError] = useState<Error>();
  const [loading, setLoading] = useState(true);
  const [tick, setTick] = useState(0);
  const loadRef = useRef(load);
  loadRef.current = load;
  const reload = useCallback(() => setTick((t) => t + 1), []);

  useEffect(() => {
    const ctl = new AbortController();
    setLoading(true);
    loadRef.current(ctl.signal).then(
      (d) => {
        setData(d);
        setError(undefined);
        setLoading(false);
      },
      (err: unknown) => {
        if (ctl.signal.aborted) return;
        setError(err instanceof Error ? err : new Error(String(err)));
        setLoading(false);
      },
    );
    return () => ctl.abort();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, tick]);

  useEffect(() => {
    if (!refreshMs) return;
    const t = setInterval(reload, refreshMs);
    return () => clearInterval(t);
  }, [refreshMs, reload]);

  return { data, error, loading, reload };
}
