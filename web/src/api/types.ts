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
  /**
   * The lease holding this unit today, or null when nothing is.
   *
   * Derived by the server from the lease dates on every read, never stored.
   * It is an id rather than a boolean so a screen can link to the reason for
   * the answer instead of just asserting it.
   */
  active_lease_id?: number | null;
  active_lease_end_date?: string | null;
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

/* Documents ---------------------------------------------------------------- */

export type DocumentKind =
  | "lease"
  | "insurance"
  | "receipt"
  | "statement"
  | "tax"
  | "photo"
  | "correspondence"
  | "other";

/** What a document can be filed against. Mirrors the CHECK in migration 0002. */
export type LinkEntityType =
  | "property"
  | "unit"
  | "transaction"
  | "repair"
  | "repair_event"
  | "lease"
  | "tenant"
  | "vendor";

export interface DocumentLink {
  entity_type: LinkEntityType;
  entity_id: number;
}

export interface Document {
  id: number;
  property_id: number | null;
  kind: DocumentKind;
  title: string;
  original_filename: string;
  mime: string;
  size_bytes: number;
  /** The content's own name. Its first bytes are the accession number. */
  sha256: string;
  uploaded_by: number | null;
  created_at: string;
  updated_at: string;
  /** Present on one document, absent from a list. */
  links?: DocumentLink[];
}

/** An upload result: `deduplicated` means these bytes were already on file. */
export interface UploadedDocument extends Document {
  deduplicated: boolean;
}

export interface DocumentPage {
  items: Document[];
  next_cursor?: string;
}

/* The ledger --------------------------------------------------------------- */

export type TransactionCategory =
  | "rent_income"
  | "other_income"
  | "mortgage_payment"
  | "insurance"
  | "property_tax"
  | "hoa"
  | "mgmt_fee"
  | "repair"
  | "capex"
  | "utilities"
  | "legal"
  | "other";

export interface Transaction {
  id: number;
  property_id: number;
  occurred_on: string;
  /** Whole cents, signed: income positive, expense negative. */
  amount_cents: number;
  category: TransactionCategory;
  description: string;
  counterparty: string;
  payment_method: string;
  unit_id: number | null;
  lease_id: number | null;
  repair_id: number | null;
  vendor_id: number | null;
  document_id: number | null;
  source: "manual" | "email" | "import";
  needs_review: boolean;
  created_at: string;
  updated_at: string;
}

/** The foot of the ledger sheet, computed by the server over the filtered set. */
export interface LedgerTotals {
  income_cents: number;
  expense_cents: number;
  net_cents: number;
  entry_count: number;
}

export interface TransactionPage {
  items: Transaction[];
  totals: LedgerTotals;
  next_cursor?: string;
}

export interface TransactionWrite {
  occurred_on?: string;
  amount_cents?: number;
  category?: TransactionCategory;
  description?: string;
  counterparty?: string;
  payment_method?: string;
  unit_id?: number | null;
  vendor_id?: number | null;
  repair_id?: number | null;
  document_id?: number | null;
  needs_review?: boolean;
}

/* Repairs ------------------------------------------------------------------ */

export type RepairStatus = "open" | "scheduled" | "in_progress" | "done" | "wontfix";

export interface RepairEvent {
  id: number;
  repair_id: number;
  /** RFC3339: when it happened, not when it was written down. */
  at: string;
  note: string;
  document_id: number | null;
  created_at: string;
  updated_at: string;
}

export interface Repair {
  id: number;
  property_id: number;
  unit_id: number | null;
  opened_on: string;
  closed_on: string | null;
  status: RepairStatus;
  category: string;
  vendor_id: number | null;
  description: string;
  estimate_cents: number | null;
  actual_cents: number | null;
  is_capex: boolean;
  warranty_until: string | null;
  notes: string;
  created_at: string;
  updated_at: string;
  /** Present on one repair, absent from a list. */
  events?: RepairEvent[];
}

export interface RepairList {
  items: Repair[];
}

