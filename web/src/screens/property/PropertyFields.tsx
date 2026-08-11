import type { PropertyStatus } from "../../api";
import { AmendableRow } from "../../components/FieldRow";
import { calendarDate, DASH, orDashNumber } from "../../format";
import type { Draft } from "./draft";
import { entryClass } from "./entryClass";

const STATUSES: PropertyStatus[] = ["active", "sold", "prospect"];

interface Props {
  draft: Draft;
  set: <K extends keyof Draft>(key: K, value: Draft[K]) => void;
  editing: boolean;
  isNew: boolean;
  /** Derived by the server from the address; shown, never typed. */
  normalizedAddress: string;
  problemFor: (field: string) => string | undefined;
}

/**
 * The property's own entries.
 *
 * Reading and amending are the same rows: the entries sit on the rules the
 * values sat on, so nothing moves when the mode changes. Which is why every
 * row here is one AmendableRow with both faces rather than two blocks of JSX
 * behind a conditional.
 */
export function PropertyFields({
  draft,
  set,
  editing,
  isNew,
  normalizedAddress,
  problemFor,
}: Props) {
  return (
    <dl className="card__fields">
      <AmendableRow
        label="Nickname"
        editing={editing}
        value={draft.nickname || DASH}
        htmlFor="nickname"
      >
        <input
          id="nickname"
          className={entryClass(problemFor("nickname"))}
          value={draft.nickname}
          onChange={(e) => set("nickname", e.target.value)}
          autoComplete="off"
        />
      </AmendableRow>

      <AmendableRow
        label="Address"
        editing={editing}
        value={draft.address_line1 || DASH}
        htmlFor="address_line1"
      >
        <input
          id="address_line1"
          className="entry"
          value={draft.address_line1}
          onChange={(e) => set("address_line1", e.target.value)}
          autoComplete="off"
        />
      </AmendableRow>

      <AmendableRow
        label="Line 2"
        editing={editing}
        value={draft.address_line2 || DASH}
        htmlFor="address_line2"
      >
        <input
          id="address_line2"
          className="entry"
          value={draft.address_line2}
          onChange={(e) => set("address_line2", e.target.value)}
          autoComplete="off"
        />
      </AmendableRow>

      <AmendableRow
        label="City / State / ZIP"
        editing={editing}
        value={[draft.city, draft.state, draft.postal_code].filter(Boolean).join(" ") || DASH}
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
      </AmendableRow>

      <AmendableRow label="County" editing={editing} value={draft.county || DASH} htmlFor="county">
        <input
          id="county"
          className="entry"
          value={draft.county}
          onChange={(e) => set("county", e.target.value)}
          autoComplete="off"
        />
      </AmendableRow>

      {/* Not a Select: these options label themselves with the wire value,
            the way the closed card reads it back. Giving them written-out
            words would change what the record says. */}
      <AmendableRow label="Status" editing={editing} value={draft.status} htmlFor="status">
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
      </AmendableRow>

      <AmendableRow
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
      </AmendableRow>

      <AmendableRow
        label="Price"
        editing={editing}
        value={draft.purchase_price ? `$${draft.purchase_price}` : DASH}
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
      </AmendableRow>

      <AmendableRow
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
      </AmendableRow>

      <AmendableRow
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
      </AmendableRow>

      <AmendableRow
        label="Notes"
        editing={editing}
        value={draft.notes || DASH}
        htmlFor="notes"
        block
      >
        <textarea
          id="notes"
          className="entry"
          value={draft.notes}
          onChange={(e) => set("notes", e.target.value)}
          rows={3}
        />
      </AmendableRow>

      {!editing && !isNew && (
        <AmendableRow label="Match key" editing={false} value={normalizedAddress || DASH}>
          <span />
        </AmendableRow>
      )}
    </dl>
  );
}

/**
 * A typed number, for reading back a value the operator is part way through
 * typing. Anything unparseable reads as unknown here; `toPropertyWrite` is
 * where it becomes a problem the operator is told about.
 */
function numberOrNull(text: string): number | null {
  const trimmed = text.trim();
  if (trimmed === "") return null;
  const value = Number(trimmed);
  return Number.isFinite(value) ? value : null;
}
