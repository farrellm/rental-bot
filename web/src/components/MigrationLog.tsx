import type { Migration } from "../api";
import { timestamp } from "../format";

interface Props {
  migrations: Migration[];
}

/**
 * The migration ledger at the foot of the card.
 *
 * Migrations are append-only, so this list only ever grows — which is why it
 * reads as a ledger rather than a status field.
 */
export function MigrationLog({ migrations }: Props) {
  return (
    <section className="ledger">
      <h2 className="ledger__eyebrow stamped">Schema history</h2>

      {migrations.length === 0 ? (
        <p className="ledger__empty">
          No migrations recorded. Run <span className="mono">make migrate</span> to
          build the schema.
        </p>
      ) : (
        <ul className="ledger__list">
          {migrations.map((m) => (
            <li className="ledger__row" key={m.version}>
              <span className="ledger__file mono">
                {String(m.version).padStart(4, "0")}_{m.name}.sql
              </span>
              <span className="ledger__at mono">{timestamp(m.applied_at)}</span>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
