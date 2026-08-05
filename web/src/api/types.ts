/**
 * The wire types, mirroring the Go DTOs in internal/httpapi.
 *
 * Money is an integer count of cents here as it is everywhere else — in the
 * database, in Go, and on the wire (docs/DESIGN.md §3). Never a float, never a
 * decimal string. A nullable column is `| null` rather than optional, because
 * the API distinguishes "unknown" from "absent from this response".
 */

/** An RFC 7807 error body. Every error this API returns has this shape. */
export interface Problem {
  type: string;
  title: string;
  status: number;
  detail?: string;
  instance?: string;
}

export interface User {
  id: number;
  username: string;
  email: string;
}

export type PropertyStatus = "active" | "sold" | "prospect";

export interface Property {
  id: number;
  nickname: string;
  address_line1: string;
  address_line2: string;
  city: string;
  state: string;
  postal_code: string;
  county: string;
  /** Derived by the server from the address; read-only. */
  normalized_address: string;
  purchase_date: string | null;
  /** Whole cents. */
  purchase_price_cents: number | null;
  beds: number | null;
  baths: number | null;
  sqft: number | null;
  year_built: number | null;
  status: PropertyStatus;
  zpid: string | null;
  notes: string;
  created_at: string;
  updated_at: string;
}

/** An index row: the property plus how many units it has. */
export interface PropertyListItem extends Property {
  unit_count: number;
}

/** The detail response, which carries the units inline. */
export interface PropertyDetail extends Property {
  units: Unit[];
}

export interface PropertyPage {
  items: PropertyListItem[];
  next_cursor?: string;
}

export interface Unit {
  id: number;
  property_id: number;
  label: string;
  beds: number | null;
  baths: number | null;
  sqft: number | null;
  created_at: string;
  updated_at: string;
}

export interface UnitList {
  items: Unit[];
}

/**
 * The writable half of a property.
 *
 * Every field is optional because PATCH sends only what changed, and null is
 * meaningful: it clears the column. Omitting a field leaves it alone.
 */
export interface PropertyWrite {
  nickname?: string;
  address_line1?: string;
  address_line2?: string;
  city?: string;
  state?: string;
  postal_code?: string;
  county?: string;
  purchase_date?: string | null;
  purchase_price_cents?: number | null;
  beds?: number | null;
  baths?: number | null;
  sqft?: number | null;
  year_built?: number | null;
  status?: PropertyStatus;
  zpid?: string | null;
  notes?: string;
  units?: UnitWrite[];
}

export interface UnitWrite {
  label?: string;
  beds?: number | null;
  baths?: number | null;
  sqft?: number | null;
}

/** One named readiness condition, as reported by /readyz and /api/v1/status. */
export interface Check {
  name: string;
  ok: boolean;
  detail?: string;
}

/** A migration recorded in schema_migrations. */
export interface Migration {
  version: number;
  name: string;
  checksum: string;
  applied_at: string;
}

/** The body of GET /api/v1/status. */
export interface Status {
  status: "operational" | "degraded";
  version: string;
  commit: string;
  build_date: string;
  go_version: string;
  started_at: string;
  uptime_seconds: number;
  schema_version: number;
  database: string;
  checks: Check[];
  migrations: Migration[];
  checked_at: string;
}
