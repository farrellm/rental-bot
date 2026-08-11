/**
 * The editable shape of a record.
 *
 * Every value is a string, because that is what an input holds. Converting at
 * the boundary — once on the way in, once on the way out — keeps the "3" the
 * operator is halfway through typing from being read as a number and snapped
 * back to 3 under their cursor.
 */

import { isCalendarDate, money, parseMoney } from "../../format";
import type {
  PropertyDetail,
  PropertyStatus,
  PropertyWrite,
  Unit,
  UnitWrite,
} from "../../api/types";

export interface Draft {
  nickname: string;
  address_line1: string;
  address_line2: string;
  city: string;
  state: string;
  postal_code: string;
  county: string;
  purchase_date: string;
  purchase_price: string;
  beds: string;
  baths: string;
  sqft: string;
  year_built: string;
  status: PropertyStatus;
  notes: string;
}

export interface UnitDraft {
  /** Stable across renders, so React does not reuse a row for another unit. */
  key: string;
  /** Null until the unit has been saved. */
  id: number | null;
  label: string;
  beds: string;
  baths: string;
  sqft: string;
  removed: boolean;
}

export const emptyDraft: Draft = {
  nickname: "",
  address_line1: "",
  address_line2: "",
  city: "",
  state: "",
  postal_code: "",
  county: "",
  purchase_date: "",
  purchase_price: "",
  beds: "",
  baths: "",
  sqft: "",
  year_built: "",
  status: "active",
  notes: "",
};

export function toDraft(property: PropertyDetail): Draft {
  return {
    nickname: property.nickname,
    address_line1: property.address_line1,
    address_line2: property.address_line2,
    city: property.city,
    state: property.state,
    postal_code: property.postal_code,
    county: property.county,
    purchase_date: property.purchase_date ?? "",
    // Shown the way it is typed back: "285,000.00", not 28500000.
    purchase_price:
      property.purchase_price_cents === null
        ? ""
        : money(property.purchase_price_cents).replace("$", ""),
    beds: numberToText(property.beds),
    baths: numberToText(property.baths),
    sqft: numberToText(property.sqft),
    year_built: numberToText(property.year_built),
    status: property.status,
    notes: property.notes,
  };
}

export function toUnitDrafts(units: Unit[]): UnitDraft[] {
  return units.map((unit) => ({
    key: `unit-${unit.id}`,
    id: unit.id,
    label: unit.label,
    beds: numberToText(unit.beds),
    baths: numberToText(unit.baths),
    sqft: numberToText(unit.sqft),
    removed: false,
  }));
}

let newUnitCounter = 0;

export function blankUnitDraft(): UnitDraft {
  newUnitCounter += 1;
  return {
    key: `new-${newUnitCounter}`,
    id: null,
    label: "",
    beds: "",
    baths: "",
    sqft: "",
    removed: false,
  };
}

function numberToText(value: number | null): string {
  return value === null ? "" : String(value);
}

/**
 * Reads a typed number. An empty field is null — the value is unknown, which
 * is not the same as zero — and anything unparseable is undefined, which the
 * caller reports rather than rounding away.
 */
function textToNumber(text: string, whole: boolean): number | null | undefined {
  const trimmed = text.trim();
  if (trimmed === "") return null;

  const pattern = whole ? /^-?\d+$/ : /^-?\d*\.?\d*$/;
  if (!pattern.test(trimmed) || trimmed === "." || trimmed === "-") return undefined;

  const value = Number(trimmed);
  return Number.isFinite(value) ? value : undefined;
}

/** A field that could not be read, named so the operator can fix it. */
export interface DraftProblem {
  field: string;
  message: string;
}

/**
 * Turns a draft into a body for the API, or lists what could not be read.
 *
 * Only what changed is sent. Sending the whole record would work — the API
 * takes every field — but a patch that names only what moved is the honest
 * description of what happened, and it leaves everything else genuinely
 * untouched rather than rewritten with its own value.
 */
export function toPropertyWrite(
  draft: Draft,
  original: Draft | null,
): { body: PropertyWrite; problems: DraftProblem[] } {
  const body: PropertyWrite = {};
  const problems: DraftProblem[] = [];

  const text = (key: keyof Draft & keyof PropertyWrite) => {
    if (original && draft[key] === original[key]) return;
    (body as Record<string, unknown>)[key] = String(draft[key]).trim();
  };

  text("nickname");
  text("address_line1");
  text("address_line2");
  text("city");
  text("state");
  text("postal_code");
  text("county");
  text("notes");

  if (!original || draft.status !== original.status) {
    body.status = draft.status;
  }

  if (!original || draft.purchase_date !== original.purchase_date) {
    const date = draft.purchase_date.trim();
    if (date === "") {
      body.purchase_date = null;
    } else if (!isCalendarDate(date)) {
      problems.push({ field: "purchase_date", message: "Write the date as YYYY-MM-DD." });
    } else {
      body.purchase_date = date;
    }
  }

  if (!original || draft.purchase_price !== original.purchase_price) {
    const cents = parseMoney(draft.purchase_price);
    if (cents === undefined) {
      problems.push({
        field: "purchase_price",
        message: "Write the price as an amount, like 285000.00.",
      });
    } else {
      body.purchase_price_cents = cents;
    }
  }

  const number = (key: "beds" | "baths" | "sqft" | "year_built", whole: boolean, label: string) => {
    if (original && draft[key] === original[key]) return;
    const value = textToNumber(draft[key], whole);
    if (value === undefined) {
      problems.push({ field: key, message: `${label} has to be a number.` });
      return;
    }
    body[key] = value;
  };

  number("beds", true, "Beds");
  number("baths", false, "Baths");
  number("sqft", true, "Square feet");
  number("year_built", true, "Year built");

  return { body, problems };
}

/** The same conversion for a unit. */
export function toUnitWrite(draft: UnitDraft): { body: UnitWrite; problems: DraftProblem[] } {
  const body: UnitWrite = { label: draft.label.trim() };
  const problems: DraftProblem[] = [];

  if (body.label === "") {
    problems.push({ field: `unit-${draft.key}`, message: "Every unit needs a label." });
  }

  const number = (key: "beds" | "baths" | "sqft", whole: boolean, label: string) => {
    const value = textToNumber(draft[key], whole);
    if (value === undefined) {
      problems.push({ field: `unit-${draft.key}-${key}`, message: `${label} has to be a number.` });
      return;
    }
    body[key] = value;
  };

  number("beds", true, "Beds");
  number("baths", false, "Baths");
  number("sqft", true, "Square feet");

  return { body, problems };
}

/** True when a unit row differs from the one it was loaded from. */
export function unitChanged(draft: UnitDraft, originals: UnitDraft[]): boolean {
  const original = originals.find((u) => u.key === draft.key);
  if (!original) return true;
  return (
    draft.label !== original.label ||
    draft.beds !== original.beds ||
    draft.baths !== original.baths ||
    draft.sqft !== original.sqft
  );
}
