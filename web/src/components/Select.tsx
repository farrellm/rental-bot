interface Props<T extends string> {
  value: T | "";
  onChange: (value: T) => void;
  /** The values, in the order they should be offered. */
  options: readonly T[];
  /** What each value reads as. The wire keeps the underscored name. */
  labels: Record<T, string>;
  /** An "All" row at the top, for a filter that can select nothing. */
  anyLabel?: string;
  /** Set for the narrow variant used in a filter or a standing picker. */
  short?: boolean;
  id?: string;
}

/**
 * A picker over a closed set.
 *
 * Every enum on the wire arrives here the same way — an array in the order the
 * database's own CHECK lists them, and a record of what each one reads as — so
 * a screen states the vocabulary and not the markup around it.
 */
export function Select<T extends string>({
  value,
  onChange,
  options,
  labels,
  anyLabel,
  short,
  id,
}: Props<T>) {
  return (
    <select
      id={id}
      className={short ? "entry entry--short" : "entry"}
      value={value}
      onChange={(e) => onChange(e.target.value as T)}
    >
      {anyLabel !== undefined && <option value="">{anyLabel}</option>}
      {options.map((option) => (
        <option key={option} value={option}>
          {labels[option]}
        </option>
      ))}
    </select>
  );
}