export interface RepairWrite {
  unit_id?: number | null;
  opened_on?: string;
  closed_on?: string | null;
  status?: RepairStatus;
  category?: string;
  vendor_id?: number | null;
  description?: string;
  estimate_cents?: number | null;
  actual_cents?: number | null;
  is_capex?: boolean;
  warranty_until?: string | null;
  notes?: string;
}

/* Tenancy ------------------------------------------------------------------ */

export type LeaseStatus = "pending" | "active" | "ended" | "terminated";
export type TenantRole = "primary" | "cosigner" | "occupant";

export interface Tenant {
  id: number;
  name: string;
  email: string;
  phone: string;
  notes: string;
  created_at: string;
  updated_at: string;
}

export interface TenantList {
  items: Tenant[];
}

export interface LeaseTenant extends Tenant {
  role: TenantRole;
}

export interface Lease {
  id: number;
  unit_id: number;
  unit_label: string;
  start_date: string;
  /** Null is month-to-month, not a missing value. */
  end_date: string | null;
  rent_cents: number;
  deposit_cents: number | null;
  due_day: number | null;
  late_fee_cents: number | null;
  status: LeaseStatus;
  renewal_of_lease_id: number | null;
  document_id: number | null;
  notes: string;
  created_at: string;
  updated_at: string;
  /** Present on one lease, absent from a list. */
  tenants?: LeaseTenant[];
}

export interface LeaseList {
  items: Lease[];
}

export interface LeaseWrite {
  unit_id?: number;
  start_date?: string;
  end_date?: string | null;
  rent_cents?: number;
  deposit_cents?: number | null;
  due_day?: number | null;
  late_fee_cents?: number | null;
  status?: LeaseStatus;
  document_id?: number | null;
  notes?: string;
}

/* Vendors ------------------------------------------------------------------ */

export interface Vendor {
  id: number;
  name: string;
  trade: string;
  phone: string;
  email: string;
  notes: string;
  created_at: string;
  updated_at: string;
}

export interface VendorList {
  items: Vendor[];
}

/* Intake ------------------------------------------------------------------- */

/**
 * What the mailbox is doing, as one word.
 *
 * `not-configured` and `not-connected` are different claims and both are
 * working states: nobody asked for ingestion, versus somebody did and has not
 * finished. Collapsing them shows a fresh clone a broken mailbox.
 */
export type IntakeState =
  | "watching"
  | "lapsed"
  | "degraded"
  | "revoked"
  | "not-connected"
  | "not-configured";

export interface IntakeStanding {
  configured: boolean;
  connected: boolean;
  state: IntakeState;
  address?: string;
  /** Where the operator forwards mail: the connected account's own address. */
  forward_to?: string;
  connected_at?: string;
  history_id?: string;
  watch_expires_at?: string;
  last_sync_at?: string;
  last_sync_count: number;
  last_error?: string;
  allowed_senders: string[];
  poll_interval_seconds: number;
  /** The configuration keys that are not set, when ingestion is off. */
  missing?: string[];
  counts: Record<string, number>;
  queue_depth: Record<string, number>;
  checked_at: string;
}

/** Every disposition the database's CHECK allows. M3 writes three of them. */
export type EmailStatus =
  | "received"
  | "parsing"
  | "needs_review"
  | "applied"
  | "rejected"
  | "ignored"
  | "failed";

export interface EmailAttachment {
  id: number;
  filename: string;
  mime: string;
  size_bytes: number;
  /** Null when the bytes were not stored; skipped_reason says why. */
  document_id: number | null;
  skipped_reason?: string;
}

export interface EmailMessage {
  id: number;
  gmail_message_id: string;
  thread_id: string;
  from_addr: string;
  to_addr: string;
  subject: string;
  received_at: string;
  snippet: string;
  status: EmailStatus;
  error?: string;
  /** False for a message that was never downloaded, so there is no original. */
  has_raw: boolean;
  attachments: EmailAttachment[];
}

export interface EmailMessagePage {
  items: EmailMessage[];
  next_cursor?: string;
}

export interface ConnectResponse {
  authorize_url: string;
}
