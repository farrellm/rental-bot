-- 0002_documents_and_ledger -- the tables M2 touches.
--
-- Documents and the blob store, the cash-flow ledger, repairs and their
-- timeline, and tenancy. M3's email ingestion and M4's proposal gate both write
-- into exactly these tables, so the shape here is the one those milestones
-- inherit.
--
-- Conventions, from docs/DESIGN.md section 3, unchanged from 0001:
--   * id INTEGER PRIMARY KEY, created_at, updated_at on every table.
--   * Money is INTEGER cents, signed. Income positive, expense negative.
--   * Calendar dates off documents are TEXT 'YYYY-MM-DD'.
--   * Timestamps are TEXT RFC3339 in UTC.
--   * Enums are TEXT with a CHECK. Booleans are INTEGER CHECK (x IN (0, 1)),
--     because STRICT has no boolean type.
--
-- Two columns from section 3.2 are deliberately absent: transactions.proposal_id
-- and documents.source_message_id. Both point at tables that arrive in M3 and
-- M4, and with foreign_keys=ON a declared reference to a table that does not
-- exist yet fails at INSERT time rather than at CREATE. The milestone that
-- creates the target adds the column together with its constraint.

-- People and companies ---------------------------------------------------

CREATE TABLE vendors (
    id           INTEGER PRIMARY KEY,
    name         TEXT NOT NULL,
    trade        TEXT NOT NULL DEFAULT '',
    phone        TEXT NOT NULL DEFAULT '',
    email        TEXT NOT NULL DEFAULT '',
    notes        TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
) STRICT;

CREATE INDEX vendors_name_idx ON vendors (name);

CREATE TABLE tenants (
    id           INTEGER PRIMARY KEY,
    name         TEXT NOT NULL,
    email        TEXT NOT NULL DEFAULT '',
    phone        TEXT NOT NULL DEFAULT '',
    notes        TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
) STRICT;

CREATE INDEX tenants_name_idx ON tenants (name);

-- Documents --------------------------------------------------------------

-- The row is the record; the bytes live in the content-addressed blob store
-- under storage.blobs (section 9.1). sha256 is UNIQUE, which is what makes
-- re-forwarding the same PDF a no-op rather than a second copy.
CREATE TABLE documents (
    id                INTEGER PRIMARY KEY,
    property_id       INTEGER REFERENCES properties (id) ON DELETE SET NULL,
    kind              TEXT NOT NULL DEFAULT 'other'
                      CHECK (kind IN ('lease', 'insurance', 'receipt', 'statement',
                                      'tax', 'photo', 'correspondence', 'other')),
    title             TEXT NOT NULL DEFAULT '',
    original_filename TEXT NOT NULL DEFAULT '',
    mime              TEXT NOT NULL DEFAULT 'application/octet-stream',
    size_bytes        INTEGER NOT NULL DEFAULT 0,
    sha256            TEXT NOT NULL UNIQUE,
    -- Relative to the configured blob root, so moving the data directory does
    -- not rewrite every row.
    storage_path      TEXT NOT NULL,
    -- Filled by the extraction pipeline from M4; empty until then.
    extracted_text    TEXT NOT NULL DEFAULT '',
    uploaded_by       INTEGER REFERENCES users (id) ON DELETE SET NULL,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL
) STRICT;

CREATE INDEX documents_property_id_idx ON documents (property_id);
CREATE INDEX documents_kind_idx ON documents (kind);

-- Many-to-many, because one lease PDF legitimately backs both the leases row
-- and the deposit transaction (section 3.2).
--
-- entity_type carries a CHECK over the entities that exist at M2. A misspelled
-- type would silently orphan a link, which is worth the table rebuild a later
-- milestone needs to widen the list.
CREATE TABLE document_links (
    id          INTEGER PRIMARY KEY,
    document_id INTEGER NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
    entity_type TEXT NOT NULL
                CHECK (entity_type IN ('property', 'unit', 'transaction', 'repair',
                                       'repair_event', 'lease', 'tenant', 'vendor')),
    entity_id   INTEGER NOT NULL,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    UNIQUE (document_id, entity_type, entity_id)
) STRICT;

CREATE INDEX document_links_entity_idx ON document_links (entity_type, entity_id);
CREATE INDEX document_links_document_id_idx ON document_links (document_id);

-- Operations -------------------------------------------------------------

CREATE TABLE repairs (
    id             INTEGER PRIMARY KEY,
    property_id    INTEGER NOT NULL REFERENCES properties (id) ON DELETE CASCADE,
    unit_id        INTEGER REFERENCES units (id) ON DELETE SET NULL,
    opened_on      TEXT NOT NULL,
    closed_on      TEXT,
    status         TEXT NOT NULL DEFAULT 'open'
                   CHECK (status IN ('open', 'scheduled', 'in_progress', 'done', 'wontfix')),
    category       TEXT NOT NULL DEFAULT '',
    vendor_id      INTEGER REFERENCES vendors (id) ON DELETE SET NULL,
    description    TEXT NOT NULL DEFAULT '',
    estimate_cents INTEGER,
    actual_cents   INTEGER,
    -- The repair-versus-improvement split that matters at tax time.
    is_capex       INTEGER NOT NULL DEFAULT 0 CHECK (is_capex IN (0, 1)),
    warranty_until TEXT,
    notes          TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
) STRICT;

