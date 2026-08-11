import { Fragment, type ReactNode } from "react";

import { dayKey, dayRule } from "../format";

interface Props<T> {
  /** The word over the register: "Received", "Sent". */
  eyebrow: string;
  entries: readonly T[];
  /** A stable identity for a row. */
  keyOf: (entry: T) => number | string;
  /** The instant the row is filed under. */
  at: (entry: T) => string;
  render: (entry: T) => ReactNode;
  loading: boolean;
  error: string | null;
  /** What an empty register says. It is an invitation, so it names the next move. */
  empty: string;
}

/**
 * A register kept by day.
 *
 * Both of Intake's cards are one: the mail room's is what arrived and the
 * dispatch book's is what went out, kept in opposite directions but ruled the
 * same way. The date is a rule across the page and every line under it carries
 * only its time, so the times form a column the eye runs down — repeating the
 * date on each line would say nothing eleven times.
 */
export function DayRegister<T>({
  eyebrow,
  entries,
  keyOf,
  at,
  render,
  loading,
  error,
  empty,
}: Props<T>) {
  return (
    <section className="register">
      <p className="register__eyebrow stamped">{eyebrow}</p>

      {error && <p className="register__empty">{error}</p>}
      {!error && loading && <p className="register__empty">Reading the register…</p>}
      {!error && !loading && entries.length === 0 && <p className="register__empty">{empty}</p>}

      {!error &&
        groupByDay(entries, at).map(([day, sameDay]) => (
          <div key={day} className="register__day">
            <p className="register__rule">
              <span className="register__date mono">{dayRule(at(sameDay[0]!))}</span>
            </p>
            {/* A Fragment, not a wrapper: the rows are direct children of the
                day the way they were before this was one component. */}
            {sameDay.map((entry) => (
              <Fragment key={keyOf(entry)}>{render(entry)}</Fragment>
            ))}
          </div>
        ))}
    </section>
  );
}

/** Groups by local day, newest first, preserving the order within a day. */
function groupByDay<T>(entries: readonly T[], at: (entry: T) => string): [string, T[]][] {
  const days = new Map<string, T[]>();
  for (const entry of entries) {
    const key = dayKey(at(entry));
    const bucket = days.get(key);
    if (bucket) {
      bucket.push(entry);
    } else {
      days.set(key, [entry]);
    }
  }
  return [...days.entries()];
}
