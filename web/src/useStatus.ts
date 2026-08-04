import { useEffect, useState } from "react";
import { fetchStatus, type Status } from "./api";

/** How often the card re-reads the process state. */
const POLL_MS = 10_000;

export interface StatusState {
  /** The last successful reading, or null before the first one lands. */
  status: Status | null;
  /** Why the last attempt failed, or null while contact holds. */
  error: string | null;
  /** True until the first attempt settles, either way. */
  loading: boolean;
}

/**
 * Polls /api/v1/status.
 *
 * A failed poll keeps the last reading rather than blanking the card — the
 * values were true when they were read, and the card marks them stale rather
 * than pretending it knows nothing.
 */
export function useStatus(): StatusState {
  const [state, setState] = useState<StatusState>({
    status: null,
    error: null,
    loading: true,
  });

  useEffect(() => {
    const controller = new AbortController();
    let active = true;

    async function read() {
      try {
        const status = await fetchStatus(controller.signal);
        if (active) setState({ status, error: null, loading: false });
      } catch (err) {
        if (!active || controller.signal.aborted) return;
        setState((prev) => ({
          status: prev.status,
          error: messageFor(err),
          loading: false,
        }));
      }
    }

    void read();
    const timer = setInterval(() => void read(), POLL_MS);

    return () => {
      active = false;
      controller.abort();
      clearInterval(timer);
    };
  }, []);

  return state;
}

function messageFor(err: unknown): string {
  if (err instanceof TypeError) {
    // fetch rejects with a TypeError when it never reached the server.
    return "No contact with the server. The process may be stopped.";
  }
  return err instanceof Error ? err.message : "The status could not be read.";
}
