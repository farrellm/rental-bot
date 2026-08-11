import { useState } from "react";
import { useSearchParams } from "react-router";

import {
  describeError,
  useConnectGmail,
  useDisconnectGmail,
  useEmailMessages,
  useIntakeStanding,
  useSyncNow,
  type EmailMessage,
  type EmailStatus,
  type IntakeStanding,
} from "../api";
import { DayRegister } from "../components/DayRegister";
import { FieldRow } from "../components/FieldRow";
import { Stamp, type StampState } from "../components/Stamp";
import { ago, bytes, clock, DASH, hourMinute, plural, timestamp } from "../format";
import { Dispatch } from "./Dispatch";

/** The mailbox's state and the word stamped on the card are one vocabulary. */
const STATE_STAMP: Record<IntakeStanding["state"], StampState> = {
  watching: "watching",
  lapsed: "lapsed",
  degraded: "degraded",
  revoked: "revoked",
  "not-connected": "not-connected",
  "not-configured": "not-configured",
};

const MESSAGE_STAMP: Record<EmailStatus, StampState> = {
  received: "received",
  parsing: "parsing",
  needs_review: "needs-review",
  applied: "applied",
  rejected: "rejected",
  ignored: "ignored",
  failed: "failed",
};

/**
 * Intake: what comes in, and what goes out.
 *
 * Two records on one desk. The mail room is what arrives — forwarded receipts,
 * leases, statements — and the dispatch book below it is what this process
 * sends back out when something needs attention. They are the same shape of
 * thing kept in opposite directions, which is why they are one screen rather
 * than two.
 */
export function Intake() {
  return (
    <main className="shell__main shell__main--single">
      <div className="desk">
        <Mailbox />
        <Dispatch />
      </div>
    </main>
  );
}

/**
 * The mail room.
 *
 * One question comes first and everything is arranged around it: is mail still
 * arriving? The head answers it — the account, the watch, the cursor, when the
 * last delivery landed — and the register below is the evidence, kept by day
 * the way an inbound mail log is kept.
 */
function Mailbox() {
  const standing = useIntakeStanding();
  const messages = useEmailMessages();
  const [params, setParams] = useSearchParams();

  const data = standing.data ?? null;
  const error = standing.isError ? describeError(standing.error) : null;
  const outcome = params.get("gmail");

  return (
    <section className="card" data-stale={Boolean(error && data)}>
      <header className="card__head">
        <div className="card__title">
          <h1 className="card__mark stamped">Intake</h1>
          <p className="card__origin mono">{data?.address ?? "no mailbox connected"}</p>
        </div>
        <div className="card__file">
          <p className="card__eyebrow stamped">Record of intake</p>
          <p className="card__read mono">{data ? `read ${clock(data.checked_at)}` : DASH}</p>
        </div>
      </header>

      {error && <p className="card__notice">{error}</p>}
      {outcome && <Outcome outcome={outcome} onDismiss={() => setParams({}, { replace: true })} />}
      {data?.last_error && !error && <p className="card__notice">{data.last_error}</p>}

      {data && <Standing standing={data} />}

      {/* Kept by day, because an inbound mail log is. */}
      <DayRegister
        eyebrow="Received"
        entries={messages.data?.items ?? []}
        keyOf={(message) => message.id}
        at={(message) => message.received_at}
        render={(message) => <Entry message={message} />}
        loading={messages.isPending}
        error={messages.isError ? describeError(messages.error) : null}
        empty={emptyRegister(data)}
      />

      <footer className="card__foot">
        <div className="register__foot">
          <Tally counts={data?.counts ?? {}} />
          <Actions standing={data} />
        </div>
        {!standing.isPending && data && (
          <Stamp state={STATE_STAMP[data.state]} at={data.checked_at} />
        )}
      </footer>
    </section>
  );
}

/** What came back from the round trip through Google. */
function Outcome({ outcome, onDismiss }: { outcome: string; onDismiss: () => void }) {
  const said: Record<string, string> = {
    connected: "Gmail is connected. The first sync is running now.",
    denied: "Google did not grant access. Nothing was changed.",
    failed: "The connection could not be completed. Try again.",
  };
  const text = said[outcome];
  if (!text) return null;

  return (
    <p className="card__notice card__notice--quiet">
      {text}{" "}
      <button type="button" className="button button--quiet intake__dismiss" onClick={onDismiss}>
        Dismiss
      </button>
    </p>
  );
}

