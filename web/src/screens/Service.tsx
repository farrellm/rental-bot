import { describeError, useStatus } from "../api";
import { RecordCard } from "../components/RecordCard";

/**
 * The record of service: what this process is and whether it is working.
 *
 * This was the whole application at M0. It is now one screen among several,
 * and it sits behind the session like everything else — /api/v1/status reports
 * build identity and schema state, which is nobody else's business.
 */
export function Service() {
  const status = useStatus();

  return (
    <main className="shell__main shell__main--single">
      <RecordCard
        status={status.data ?? null}
        error={status.isError ? describeError(status.error) : null}
        loading={status.isPending}
      />
    </main>
  );
}
