import { useState } from "react";
import { Link, useNavigate } from "react-router";

import {
  describeError,
  useCreateProperty,
  useCreateUnit,
  useDeleteProperty,
  useDeleteUnit,
  useProperty,
  useUpdateProperty,
  useUpdateUnit,
  type PropertyDetail,
} from "../../api";
import { Stamp } from "../../components/Stamp";
import { PropertyFields } from "./PropertyFields";
import { UnitsEditor } from "./UnitsEditor";
import { usePropertyId } from "./usePropertyId";
import {
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
  const navigate = useNavigate();

  // A property that does not exist yet has no id, and every query below is
  // guarded on it being one.
  const routeId = usePropertyId();
  const id = isNew ? 0 : routeId;

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
  //
  // Adjusted during render rather than in an effect. React documents this as
  // the way to reset state when a prop changes, and it is the better one here:
  // an effect paints the stale draft first and replaces it on the next frame,
  // so a refetch landing while the card was closed showed the old values for
  // a beat. This re-renders before anything reaches the screen.
  const [seeded, setSeeded] = useState<PropertyDetail | null>(null);
  if (property.data && !editing && property.data !== seeded) {
    setSeeded(property.data);
    setDraft(toDraft(property.data));
    setOriginal(toDraft(property.data));
    setUnits(toUnitDrafts(property.data.units));
    setOriginalUnits(toUnitDrafts(property.data.units));
  }

  const set = <K extends keyof Draft>(key: K, value: Draft[K]) =>
    setDraft((current) => ({ ...current, [key]: value }));

  const live = units.filter((u) => !u.removed);

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
        // amend mode — and an open card blocks the seeding that would fill it
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

      <PropertyFields
        draft={draft}
        set={set}
        editing={editing}
        isNew={isNew}
        normalizedAddress={property.data?.normalized_address ?? ""}
        problemFor={problemFor}
      />

      <UnitsEditor
        units={units}
        setUnits={setUnits}
        editing={editing}
        isNew={isNew}
        onFile={property.data?.units ?? []}
        problemFor={problemFor}
      />

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
