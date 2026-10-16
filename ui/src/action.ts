// useAction runs one write against the API at a time and keeps its
// refusal to show. The UI never decides a write will be refused: it
// sends it, and shows what the API answered (a 403, 404 or 409 in the
// API's own words) next to the button that sent it.
import { useCallback, useState } from 'react';
import { ApiError } from './api';

export interface Action<A extends unknown[]> {
  run: (...args: A) => Promise<boolean>;
  busy: boolean;
  error?: string;
  clear: () => void;
}

// refusal is how an error from the API reads to a person: its status
// and the API's message, with nothing added that the API did not say.
export function refusal(err: unknown): string {
  if (err instanceof ApiError) {
    const what =
      err.status === 401 ? 'Not signed in' : err.status === 403 ? 'Not allowed' : err.status === 404 ? 'Not found' : err.status === 409 ? 'Conflict' : 'Refused';
    return `${what} (${err.status}): ${err.message}`;
  }
  return err instanceof Error ? err.message : String(err);
}

export function useAction<A extends unknown[]>(f: (...args: A) => Promise<unknown>, onDone?: () => void): Action<A> {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const run = useCallback(
    async (...args: A) => {
      setBusy(true);
      setError(undefined);
      try {
        await f(...args);
        onDone?.();
        return true;
      } catch (err) {
        setError(refusal(err));
        return false;
      } finally {
        setBusy(false);
      }
    },
    [f, onDone],
  );
  return { run, busy, error, clear: useCallback(() => setError(undefined), []) };
}
