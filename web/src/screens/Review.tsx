import { useState } from "react";
import { Link } from "react-router";

import {
  describeError,
  useReviewQueue,
  type ProposalKind,
  type ProposalLine,
  type ProposalStatus,
} from "../api";
import { DayRegister } from "../components/DayRegister";
import { Stamp, type StampState } from "../components/Stamp";
import { hourMinute, plural } from "../format";

/**
 * A proposal's standing, in the vocabulary the whole application stamps with.
 *
 * `auto_applied` reads NEEDS REVIEW rather than getting a word of its own,
 * because that is the true claim: §5.4 files a receipt on three conditions and
 * leaves it flagged until somebody clears it. Which hand filed it is a field
 * on the slip, not a second bold mark.
 */
const STATUS_STAMP: Record<ProposalStatus, StampState> = {
  pending: "pending",
  approved: "applied",
  rejected: "rejected",
  auto_applied: "needs-review",
};

/** What each kind is called on a line. Nouns carry the metaphor. */
const KIND_WORD: Record<ProposalKind, string> = {
  receipt: "receipt",
  lease: "lease",
  insurance: "policy",
  mortgage_statement: "statement",
  repair: "repair",
  valuation: "valuation",
  note: "note",
  unknown: "unread",
};

/**
 * Review: what has been read, and what is waiting to be agreed to.
 *
 * The mail room's register is what arrived; this is what came of it. One line
 * per document, kept by day like every other register in this application,
 * and each one opens the slip where the reading sits beside the document it
 * came off.
 */
export function Review() {
  const [showing, setShowing] = useState<"pending" | "all">("pending");
  const queue = useReviewQueue(showing);

  const counts = queue.data?.counts ?? {};
  const waiting = counts.pending ?? 0;

  return (
    <main className="shell__main shell__main--single">
      <section className="card">
        <header className="card__head">
          <div className="card__title">
            <h1 className="card__mark stamped">Review</h1>
            <p className="card__origin mono">
              {waiting === 0
                ? "nothing waiting"
                : `${plural(waiting, "proposal", "proposals")} waiting`}
            </p>
          </div>
          <div className="card__file">
            <p className="card__eyebrow stamped">Record of review</p>
            <p className="card__read mono">
              {showing === "pending" ? "what is waiting" : "everything read"}
            </p>
          </div>
        </header>

        <DayRegister
          eyebrow={showing === "pending" ? "Waiting" : "Read"}
          entries={queue.data?.items ?? []}
          keyOf={(proposal) => proposal.id}
          at={(proposal) => proposal.received_at}
          render={(proposal) => <Line proposal={proposal} />}
          loading={queue.isPending}
          error={queue.isError ? describeError(queue.error) : null}
          empty={emptyRegister(showing, counts)}
        />

        <footer className="card__foot">
          <div className="register__foot">
            <Tally counts={counts} />
            <div className="actions">
              <button
                type="button"
                className="button"
                onClick={() => setShowing((was) => (was === "pending" ? "all" : "pending"))}
              >
                {showing === "pending" ? "Show what was decided" : "Show only what is waiting"}
              </button>
            </div>
          </div>
        </footer>
      </section>
    </main>
  );
}

/** What the register holds, by standing. */
function Tally({ counts }: { counts: Record<string, number> }) {
  const order: [string, string][] = [
    ["pending", "waiting"],
    ["auto_applied", "filed automatically"],
    ["approved", "approved"],
    ["rejected", "rejected"],
  ];
  const held = order.filter(([key]) => (counts[key] ?? 0) > 0);
  if (held.length === 0) return null;

  return (
    <p className="register__tally">
      {held.map(([key, noun], i) => (
        <span key={key}>
          {i > 0 && <span className="register__separator"> · </span>}
          <span className="mono">{counts[key]}</span> {noun}
        </span>
      ))}
    </p>
  );
}

/** An empty screen is an invitation, and names the next move. */
function emptyRegister(showing: string, counts: Record<string, number>): string {
  if (showing === "all") {
    return "Nothing has been read yet. Forward a receipt to the connected mailbox and it lands here.";
  }
  if ((counts.approved ?? 0) + (counts.rejected ?? 0) + (counts.auto_applied ?? 0) > 0) {
    return "Nothing is waiting. Everything that arrived has been dealt with.";
  }
  return "Nothing is waiting. Forwarded mail that needs a decision lands here.";
}

/**
 * One line of the register.
 *
 * It navigates rather than opening in place, unlike the mail room's. A message
 * is four facts and fits under the line it was on; a proposal is a document
 * and a form read against each other, and that needs the whole card.
 */
function Line({ proposal }: { proposal: ProposalLine }) {
  return (
    <div className="slip-line">
      <Link className="slip-line__line" to={`/review/${proposal.id}`}>
        <span className="slip-line__time mono">{hourMinute(proposal.received_at)}</span>
        <span className="slip-line__what">
          <span className="slip-line__who">{counterparty(proposal)}</span>
          <span className="slip-line__about">
            {proposal.property_nickname ?? (
              <span className="slip-line__unmatched">no property matched</span>
            )}
          </span>
        </span>
        <span className="slip-line__kind stamped">{KIND_WORD[proposal.kind]}</span>
        {proposal.enclosures > 0 && (
          <span className="slip-line__encl mono">{proposal.enclosures} encl.</span>
        )}
        <Stamp state={STATUS_STAMP[proposal.status]} small />
      </Link>
    </div>
  );
}

/**
 * Who a line is about.
 *
 * The extraction names it — a vendor, a carrier, a lender — and the email's
 * subject is the fallback, because a subject is what the operator saw when
 * they forwarded it.
 */
function counterparty(proposal: ProposalLine): string {
  const payload = proposal.payload;
  for (const field of ["vendor_name", "carrier", "lender"]) {
    const value = payload[field];
    if (typeof value === "string" && value.trim() !== "") return value;
  }
  return proposal.subject || "no subject";
}
