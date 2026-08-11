import type { ReactNode } from "react";

interface Props {
  label: string;
  /** Marks a value that reports a condition needing attention. */
  tone?: "fault";
  children: ReactNode;
}

/** One ruled entry on the card: a stamped label, a typed value. */
export function FieldRow({ label, tone, children }: Props) {
  return (
    <div className="field">
      <dt className="field__label stamped">{label}</dt>
      <dd className={tone ? `field__value mono field__value--${tone}` : "field__value mono"}>
        {children}
      </dd>
    </div>
  );
}

interface AmendableProps {
  label: string;
  editing: boolean;
  /** What the field reads as when the card is closed. */
  value: ReactNode;
  htmlFor?: string;
  hint?: string;
  /** Set when the entry is taller than one line, so the label aligns to its top. */
  block?: boolean;
  children: ReactNode;
}

/**
 * The same entry, in both states.
 *
 * The read value and the entry occupy the same row of the same grid, which is
 * what makes amending cost no reflow. It is FieldRow with a second face rather
 * than a different row: the label, the rule and the geometry are the ones
 * above, and a reader should not be able to tell which component drew a line.
 */
export function AmendableRow({
  label,
  editing,
  value,
  htmlFor,
  hint,
  block,
  children,
}: AmendableProps) {
  const classes = ["field"];
  if (editing) classes.push("field--entry");
  if (editing && block) classes.push("field--block");

  return (
    <div className={classes.join(" ")}>
      <dt className="field__label stamped">
        {editing && htmlFor ? <label htmlFor={htmlFor}>{label}</label> : label}
      </dt>
      <dd className={editing ? "field__value" : "field__value mono"}>
        {editing ? children : value}
        {editing && hint && <p className="hint hint--fault">{hint}</p>}
      </dd>
    </div>
  );
}
