import { useEffect, useState, type ReactNode } from "react";
import { Link, useNavigate, useParams } from "react-router";

import { describeError } from "../../api/client";
import {
  useCreateProperty,
  useCreateUnit,
  useDeleteProperty,
  useDeleteUnit,
  useProperty,
  useUpdateProperty,
  useUpdateUnit,
} from "../../api/queries";
import type { PropertyStatus, Unit } from "../../api/types";
import { Stamp } from "../../components/Stamp";
import { calendarDate, orDashNumber } from "../../format";
import {
  blankUnitDraft,
  emptyDraft,
  toDraft,
  toPropertyWrite,
  toUnitDrafts,
  toUnitWrite,
  unitChanged,
  type Draft,
  type DraftProblem,
  type UnitDraft,
} from "./draft";

const STATUSES: PropertyStatus[] = ["active", "sold", "prospect"];

/**
 * The Overview section: the property's own entries, amended in place.
 *
 * Reading and amending are the same rows: the entries sit on the rules the
 * values sat on, so nothing moves when the mode changes. While the section is
 * open its status stamp is replaced by AMENDING, because what is on it is not
 * yet what is on file.
 *
 * This fills the card's body. The head band and the divider tabs belong to
 * PropertyRecord, which is the folder these sections live in.
 */
