import type { Unit } from "../../api";
import { DASH } from "../../format";
import { blankUnitDraft, type UnitDraft } from "./draft";
import { entryClass } from "./entryClass";

interface Props {
  units: UnitDraft[];
  setUnits: (update: (current: UnitDraft[]) => UnitDraft[]) => void;
  editing: boolean;
  isNew: boolean;
  /** The units as the server holds them, which is where occupancy comes from. */
  onFile: Unit[];
  problemFor: (field: string) => string | undefined;
}

/**
 * The units on a property, read and amended.
 *
 * Every property keeps at least one, so Remove is disabled on the last row
 * rather than refused after the fact — the rule is in the write path too, and
 * a button that produces a 409 is a button that should have been greyed.
 */
export function UnitsEditor({ units, setUnits, editing, isNew, onFile, problemFor }: Props) {
  const live = units.filter((u) => !u.removed);

  const setUnit = (key: string, patch: Partial<UnitDraft>) =>
    setUnits((current) => current.map((u) => (u.key === key ? { ...u, ...patch } : u)));

  // Occupancy comes off the record rather than the draft: it is derived from
  // the lease dates by the server and is not something this card edits.
  const byId = new Map<number, Unit>(onFile.map((u) => [u.id, u]));

  return (
    <section className="units">
      <h2 className="units__eyebrow">Units</h2>

      <div className="units__list">
        {live.length === 0 && !editing && <p className="units__empty">No units on file.</p>}

        {live.map((unit) =>
          editing ? (
            <div key={unit.key} className="unit unit--editing">
              <div className="unit__entries">
                <input
                  className={entryClass(problemFor(`unit-${unit.key}`))}
                  value={unit.label}
                  onChange={(e) => setUnit(unit.key, { label: e.target.value })}
                  placeholder="Label"
                  aria-label="Unit label"
                  autoComplete="off"
                />
                <input
                  className={entryClass(problemFor(`unit-${unit.key}-beds`))}
                  value={unit.beds}
                  onChange={(e) => setUnit(unit.key, { beds: e.target.value })}
                  placeholder="Beds"
                  aria-label="Beds"
                  inputMode="numeric"
                  autoComplete="off"
                />
                <input
                  className={entryClass(problemFor(`unit-${unit.key}-baths`))}
                  value={unit.baths}
                  onChange={(e) => setUnit(unit.key, { baths: e.target.value })}
                  placeholder="Baths"
                  aria-label="Baths"
                  inputMode="decimal"
                  autoComplete="off"
                />
                <input
                  className={entryClass(problemFor(`unit-${unit.key}-sqft`))}
                  value={unit.sqft}
                  onChange={(e) => setUnit(unit.key, { sqft: e.target.value })}
                  placeholder="Sq ft"
                  aria-label="Square feet"
                  inputMode="numeric"
                  autoComplete="off"
                />
              </div>
              <div className="unit__actions">
                <button
                  type="button"
                  className="button button--danger"
                  onClick={() => setUnit(unit.key, { removed: true })}
                  disabled={live.length === 1}
                  title={live.length === 1 ? "A property keeps at least one unit." : undefined}
                >
                  Remove
                </button>
              </div>
            </div>
          ) : (
            <div key={unit.key} className="unit">
              <span className="unit__label mono">{unit.label}</span>
              <span className="unit__facts mono">{unitFacts(unit)}</span>
              <span className={occupancyClass(byId.get(unit.id ?? 0))}>
                {occupancy(byId.get(unit.id ?? 0))}
              </span>
            </div>
          ),
        )}
      </div>

      {editing && (
        <div className="units__add">
          <button
            type="button"
            className="button"
            onClick={() => setUnits((current) => [...current, blankUnitDraft()])}
          >
            Add unit
          </button>
          {isNew && live.length === 0 && (
            <p className="hint">A property with no units listed gets one called Main.</p>
          )}
        </div>
      )}
    </section>
  );
}

function unitFacts(unit: UnitDraft): string {
  const parts = [
    unit.beds ? `${unit.beds} bd` : null,
    unit.baths ? `${unit.baths} ba` : null,
    unit.sqft ? `${unit.sqft} sq ft` : null,
  ].filter(Boolean);
  return parts.length > 0 ? parts.join("  ") : DASH;
}

/**
 * What a unit's leases say about it today.
 *
 * Occupancy is derived, never stored: the server answers it from the lease
 * dates on every read. "Let" here means a lease covers today, and the date is
 * the day that stops being true.
 */
function occupancy(unit: Unit | undefined): string {
  if (!unit || unit.active_lease_id == null) return "Vacant";
  return unit.active_lease_end_date
    ? `Let to ${unit.active_lease_end_date}`
    : "Let, month to month";
}

function occupancyClass(unit: Unit | undefined): string {
  const let_ = unit != null && unit.active_lease_id != null;
  return let_ ? "unit__standing mono unit__standing--let" : "unit__standing mono";
}
