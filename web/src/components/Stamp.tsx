import { clock } from "../format";

/**
 * Every state this application stamps onto a card.
 *
 * The stamp began as a health indicator and is now the state machine for the
 * whole product: a service reading, a property's standing, a card open for
 * amendment, a refused sign-in. One component, one vocabulary, and a reader
 * who learns to look in the same corner on every screen.
 */
export type StampState =
  | "operational"
  | "degraded"
  | "no-contact"
  | "active"
  | "prospect"
  | "sold"
  | "amending"
  | "refused"
  // Repairs: where a job stands.
  | "open"
  | "scheduled"
  | "in-progress"
  | "done"
  | "wontfix"
  // Leases: where a tenancy stands. `active` is shared with a property, on
  // purpose -- a let unit and a held property are the same idea.
  | "pending"
  | "ended"
  | "terminated"
  // Intake: the mailbox itself. Not connected and not configured are different
  // claims -- nobody asked for ingestion, versus somebody did and did not
  // finish -- and a screen that says the wrong one sends the operator looking
  // for a fault that is not there.
  | "watching"
  | "lapsed"
  | "revoked"
  | "not-connected"
  | "not-configured"
  // The alert channel. `not-connected` and `not-configured` are shared with
  // the mailbox on purpose -- they are the same two claims about a different
  // subsystem, and a reader who has learned them once should not have to learn
  // a second pair of words for them. `no-contact` is shared with the service
  // record for the same reason: something that should answer is not answering.
  | "paired"
  | "muted"
  // Intake: where one message stands. All seven of the database's dispositions
  // have a word, including the four M4 writes, so the register can never show
  // a state it has no word for.
  | "received"
  | "parsing"
  | "needs-review"
  | "applied"
  | "rejected"
  | "ignored"
  | "failed";

const WORD: Record<StampState, string> = {
  operational: "Operational",
  degraded: "Degraded",
  "no-contact": "No contact",
  active: "Active",
  prospect: "Prospect",
  sold: "Sold",
  amending: "Amending",
  refused: "Refused",
  open: "Open",
  scheduled: "Scheduled",
  "in-progress": "In progress",
  done: "Done",
  wontfix: "Won't fix",
  pending: "Pending",
  ended: "Ended",
  terminated: "Terminated",
  watching: "Watching",
  lapsed: "Lapsed",
  revoked: "Revoked",
  "not-connected": "Not connected",
  "not-configured": "Not set up",
  paired: "Paired",
  muted: "Muted",
  received: "Received",
  parsing: "Parsing",
  "needs-review": "Needs review",
  applied: "Applied",
  rejected: "Rejected",
  ignored: "Ignored",
  failed: "Failed",
};

interface Props {
  state: StampState;
  /** RFC3339 timestamp of the reading this stamp records, where there is one. */
  at?: string;
  /** Renders the stamp smaller, for an index card rather than a full record. */
  small?: boolean;
}

/**
 * The status stamp: the one bold mark on the card.
 *
 * It is announced politely, and it always says a word as well as wearing a
 * colour, so the state survives both a screen reader and a monochrome
 * screenshot.
 */
export function Stamp({ state, at, small }: Props) {
  const classes = ["stamp", `stamp--${state}`];
  if (small) classes.push("stamp--small");

  return (
    <div className={classes.join(" ")} role="status" aria-live="polite">
      <span className="stamp__word stamped">{WORD[state]}</span>
      {at && <span className="stamp__at mono">{clock(at)}</span>}
    </div>
  );
}