export function Overview({ isNew = false }: { isNew?: boolean }) {
  const params = useParams();
  const navigate = useNavigate();

  const id = isNew ? 0 : Number(params.id ?? 0);

  const property = useProperty(id);
  const createProperty = useCreateProperty();
  const updateProperty = useUpdateProperty(id);
  const deleteProperty = useDeleteProperty();
  const createUnit = useCreateUnit(id);
  const updateUnit = useUpdateUnit(id);
  const deleteUnit = useDeleteUnit(id);

  const [editing, setEditing] = useState(isNew);
  const [draft, setDraft] = useState<Draft>(emptyDraft);
  const [original, setOriginal] = useState<Draft>(emptyDraft);
  const [units, setUnits] = useState<UnitDraft[]>([]);
  const [originalUnits, setOriginalUnits] = useState<UnitDraft[]>([]);
  const [problems, setProblems] = useState<DraftProblem[]>([]);
  const [notice, setNotice] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [confirmingDelete, setConfirmingDelete] = useState(false);

  // Seed the draft from the record whenever a fresh one arrives, unless the
  // operator is part way through amending it — overwriting what they have
  // typed because a refetch landed would be its own kind of data loss.
  useEffect(() => {
    if (!property.data || editing) return;
    setDraft(toDraft(property.data));
    setOriginal(toDraft(property.data));
    setUnits(toUnitDrafts(property.data.units));
    setOriginalUnits(toUnitDrafts(property.data.units));
  }, [property.data, editing]);

  const set = <K extends keyof Draft>(key: K, value: Draft[K]) =>
    setDraft((current) => ({ ...current, [key]: value }));

  const setUnit = (key: string, patch: Partial<UnitDraft>) =>
    setUnits((current) => current.map((u) => (u.key === key ? { ...u, ...patch } : u)));

  const live = units.filter((u) => !u.removed);

  // Occupancy comes off the record rather than the draft: it is derived from
  // the lease dates by the server and is not something this card edits.
  const unitsById = new Map<number, Unit>((property.data?.units ?? []).map((u) => [u.id, u]));

  function startAmending() {
    setProblems([]);
    setNotice(null);
    setEditing(true);
  }

  function cancel() {
    setDraft(original);
    setUnits(originalUnits);
    setProblems([]);
    setNotice(null);
    if (isNew) {
      void navigate("/properties");
      return;
    }
    setEditing(false);
  }

  async function save() {
    if (saving) return;

    const { body, problems: fieldProblems } = toPropertyWrite(draft, isNew ? null : original);
    const unitBodies = live.map((unit) => ({ unit, ...toUnitWrite(unit) }));
    const allProblems = [...fieldProblems, ...unitBodies.flatMap((u) => u.problems)];

    if (allProblems.length > 0) {
      setProblems(allProblems);
      setNotice(null);
      return;
    }

    setProblems([]);
    setNotice(null);
    setSaving(true);
    try {
      if (isNew) {
        // One request: the units ride along, and the server makes an implicit
        // one if this list is empty.
        const created = await createProperty.mutateAsync({
          ...body,
          units: unitBodies.map((u) => u.body),
        });
        // Close the card before navigating. This route does not remount when
        // the id changes, so leaving it open would strand the new record in
        // amend mode — and an open card blocks the effect that would seed it
        // with what the server actually stored, including the implicit unit.
        setEditing(false);
        void navigate(`/properties/${created.id}`, { replace: true });
        return;
      }

      // New units first and removals last, so replacing the only unit works:
      // a property is never allowed to pass through having none.
      for (const { unit, body: unitBody } of unitBodies) {
        if (unit.id === null) await createUnit.mutateAsync(unitBody);
      }
      for (const { unit, body: unitBody } of unitBodies) {
        if (unit.id !== null && unitChanged(unit, originalUnits)) {
          await updateUnit.mutateAsync({ id: unit.id, body: unitBody });
        }
      }
      for (const unit of units) {
        if (unit.removed && unit.id !== null) await deleteUnit.mutateAsync(unit.id);
      }

      if (Object.keys(body).length > 0) {
        await updateProperty.mutateAsync(body);
      }
      setEditing(false);
    } catch (err) {
      setNotice(describeError(err));
    } finally {
      setSaving(false);
    }
  }

  async function remove() {
    setSaving(true);
    try {
      await deleteProperty.mutateAsync(id);
      void navigate("/properties", { replace: true });
    } catch (err) {
      setNotice(describeError(err));
      setConfirmingDelete(false);
    } finally {
      setSaving(false);
    }
  }

  const problemFor = (field: string) => problems.find((p) => p.field === field)?.message;

  return (
    <>
      {notice && <p className="card__notice">{notice}</p>}
        {problems.length > 0 && (
          <p className="card__notice">
            {problems.length === 1 ? problems[0]?.message : "Some entries could not be read."}
          </p>
        )}

        <dl className="card__fields">
          <Row label="Nickname" editing={editing} value={draft.nickname || "—"} htmlFor="nickname">
            <input
              id="nickname"
              className={entryClass(problemFor("nickname"))}
              value={draft.nickname}
              onChange={(e) => set("nickname", e.target.value)}
              autoComplete="off"
            />
          </Row>

          <Row label="Address" editing={editing} value={draft.address_line1 || "—"} htmlFor="address_line1">
            <input
              id="address_line1"
              className="entry"
              value={draft.address_line1}
              onChange={(e) => set("address_line1", e.target.value)}
              autoComplete="off"
            />
          </Row>

          <Row label="Line 2" editing={editing} value={draft.address_line2 || "—"} htmlFor="address_line2">
            <input
              id="address_line2"
              className="entry"
              value={draft.address_line2}
              onChange={(e) => set("address_line2", e.target.value)}
              autoComplete="off"
            />
          </Row>

          <Row
            label="City / State / ZIP"
            editing={editing}
            value={[draft.city, draft.state, draft.postal_code].filter(Boolean).join(" ") || "—"}
            htmlFor="city"
          >
            <div className="entry-group">
              <input
                id="city"
                className="entry"
                value={draft.city}
                onChange={(e) => set("city", e.target.value)}
                aria-label="City"
                placeholder="City"
                autoComplete="off"
              />
              <input
                className="entry"
                value={draft.state}
                onChange={(e) => set("state", e.target.value)}
                aria-label="State"
                placeholder="State"
                autoComplete="off"
              />
              <input
                className="entry"
                value={draft.postal_code}
                onChange={(e) => set("postal_code", e.target.value)}
                aria-label="Postal code"
                placeholder="ZIP"
                inputMode="numeric"
                autoComplete="off"
              />
            </div>
          </Row>

          <Row label="County" editing={editing} value={draft.county || "—"} htmlFor="county">
            <input
              id="county"
              className="entry"
              value={draft.county}
              onChange={(e) => set("county", e.target.value)}
              autoComplete="off"
            />
          </Row>

          <Row label="Status" editing={editing} value={draft.status} htmlFor="status">
            <select
              id="status"
              className="entry entry--short"
              value={draft.status}
              onChange={(e) => set("status", e.target.value as PropertyStatus)}
            >
              {STATUSES.map((status) => (
                <option key={status} value={status}>
                  {status}
                </option>
              ))}
            </select>
          </Row>

          <Row
            label="Purchased"
            editing={editing}
            value={calendarDate(draft.purchase_date || null)}
            htmlFor="purchase_date"
            hint={problemFor("purchase_date")}
          >
            <input
              id="purchase_date"
              className={entryClass(problemFor("purchase_date"))}
              value={draft.purchase_date}
              onChange={(e) => set("purchase_date", e.target.value)}
              placeholder="YYYY-MM-DD"
              inputMode="numeric"
              autoComplete="off"
            />
          </Row>

          <Row
            label="Price"
            editing={editing}
            value={draft.purchase_price ? `$${draft.purchase_price}` : "—"}
            htmlFor="purchase_price"
            hint={problemFor("purchase_price")}
          >
            <input
              id="purchase_price"
              className={entryClass(problemFor("purchase_price"))}
              value={draft.purchase_price}
              onChange={(e) => set("purchase_price", e.target.value)}
              placeholder="285000.00"
              inputMode="decimal"
              autoComplete="off"
            />
          </Row>

          <Row
            label="Beds / Baths"
            editing={editing}
            value={`${orDashNumber(numberOrNull(draft.beds))} / ${orDashNumber(numberOrNull(draft.baths))}`}
            htmlFor="beds"
            hint={problemFor("beds") ?? problemFor("baths")}
          >
            <div className="entry-group">
              <input
                id="beds"
                className={entryClass(problemFor("beds"))}
                value={draft.beds}
                onChange={(e) => set("beds", e.target.value)}
                aria-label="Beds"
                inputMode="numeric"
                placeholder="Beds"
                autoComplete="off"
              />
              <input
                className={entryClass(problemFor("baths"))}
                value={draft.baths}
                onChange={(e) => set("baths", e.target.value)}
                aria-label="Baths"
                inputMode="decimal"
                placeholder="Baths"
                autoComplete="off"
              />
            </div>
          </Row>

          <Row
            label="Sq ft / Built"
            editing={editing}
            value={`${orDashNumber(numberOrNull(draft.sqft))} / ${orDashNumber(numberOrNull(draft.year_built))}`}
            htmlFor="sqft"
            hint={problemFor("sqft") ?? problemFor("year_built")}
          >
            <div className="entry-group">
              <input
                id="sqft"
                className={entryClass(problemFor("sqft"))}
                value={draft.sqft}
                onChange={(e) => set("sqft", e.target.value)}
                aria-label="Square feet"
                inputMode="numeric"
                placeholder="Sq ft"
                autoComplete="off"
              />
              <input
                className={entryClass(problemFor("year_built"))}
                value={draft.year_built}
                onChange={(e) => set("year_built", e.target.value)}
                aria-label="Year built"
                placeholder="Year"
                inputMode="numeric"
                autoComplete="off"
              />
            </div>
          </Row>

          <Row label="Notes" editing={editing} value={draft.notes || "—"} htmlFor="notes" block>
            <textarea
              id="notes"
              className="entry"
              value={draft.notes}
              onChange={(e) => set("notes", e.target.value)}
              rows={3}
            />
          </Row>

          {!editing && !isNew && (
            <Row label="Match key" editing={false} value={property.data?.normalized_address || "—"}>
              <span />
            </Row>
          )}
        </dl>

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
                  <span className={occupancyClass(unitsById.get(unit.id ?? 0))}>
                    {occupancy(unitsById.get(unit.id ?? 0))}
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

        <footer className="record__foot">
          <div className="actions">
            {editing ? (
              <>
                <button
                  type="button"
                  className="button button--primary"
                  onClick={() => void save()}
                  disabled={saving}
                >
                  {saving ? "Saving" : isNew ? "Create property" : "Save changes"}
                </button>
                <button type="button" className="button" onClick={cancel} disabled={saving}>
                  Cancel
                </button>
              </>
            ) : (
              <>
                <button type="button" className="button button--primary" onClick={startAmending}>
                  Amend
                </button>
                <Link to="/properties" className="button">
                  Back
                </Link>
                {confirmingDelete ? (
                  <>
                    <span className="hint hint--fault">Delete this record?</span>
                    <button
                      type="button"
                      className="button button--danger"
                      onClick={() => void remove()}
                      disabled={saving}
                    >
                      Delete
                    </button>
                    <button
                      type="button"
                      className="button"
                      onClick={() => setConfirmingDelete(false)}
                    >
                      Keep
                    </button>
                  </>
                ) : (
                  <button
                    type="button"
                    className="button button--danger"
                    onClick={() => setConfirmingDelete(true)}
                  >
                    Delete
                  </button>
                )}
              </>
            )}
          </div>

          <Stamp state={editing ? "amending" : draft.status} />
        </footer>
    </>
  );
}

interface RowProps {
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
 * One ruled entry, in both states.
 *
 * The read value and the entry occupy the same row of the same grid, which is
 * what makes amending cost no reflow.
 */
function Row({ label, editing, value, htmlFor, hint, block, children }: RowProps) {
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

function entryClass(problem: string | undefined): string {
  return problem ? "entry entry--invalid" : "entry";
}

function numberOrNull(text: string): number | null {
  const trimmed = text.trim();
  if (trimmed === "") return null;
  const value = Number(trimmed);
  return Number.isFinite(value) ? value : null;
}

function unitFacts(unit: UnitDraft): string {
  const parts = [
    unit.beds ? `${unit.beds} bd` : null,
    unit.baths ? `${unit.baths} ba` : null,
    unit.sqft ? `${unit.sqft} sq ft` : null,
  ].filter(Boolean);
  return parts.length > 0 ? parts.join("  ") : "—";
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
  return unit.active_lease_end_date ? `Let to ${unit.active_lease_end_date}` : "Let, month to month";
}

function occupancyClass(unit: Unit | undefined): string {
  const let_ = unit != null && unit.active_lease_id != null;
  return let_ ? "unit__standing mono unit__standing--let" : "unit__standing mono";
}
