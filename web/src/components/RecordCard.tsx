import type { Status } from "../api/types";
import { clock, commit, orDash, uptime } from "../format";
import { FieldRow } from "./FieldRow";
import { MigrationLog } from "./MigrationLog";
import { Stamp, type StampState } from "./Stamp";

interface Props {
  status: Status | null;
  /** Set when the last reading failed; the card then shows what to do. */
  error: string | null;
  loading: boolean;
}

const DASH = "—";

/** The record of service: what this process is and whether it is working. */
export function RecordCard({ status, error, loading }: Props) {
  const stale = Boolean(error && status);
  const state = stampState(status, error);

  return (
    <main className="card" data-stale={stale}>
      <header className="card__head">
        <div>
          <h1 className="card__mark stamped">rental-bot</h1>
          <p className="card__origin mono">{window.location.host}</p>
        </div>
        <div className="card__file">
          <p className="card__eyebrow stamped">Record of service</p>
          <p className="card__read mono">{status ? `read ${clock(status.checked_at)}` : DASH}</p>
        </div>
      </header>

      {error && <p className="card__notice">{error}</p>}

      <dl className="card__fields">
        <FieldRow label="Version">{orDash(status?.version)}</FieldRow>
        <FieldRow label="Commit">{status ? commit(status.commit) : DASH}</FieldRow>
        <FieldRow label="Built">{orDash(status?.build_date)}</FieldRow>
        <FieldRow label="Runtime">{orDash(status?.go_version)}</FieldRow>
        <FieldRow label="Uptime">{status ? uptime(status.uptime_seconds) : DASH}</FieldRow>

        {/* The readiness checks carry their own labels and detail; a failing
            one says what to do about it. */}
        {status?.checks.map((check) => (
          <FieldRow key={check.name} label={check.name} tone={check.ok ? undefined : "fault"}>
            {check.detail ?? (check.ok ? "ok" : "not ready")}
          </FieldRow>
        ))}
      </dl>

      {/* The ledger and the stamp share the foot as flex siblings, so the
          stamp can never land on top of an entry however narrow the card. */}
      <footer className="card__foot">
        <MigrationLog migrations={status?.migrations ?? []} />
        {!loading && <Stamp state={state} at={status?.checked_at ?? new Date().toISOString()} />}
      </footer>
    </main>
  );
}

function stampState(status: Status | null, error: string | null): StampState {
  if (error || !status) return "no-contact";
  return status.status === "operational" ? "operational" : "degraded";
}
