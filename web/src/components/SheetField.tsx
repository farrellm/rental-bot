import type { ReactNode } from "react";

interface Props {
  label: string;
  /** Set for an entry that wants the full width of the row, like a description. */
  wide?: boolean;
  children: ReactNode;
}

/**
 * One labelled entry on a sheet's form.
 *
 * The label wraps the control rather than pointing at it by id, which is what
 * lets these be written without inventing a unique id for every field on every
 * form on every screen.
 */
export function SheetField({ label, wide, children }: Props) {
  return (
    <label className={wide ? "sheet__field sheet__field--wide" : "sheet__field"}>
      <span className="field__label stamped">{label}</span>
      {children}
    </label>
  );
}

/**
 * A labelled control that narrows what a sheet shows, or changes a standing.
 *
 * The same pair as above with a different label class, because a filter sits
 * over the sheet rather than on it. Kept beside SheetField so the two stay
 * recognisably a pair.
 */
export function SheetFilter({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className="sheet__filter">
      <span className="sheet__filter-label stamped">{label}</span>
      {children}
    </label>
  );
}
