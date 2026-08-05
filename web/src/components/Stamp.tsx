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
  | "refused";

const WORD: Record<StampState, string> = {
  operational: "Operational",
  degraded: "Degraded",
  "no-contact": "No contact",
  active: "Active",
  prospect: "Prospect",
  sold: "Sold",
  amending: "Amending",
  refused: "Refused",
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
