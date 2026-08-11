import type { ReactNode } from "react";

interface Props {
  /** The eyebrow over the rows: "New entry", "New repair", "New lease". */
  title: string;
  /** What could not be read, in the operator's words. */
  problem: string | null;
  saving: boolean;
  submitLabel: string;
  onSubmit: () => void;
  onCancel: () => void;
  children: ReactNode;
}

/**
 * A new line being written onto a sheet.
 *
 * Three screens add a row to a list, and all three do it the same way: an
 * eyebrow, a grid of entries, one line saying what could not be read, and a
 * plain pair of verbs. Only the entries and the reading of them differ, so
 * those stay with the caller and the frame lives here.
 *
 * Deliberately not a `<form>`. These sit inside the record's own card, and one
 * form nested in another is not a thing HTML has; the buttons carry the intent
 * instead. That is what the three did before this component existed, and
 * changing it would silently turn Enter into a submit key.
 */
export function SheetForm({
  title,
  problem,
  saving,
  submitLabel,
  onSubmit,
  onCancel,
  children,
}: Props) {
  return (
    <div className="sheet__form">
      <h3 className="sheet__eyebrow stamped">{title}</h3>

      <div className="sheet__form-rows">{children}</div>

      {problem && <p className="hint hint--fault">{problem}</p>}

      <div className="actions">
        <button
          type="button"
          className="button button--primary"
          onClick={onSubmit}
          disabled={saving}
        >
          {saving ? "Saving" : submitLabel}
        </button>
        <button type="button" className="button" onClick={onCancel} disabled={saving}>
          Cancel
        </button>
      </div>
    </div>
  );
}
