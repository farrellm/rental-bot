import { clock } from "../format";

export type StampState = "operational" | "degraded" | "no-contact";

const WORD: Record<StampState, string> = {
  operational: "Operational",
  degraded: "Degraded",
  "no-contact": "No contact",
};

interface Props {
  state: StampState;
  /** RFC3339 timestamp of the reading this stamp records. */
  at: string;
}

/**
 * The status stamp: the one bold mark on the card.
 *
 * It is announced politely, and it always says a word as well as wearing a
 * colour, so the state survives both a screen reader and a monochrome
 * screenshot.
 */
export function Stamp({ state, at }: Props) {
  return (
    <div className={`stamp stamp--${state}`} role="status" aria-live="polite">
      <span className="stamp__word stamped">{WORD[state]}</span>
      <span className="stamp__at mono">{clock(at)}</span>
    </div>
  );
}