/** The head entries: everything that answers "is mail still arriving". */
function Standing({ standing }: { standing: IntakeStanding }) {
  if (!standing.configured) {
    return (
      <div className="card__fields">
        <p className="register__empty">
          Email ingestion is not set up, so nothing is being collected. Fill these in and restart:
        </p>
        <ul className="register__missing mono">
          {(standing.missing ?? []).map((key) => (
            <li key={key}>{key}</li>
          ))}
        </ul>
      </div>
    );
  }

  if (!standing.connected) {
    return (
      <div className="card__fields">
        <p className="register__empty">
          No mailbox is connected. Connect one and forwarded mail files itself.
        </p>
      </div>
    );
  }

  return (
    <dl className="card__fields">
      <FieldRow label="Forward to">{standing.forward_to ?? DASH}</FieldRow>
      <FieldRow label="Watch" tone={standing.state === "lapsed" ? "fault" : undefined}>
        {watchReading(standing)}
      </FieldRow>
      <FieldRow label="Cursor">{standing.history_id || "not set"}</FieldRow>
      <FieldRow label="Last delivery">{deliveryReading(standing)}</FieldRow>
      <FieldRow label="Senders">
        {standing.allowed_senders.length > 0 ? standing.allowed_senders.join(", ") : DASH}
      </FieldRow>
    </dl>
  );
}

/**
 * A lapsed watch is not an outage, and the reading says so: mail still arrives
 * on the poll, just later. Saying only "lapsed" sends the operator looking for
 * a fault that is not there.
 */
function watchReading(standing: IntakeStanding): string {
  const minutes = Math.round(standing.poll_interval_seconds / 60);
  if (!standing.watch_expires_at) {
    return `none — mail arrives on the ${minutes}-minute poll`;
  }
  if (standing.state === "lapsed") {
    return `expired ${ago(standing.watch_expires_at)} — mail arrives on the ${minutes}-minute poll`;
  }
  return `renews before ${timestamp(standing.watch_expires_at)}`;
}

function deliveryReading(standing: IntakeStanding): string {
  if (!standing.last_sync_at) return "no sync has run yet";
  const count = standing.last_sync_count;
  return `${ago(standing.last_sync_at)} · ${count === 0 ? "nothing new" : plural(count, "message", "messages")}`;
}

/**
 * What the register holds, by disposition.
 *
 * The foot of a card in this application carries a running total beside the
 * stamp — the service record puts its migration ledger there. This is the same
 * idea: the register above is what arrived, and this is how much of it there
 * is without counting the lines.
 */
function Tally({ counts }: { counts: Record<string, number> }) {
  const order: [string, string][] = [
    ["received", "received"],
    ["ignored", "ignored"],
    ["failed", "failed"],
    ["needs_review", "need review"],
    ["applied", "applied"],
  ];
  const held = order.filter(([key]) => (counts[key] ?? 0) > 0);
  if (held.length === 0) return null;

  return (
    <p className="register__tally">
      {held.map(([key, noun], i) => (
        <span key={key} className="intake__count">
          {i > 0 && <span className="register__separator"> · </span>}
          <span className="mono">{counts[key]}</span> {noun}
        </span>
      ))}
    </p>
  );
}

