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
