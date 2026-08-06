import type { LeaseStatus } from "../../api/types";

interface Props {
  start: string;
  /** Null is month to month: an open end, not a missing one. */
  end: string | null;
  status: LeaseStatus;
}

/**
 * The term of a lease, drawn.
 *
 * A lease's most characteristic fact is that it runs out, and a date does not
 * say how soon. This is the printed timeline on a form: a rule with the term's
 * two ends dated at its ticks, the part already spent inked in, and a mark at
 * today. Reading how much of a tenancy is left becomes looking rather than
 * subtracting.
 *
 * It draws only what the dates say. A month-to-month lease has no end tick,
 * because it has no end — showing one at an arbitrary distance would be the
 * screen inventing a fact. A lease that has not started yet, or one already
 * over, gets no marker: today is not on the rule.
 *
 * Everything here is text and rules in the card's own vocabulary; the numbers
 * come from the same dates the server derives occupancy from, so the picture
 * and the answer elsewhere on the record cannot disagree.
 */
export function TermRule({ start, end, status }: Props) {
  const term = measure(start, end);

  return (
    <div className="term">
      <div
        className="term__rule"
        role="img"
        aria-label={describe(start, end, term, status)}
      >
        {/* The spent portion. On an open-ended term there is nothing to be a
            portion of, so it is left undrawn rather than guessed at. */}
        {term.fraction !== null && (
          <span className="term__spent" style={{ inlineSize: `${term.fraction * 100}%` }} />
        )}
        {term.fraction !== null && (
          <span className="term__today" style={{ insetInlineStart: `${term.fraction * 100}%` }} />
        )}
      </div>

      <div className="term__ticks">
        <span className="term__tick mono">{start}</span>
        <span className="term__left stamped">{term.remark}</span>
        <span className="term__tick term__tick--end mono">{end ?? "no end date"}</span>
      </div>
    </div>
  );
}

interface Term {
  /** How far through the term today is, 0 to 1, or null when unmeasurable. */
  fraction: number | null;
  /** Days from today to the end; negative once it is past. */
  daysLeft: number | null;
  remark: string;
}

const DAY = 86_400_000;

/**
 * Where today sits in the term.
 *
 * Dates are compared as dates. They are stored with no timezone because they
 * never had one, so they are read back at UTC midnight and differenced there —
 * anything else makes "ends today" depend on where the reader is sitting.
 */
function measure(start: string, end: string | null): Term {
  const from = Date.parse(`${start}T00:00:00Z`);
  const now = Date.now();

  if (Number.isNaN(from)) {
    return { fraction: null, daysLeft: null, remark: "" };
  }
  if (end === null) {
    return { fraction: null, daysLeft: null, remark: "month to month" };
  }

  const to = Date.parse(`${end}T00:00:00Z`);
  if (Number.isNaN(to) || to <= from) {
    return { fraction: null, daysLeft: null, remark: "" };
  }

  const daysLeft = Math.ceil((to - now) / DAY);
  const fraction = Math.min(1, Math.max(0, (now - from) / (to - from)));

  return { fraction, daysLeft, remark: remarkFor(daysLeft) };
}

/** What is worth saying about the time left, in the fewest words that say it. */
function remarkFor(daysLeft: number): string {
  if (daysLeft < 0) return "term over";
  if (daysLeft === 0) return "ends today";
  if (daysLeft === 1) return "1 day left";
  if (daysLeft < 60) return `${daysLeft} days left`;
  const months = Math.round(daysLeft / 30);
  return `${months} months left`;
}

/** The same reading, for anyone who cannot see the rule. */
function describe(start: string, end: string | null, term: Term, status: LeaseStatus): string {
  if (end === null) {
    return `Month-to-month lease, ${status}, running from ${start}.`;
  }
  return `Lease from ${start} to ${end}, ${status}. ${capitalise(term.remark)}.`;
}

function capitalise(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}