/** Connect, Sync now, Disconnect. Verbs stay plain. */
function Actions({ standing }: { standing: IntakeStanding | null }) {
  const connect = useConnectGmail();
  const disconnect = useDisconnectGmail();
  const sync = useSyncNow();
  const [confirming, setConfirming] = useState(false);

  if (!standing?.configured) {
    return <div className="actions" />;
  }

  const failure = connect.error ?? disconnect.error ?? sync.error;

  return (
    <div className="register__actions">
      {failure && <p className="register__failure">{describeError(failure)}</p>}
      <div className="actions">
        {!standing.connected && (
          <button
            type="button"
            className="button button--primary"
            disabled={connect.isPending}
            onClick={() => {
              connect.mutate(undefined, {
                // A full navigation, not a fetch: the operator has to see
                // Google's own consent screen on Google's own origin.
                onSuccess: ({ authorize_url }) => {
                  window.location.assign(authorize_url);
                },
              });
            }}
          >
            {connect.isPending ? "Opening Google…" : "Connect Gmail"}
          </button>
        )}

        {standing.connected && (
          <>
            <button
              type="button"
              className="button button--primary"
              disabled={sync.isPending}
              onClick={() => sync.mutate()}
            >
              {sync.isPending ? "Syncing…" : "Sync now"}
            </button>

            {confirming ? (
              <>
                <button
                  type="button"
                  className="button button--danger"
                  disabled={disconnect.isPending}
                  onClick={() =>
                    disconnect.mutate(undefined, { onSuccess: () => setConfirming(false) })
                  }
                >
                  Disconnect for good
                </button>
                <button type="button" className="button" onClick={() => setConfirming(false)}>
                  Keep
                </button>
              </>
            ) : (
              <button
                type="button"
                className="button button--danger"
                onClick={() => setConfirming(true)}
              >
                Disconnect
              </button>
            )}
          </>
        )}
      </div>
    </div>
  );
}

/** An empty screen is an invitation to act, and names the next move. */
function emptyRegister(standing: IntakeStanding | null): string {
  if (!standing?.configured) {
    return "Nothing can arrive until ingestion is set up.";
  }
  if (!standing.connected) {
    return "Nothing has arrived. Connect a mailbox and forwarded mail lands here.";
  }
  return `Nothing has arrived yet. Forward a receipt to ${standing.forward_to ?? "the connected address"} and it lands here.`;
}

/**
 * One line of the register, and the enclosures behind it.
 *
 * The line opens rather than navigating: the whole record is four facts, and a
 * screen of its own for four facts costs a page load to read what fits under
 * the line it was on.
 */
function Entry({ message }: { message: EmailMessage }) {
  const [open, setOpen] = useState(false);
  const filed = message.attachments.length;

  return (
    <div className="entry-line">
      <button
        type="button"
        className="entry-line__line"
        aria-expanded={open}
        onClick={() => setOpen((was) => !was)}
      >
        <span className="entry-line__time mono">{hourMinute(message.received_at)}</span>
        <span className="entry-line__who">
          <span className="entry-line__from">{message.from_addr || "unknown sender"}</span>
          <span className="entry-line__subject">{message.subject || "no subject"}</span>
        </span>
        {filed > 0 && (
          <span className="entry-line__encl mono" title={`${filed} attached`}>
            {filed} encl.
          </span>
        )}
        <Stamp state={MESSAGE_STAMP[message.status]} small />
      </button>

      {open && (
        <div className="entry-line__body">
          {message.error && <p className="entry-line__fault">{message.error}</p>}
          {message.snippet && <p className="entry-line__snippet">{message.snippet}</p>}

          {filed > 0 && (
            <ul className="enclosures">
              {message.attachments.map((att) => (
                <li key={att.id} className="enclosure">
                  <span className="enclosure__name">{att.filename || "unnamed"}</span>
                  <span className="enclosure__size mono">{bytes(att.size_bytes)}</span>
                  {att.document_id !== null ? (
                    <a
                      className="button button--quiet enclosure__open"
                      href={`/api/v1/documents/${att.document_id}/content`}
                      target="_blank"
                      rel="noreferrer"
                    >
                      Open
                    </a>
                  ) : (
                    <span className="enclosure__skipped">{att.skipped_reason || "not stored"}</span>
                  )}
                </li>
              ))}
            </ul>
          )}

          <dl className="entry-line__facts">
            <FieldRow label="Message">{message.gmail_message_id}</FieldRow>
            <FieldRow label="Arrived">{timestamp(message.received_at)}</FieldRow>
          </dl>

          {message.has_raw && (
            <a
              className="button entry-line__original"
              href={`/api/v1/email-messages/${message.id}/raw`}
            >
              Download the original
            </a>
          )}
        </div>
      )}
    </div>
  );
}