CREATE INDEX repairs_property_status_idx ON repairs (property_id, status);
CREATE INDEX repairs_opened_on_idx ON repairs (opened_on);

-- The timeline: quoted, scheduled, completed, paid. `at` is the event's own
-- time; created_at is when the row was written, and the two differ whenever
-- someone records something that happened last week.
CREATE TABLE repair_events (
    id          INTEGER PRIMARY KEY,
    repair_id   INTEGER NOT NULL REFERENCES repairs (id) ON DELETE CASCADE,
    at          TEXT NOT NULL,
    note        TEXT NOT NULL DEFAULT '',
    document_id INTEGER REFERENCES documents (id) ON DELETE SET NULL,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
) STRICT;

CREATE INDEX repair_events_repair_id_idx ON repair_events (repair_id, at);

-- Tenancy ----------------------------------------------------------------

-- Every lease hangs off a unit, never off a property (section 3.2). A null
-- end_date is a month-to-month tenancy, not a missing value.
--
-- Occupancy is derived from these dates and never stored: a unit is occupied
-- if it has an active lease covering today. There is no is_occupied column to
-- drift out of sync with the leases that are the actual evidence.
CREATE TABLE leases (
    id                  INTEGER PRIMARY KEY,
    unit_id             INTEGER NOT NULL REFERENCES units (id) ON DELETE CASCADE,
    start_date          TEXT NOT NULL,
    end_date            TEXT,
    rent_cents          INTEGER NOT NULL,
    deposit_cents       INTEGER,
    due_day             INTEGER CHECK (due_day IS NULL OR (due_day >= 1 AND due_day <= 31)),
    late_fee_cents      INTEGER,
    status              TEXT NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'active', 'ended', 'terminated')),
    renewal_of_lease_id INTEGER REFERENCES leases (id) ON DELETE SET NULL,
    document_id         INTEGER REFERENCES documents (id) ON DELETE SET NULL,
    notes               TEXT NOT NULL DEFAULT '',
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL
) STRICT;

CREATE INDEX leases_unit_status_idx ON leases (unit_id, status);
CREATE INDEX leases_end_date_idx ON leases (end_date);

CREATE TABLE lease_tenants (
    id         INTEGER PRIMARY KEY,
    lease_id   INTEGER NOT NULL REFERENCES leases (id) ON DELETE CASCADE,
    tenant_id  INTEGER NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    role       TEXT NOT NULL DEFAULT 'primary'
               CHECK (role IN ('primary', 'cosigner', 'occupant')),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (lease_id, tenant_id)
) STRICT;

CREATE INDEX lease_tenants_tenant_id_idx ON lease_tenants (tenant_id);

-- The ledger -------------------------------------------------------------

-- amount_cents is signed: income positive, expense negative. That sign is the
-- only thing distinguishing the two, in the database, in Go, and on the wire.
--
-- source and needs_review are provenance. Manual entry sets 'manual' and
-- clears the flag; M4's proposal gate is what writes 'email' rows, and an
-- auto-applied one stays flagged until a human clears it (section 5.4).
CREATE TABLE transactions (
    id             INTEGER PRIMARY KEY,
    property_id    INTEGER NOT NULL REFERENCES properties (id) ON DELETE CASCADE,
    occurred_on    TEXT NOT NULL,
    amount_cents   INTEGER NOT NULL,
    category       TEXT NOT NULL DEFAULT 'other'
                   CHECK (category IN ('rent_income', 'other_income', 'mortgage_payment',
                                       'insurance', 'property_tax', 'hoa', 'mgmt_fee',
                                       'repair', 'capex', 'utilities', 'legal', 'other')),
    description    TEXT NOT NULL DEFAULT '',
    counterparty   TEXT NOT NULL DEFAULT '',
    payment_method TEXT NOT NULL DEFAULT '',
    unit_id        INTEGER REFERENCES units (id) ON DELETE SET NULL,
    lease_id       INTEGER REFERENCES leases (id) ON DELETE SET NULL,
    repair_id      INTEGER REFERENCES repairs (id) ON DELETE SET NULL,
    vendor_id      INTEGER REFERENCES vendors (id) ON DELETE SET NULL,
    document_id    INTEGER REFERENCES documents (id) ON DELETE SET NULL,
    source         TEXT NOT NULL DEFAULT 'manual'
                   CHECK (source IN ('manual', 'email', 'import')),
    confidence     REAL,
    needs_review   INTEGER NOT NULL DEFAULT 0 CHECK (needs_review IN (0, 1)),
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
) STRICT;

-- The ledger query: one property, newest entry first.
CREATE INDEX transactions_property_occurred_idx ON transactions (property_id, occurred_on);
CREATE INDEX transactions_category_idx ON transactions (category);
CREATE INDEX transactions_needs_review_idx ON transactions (needs_review);
