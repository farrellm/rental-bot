import { useState } from "react";

import {
  describeError,
  useChannelStanding,
  useIssuePairingCode,
  useNotices,
  useSendTestNotice,
  type ChannelStanding,
  type Notice,
  type PairingCode,
  type Severity,
} from "../api";
import { DayRegister } from "../components/DayRegister";
import { FieldRow } from "../components/FieldRow";
import { Stamp, type StampState } from "../components/Stamp";
import { ago, clock, DASH, hourMinute, plural, timestamp } from "../format";

/** The channel's state and the word stamped on the card are one vocabulary. */
const STATE_STAMP: Record<ChannelStanding["state"], StampState> = {
  paired: "paired",
  muted: "muted",
  "no-contact": "no-contact",
  "not-connected": "not-connected",
  "not-configured": "not-configured",
};

/** The margin marking beside an entry, in the severity's own ink. */
const MARK: Record<Severity, string> = {
  info: "Note",
  warning: "Warning",
  critical: "Urgent",
};

/**
 * The dispatch book.
 *
 * The mail room's card answers "is mail still arriving". This one answers "will
 * I be told when something breaks" — the channel at the head, the register of
 * what has already gone out below it.
 *
 * The register is kept the way a dispatch book is: one line per condition. When
 * the same thing has to be said again after its cooldown, the line is struck
 * again and the tally in the margin goes up, rather than the book filling with
 * the same sentence. A matter that has cleared is ruled off.
 */
export function Dispatch() {
  const standing = useChannelStanding();
  const notices = useNotices();
  // The code is in the response that mints it and nowhere else: only its hash
  // is stored. So it is held here rather than re-read from the standing.
  const [issued, setIssued] = useState<PairingCode | null>(null);

  const data = standing.data ?? null;
  const error = standing.isError ? describeError(standing.error) : null;

  return (
    <section className="card" data-stale={Boolean(error && data)}>
      <header className="card__head">
        <div className="card__title">
          <h1 className="card__mark stamped">Notices</h1>
          <p className="card__origin mono">{origin(data)}</p>
        </div>
        <div className="card__file">
          <p className="card__eyebrow stamped">Record of dispatch</p>
          <p className="card__read mono">{data ? `read ${clock(data.checked_at)}` : DASH}</p>
        </div>
      </header>

      {error && <p className="card__notice">{error}</p>}
      {data?.last_error && !error && <p className="card__notice">{data.last_error}</p>}

      {data && <Standing standing={data} issued={issued} />}

      {/* The same register the mail room keeps, in the other direction. */}
      <DayRegister
        eyebrow="Sent"
        entries={notices.data?.items ?? []}
        keyOf={(notice) => notice.id}
        at={(notice) => notice.first_seen_at}
        render={(notice) => <Line notice={notice} />}
        loading={notices.isPending}
        error={notices.isError ? describeError(notices.error) : null}
        empty={emptyRegister(data)}
      />

      <footer className="card__foot">
        <div className="register__foot">
          <Tally standing={data} />
          <Actions standing={data} onIssued={setIssued} />
        </div>
        {!standing.isPending && data && (
          <Stamp state={STATE_STAMP[data.state]} at={data.checked_at} />
        )}
      </footer>
    </section>
  );
}

/** Who is listening, in the line under the heading. */
function origin(standing: ChannelStanding | null): string {
  if (!standing?.configured) return "no channel set up";
  if (!standing.paired) return `@${standing.bot_username ?? "bot"} · nobody paired`;
  return `@${standing.bot_username ?? "bot"} · chat ${standing.chat_id ?? DASH}`;
}

/** The head entries: everything that answers "will I be told". */
function Standing({ standing, issued }: { standing: ChannelStanding; issued: PairingCode | null }) {
  if (!standing.configured) {
    return (
      <div className="card__fields">
        <p className="register__empty">
          Nothing will be sent anywhere. Conditions are still recorded below. Fill these in and
          restart:
        </p>
        <ul className="register__missing mono">
          {(standing.missing ?? []).map((key) => (
            <li key={key}>{key}</li>
          ))}
        </ul>
      </div>
    );
  }

  if (!standing.paired) {
    return <Pairing standing={standing} issued={issued} />;
  }

  return (
    <dl className="card__fields">
      <FieldRow label="Sends to">
        @{standing.bot_username ?? DASH}, paired{" "}
        {standing.paired_at ? timestamp(standing.paired_at) : DASH}
      </FieldRow>
      <FieldRow label="Last sent">
        {standing.last_sent_at ? ago(standing.last_sent_at) : "nothing has gone out yet"}
      </FieldRow>
      <FieldRow label="Quiet until" tone={standing.state === "muted" ? "fault" : undefined}>
        {standing.muted_until ? timestamp(standing.muted_until) : "not muted"}
      </FieldRow>
      <FieldRow label="Repeats after">{cooldownReading(standing)}</FieldRow>
    </dl>
  );
}

/**
 * The pairing instruction.
 *
 * An empty screen is an invitation to act, so this one is the act: the code,
 * where to send it, and how long it lasts. Unpairing is not offered, and the
 * note says what to run instead — the screen honours the rule rather than
 * describing it.
 */
function Pairing({ standing, issued }: { standing: ChannelStanding; issued: PairingCode | null }) {
  const bot = standing.bot_username ?? "the bot";

  if (!issued) {
    return (
      <div className="card__fields">
        <p className="register__empty">
          Nobody is paired, so nothing is being sent. Get a code and send it to @{bot} from the
          phone you want the alerts on.
        </p>
        {standing.pairing_expires_at && (
          <p className="dispatch__note">
            A code was issued and is still good until {timestamp(standing.pairing_expires_at)}. It
            was shown once; get a new one if you no longer have it.
          </p>
        )}
      </div>
    );
  }

  return (
    <dl className="card__fields">
      <FieldRow label="Pairing code">
        <span className="dispatch__code">{issued.code}</span>
      </FieldRow>
      <FieldRow label="Send to">@{issued.bot_username}</FieldRow>
      <FieldRow label="Send this">
        <span className="dispatch__send">{issued.send}</span>
      </FieldRow>
      <FieldRow label="Good until">
        {timestamp(issued.expires_at)} · shown once, works once
      </FieldRow>
    </dl>
  );
}

function cooldownReading(standing: ChannelStanding): string {
  const hours = Math.round(standing.cooldown_seconds / 3600);
  if (hours < 1) {
    return `${Math.round(standing.cooldown_seconds / 60)} minutes, per condition`;
  }
  return `${plural(hours, "hour", "hours")}, per condition`;
}

/** What the register holds, in the register's own nouns. */
function Tally({ standing }: { standing: ChannelStanding | null }) {
  if (!standing || standing.sent === 0) return null;

  const open = standing.sent - standing.cleared;
  return (
    <p className="register__tally">
      <span className="mono">{standing.sent}</span> {standing.sent === 1 ? "notice" : "notices"}
      {open > 0 && (
        <>
          <span className="register__separator"> · </span>
          <span className="mono">{open}</span> outstanding
        </>
      )}
      {standing.cleared > 0 && (
        <>
          <span className="register__separator"> · </span>
          <span className="mono">{standing.cleared}</span> cleared
        </>
      )}
    </p>
  );
}

/** Get a pairing code, send a test notice. Verbs stay plain. */
function Actions({
  standing,
  onIssued,
}: {
  standing: ChannelStanding | null;
  onIssued: (code: PairingCode) => void;
}) {
  const issue = useIssuePairingCode();
  const test = useSendTestNotice();

  if (!standing?.configured) {
    return <div className="actions" />;
  }

  const failure = issue.error ?? test.error;

  return (
    <div className="register__actions">
      {failure && <p className="register__failure">{describeError(failure)}</p>}
      {test.isSuccess && !test.isPending && <p className="register__tally">Test notice sent.</p>}

      <div className="actions">
        {!standing.paired && (
          <button
            type="button"
            className="button button--primary"
            disabled={issue.isPending}
            onClick={() => issue.mutate(undefined, { onSuccess: onIssued })}
          >
            {issue.isPending ? "Getting a code…" : "Get a pairing code"}
          </button>
        )}

        {standing.paired && (
          <button
            type="button"
            className="button button--primary"
            disabled={test.isPending}
            onClick={() => test.mutate()}
          >
            {test.isPending ? "Sending…" : "Send a test notice"}
          </button>
        )}
      </div>

      {standing.paired && (
        <p className="dispatch__note">
          Unpairing needs a shell on the host:{" "}
          <span className="mono">rental-bot -unpair-telegram</span>. Nothing reachable from a
          browser or from the chat can change who gets these.
        </p>
      )}
    </div>
  );
}

/** An empty register here is the good outcome, and says so. */
function emptyRegister(standing: ChannelStanding | null): string {
  if (!standing?.configured) {
    return "Nothing has been recorded. Anything worth saying will appear here whether or not a channel is set up.";
  }
  return "Nothing has been sent. That is the good outcome — a line here means something needed attention.";
}

/**
 * One line of the dispatch book.
 *
 * Not a control: unlike a received message there is nothing behind it to open.
 * The whole entry is the four things on the line.
 */
function Line({ notice }: { notice: Notice }) {
  const cleared = Boolean(notice.resolved_at);
  const restruck = notice.send_count > 1;

  return (
    <div className={cleared ? "notice-line notice-line--cleared" : "notice-line"}>
      <div className="notice-line__line">
        <span className="notice-line__time mono">{hourMinute(notice.first_seen_at)}</span>
        <span className={`notice-line__mark stamped notice-line__mark--${notice.severity}`}>
          {MARK[notice.severity]}
        </span>
        <span className="notice-line__what">
          <span className="notice-line__title">{notice.title}</span>
          {notice.detail && <span className="notice-line__detail">{notice.detail}</span>}
        </span>
        {restruck && (
          <span
            className="notice-line__tally mono"
            title={`Sent ${notice.send_count} times since ${timestamp(notice.first_seen_at)}`}
          >
            ×{notice.send_count}
          </span>
        )}
        {cleared && notice.resolved_at && (
          <span className="notice-line__cleared stamped">
            cleared {hourMinute(notice.resolved_at)}
          </span>
        )}
      </div>
    </div>
  );
}
