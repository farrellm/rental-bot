-- 0005_proposals -- the tables M4 touches.
--
-- The proposal gate, the entities a proposal can resolve into, and the log of
-- what was done on whose word. Section 5.4 calls the gate the design's single
-- most important safety property: nothing an LLM produced reaches the ledger
-- except through ingest_proposals, and every apply is reversible through
-- audit_log.
--
-- Conventions, from docs/DESIGN.md section 3, unchanged since 0001:
--   * id INTEGER PRIMARY KEY, created_at, updated_at on every table.
--   * Money is INTEGER cents. Signed in transactions.amount_cents and nowhere
--     else -- a premium or a payment is a magnitude.
--   * Calendar dates off documents are TEXT 'YYYY-MM-DD'.
--   * Timestamps are TEXT RFC3339 in UTC.
--   * Enums are TEXT with a CHECK. Booleans are INTEGER CHECK (x IN (0, 1)).
--
-- Comments in this file are ASCII only. sqlc's SQLite rewriter counts comment
-- offsets in runes and slices them in bytes, so one em dash truncates the
-- clause that follows and the query stops parsing.

-- The gate ------------------------------------------------------------------

-- What the model claims, before a human has agreed to any of it.
--
-- One row per message that was read, written at classify time and enriched at
-- extract time. A classification of 'unknown' still gets a row: "we could not
-- tell what this is" is a fact the operator needs, and the enclosure is on file
-- either way.
--
-- payload is the extraction as JSON rather than a column per field, because the
-- shape differs per kind and four sparse column sets on one table would be four
-- ways to write the wrong thing. What it means is fixed by the Go struct the
-- schema was reflected off, and it is never read by SQL.
--
-- llm_model, prompt_tokens and completion_tokens are provenance (section 5.3):
-- for cost tracking, so the budget breaker has a ledger to sum, and so an
-- extraction can be replayed after a model change and compared.
--
-- property_id is resolved by deterministic Go over the model's address string
-- and is NULL when the match was ambiguous or found nothing. property_hint
-- keeps what the model actually said, so the screen can show the operator why
-- the match is what it is rather than only that it failed.
CREATE TABLE ingest_proposals (
    id                  INTEGER PRIMARY KEY,
    email_message_id    INTEGER NOT NULL REFERENCES email_messages (id) ON DELETE CASCADE,
    kind                TEXT NOT NULL DEFAULT 'unknown'
                        CHECK (kind IN ('receipt', 'lease', 'insurance',
                                        'mortgage_statement', 'repair',
                                        'valuation', 'note', 'unknown')),
    payload             TEXT NOT NULL DEFAULT '{}',
    llm_model           TEXT NOT NULL DEFAULT '',
    prompt_tokens       INTEGER NOT NULL DEFAULT 0,
    completion_tokens   INTEGER NOT NULL DEFAULT 0,
    confidence          REAL,
    -- What the model said the address was, verbatim, and what the folding
    -- matched it to. The hint is kept even when the match succeeded.
    property_hint       TEXT NOT NULL DEFAULT '',
    property_id         INTEGER REFERENCES properties (id) ON DELETE SET NULL,
    -- The classifier's one sentence about why it said what it said.
    reasoning           TEXT NOT NULL DEFAULT '',
    status              TEXT NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'approved', 'rejected', 'auto_applied')),
    -- NULL on an auto-applied row, which is the point of the distinction: it
    -- was filed, and nobody has looked at it yet.
    reviewed_by         INTEGER REFERENCES users (id) ON DELETE SET NULL,
    reviewed_at         TEXT,
    -- Where it landed. Both NULL until it lands, and they are what makes an
    -- apply traceable from the proposal forward as audit_log makes it
    -- traceable backward.
    applied_entity_type TEXT
                        CHECK (applied_entity_type IS NULL
                               OR applied_entity_type IN ('transaction', 'lease',
                                                          'insurance_policy',
                                                          'mortgage_statement')),
    applied_entity_id   INTEGER,
    -- Why an apply was refused, kept so the screen can say what to fix rather
    -- than only that it did not work.
    error               TEXT NOT NULL DEFAULT '',
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL
) STRICT;

-- The review queue: what is waiting, newest first. The keyset runs backwards
-- over (created_at, id), the shape email_messages already uses.
CREATE INDEX ingest_proposals_queue_idx ON ingest_proposals (status, created_at DESC, id DESC);
CREATE INDEX ingest_proposals_seen_idx ON ingest_proposals (created_at DESC, id DESC);
CREATE INDEX ingest_proposals_message_idx ON ingest_proposals (email_message_id);
CREATE INDEX ingest_proposals_property_idx ON ingest_proposals (property_id);

-- Provenance on the ledger row itself. 0002 left this column out on purpose:
-- with foreign_keys=ON a reference to a table that does not exist yet fails at
-- INSERT rather than at CREATE, so the milestone that creates the target adds
-- the column together with its constraint.
--
-- SQLite allows ADD COLUMN with a REFERENCES clause only when the default is
-- NULL, which is exactly what a manually entered transaction has.
ALTER TABLE transactions ADD COLUMN proposal_id INTEGER
    REFERENCES ingest_proposals (id) ON DELETE SET NULL;

CREATE INDEX transactions_proposal_id_idx ON transactions (proposal_id);

-- Where a proposal lands ----------------------------------------------------

-- Section 3.2's insurance and mortgage tables. They arrive here rather than
-- with the screens that will fill them, because an extract with nowhere to go
-- is a classification into a dead end: M4 reads insurance declarations and
-- mortgage statements off forwarded mail, and this is what it writes them to.
--
-- policy_number_enc and loan_number_enc are AES-GCM under the key from
-- RENTAL_BOT_SECRET_KEY, the discipline gmail_account.refresh_token_enc
-- already follows (section 9.2). A database copy without the key does not hand
-- over an account number.
CREATE TABLE insurance_policies (
    id                       INTEGER PRIMARY KEY,
    property_id              INTEGER NOT NULL REFERENCES properties (id) ON DELETE CASCADE,
    carrier                  TEXT NOT NULL DEFAULT '',
    policy_number_enc        TEXT NOT NULL DEFAULT '',
    type                     TEXT NOT NULL DEFAULT 'hazard'
                             CHECK (type IN ('hazard', 'flood', 'umbrella', 'liability')),
    agent_name               TEXT NOT NULL DEFAULT '',
    agent_phone              TEXT NOT NULL DEFAULT '',
    agent_email              TEXT NOT NULL DEFAULT '',
    effective_date           TEXT,
    expiration_date          TEXT,
    -- A premium is a magnitude. The signed-cents convention is
    -- transactions.amount_cents only.
    annual_premium_cents     INTEGER,
    dwelling_coverage_cents  INTEGER,
    liability_coverage_cents INTEGER,
    deductible_cents         INTEGER,
    document_id              INTEGER REFERENCES documents (id) ON DELETE SET NULL,
    notes                    TEXT NOT NULL DEFAULT '',
    created_at               TEXT NOT NULL,
    updated_at               TEXT NOT NULL
) STRICT;

CREATE INDEX insurance_policies_property_idx ON insurance_policies (property_id);
-- The expiring-policy alert M6 computes reads this.
CREATE INDEX insurance_policies_expiration_idx ON insurance_policies (expiration_date);

CREATE TABLE mortgages (
    id                       INTEGER PRIMARY KEY,
    property_id              INTEGER NOT NULL REFERENCES properties (id) ON DELETE CASCADE,
    lender                   TEXT NOT NULL DEFAULT '',
    loan_number_enc          TEXT NOT NULL DEFAULT '',
    original_principal_cents INTEGER,
    -- Basis points, not a float. 6.375% is 637, and integer arithmetic over it
    -- is exact.
    interest_rate_bps        INTEGER,
    term_months              INTEGER,
    origination_date         TEXT,
    monthly_pi_cents         INTEGER,
    escrow_monthly_cents     INTEGER,
    current_balance_cents    INTEGER,
    -- The date the balance above was true on, which is the only thing that
    -- makes a balance worth storing.
    balance_as_of            TEXT,
    payoff_date              TEXT,
    notes                    TEXT NOT NULL DEFAULT '',
    created_at               TEXT NOT NULL,
    updated_at               TEXT NOT NULL
) STRICT;

CREATE INDEX mortgages_property_idx ON mortgages (property_id);

-- Append-only, which gives an amortization history for free rather than as a
-- feature to build: every statement that ever arrived is a row, and nothing
-- overwrites one.
CREATE TABLE mortgage_statements (
    id                     INTEGER PRIMARY KEY,
    mortgage_id            INTEGER NOT NULL REFERENCES mortgages (id) ON DELETE CASCADE,
    statement_date         TEXT NOT NULL,
    principal_balance_cents INTEGER,
    payment_cents          INTEGER,
    principal_paid_cents   INTEGER,
    interest_paid_cents    INTEGER,
    escrow_paid_cents      INTEGER,
    document_id            INTEGER REFERENCES documents (id) ON DELETE SET NULL,
    created_at             TEXT NOT NULL,
    updated_at             TEXT NOT NULL,
    -- The same statement forwarded twice is one statement. This is the
    -- idempotency key an apply reads before it writes, the way the ingestion
    -- pipeline reads gmail_message_id.
    UNIQUE (mortgage_id, statement_date)
) STRICT;

CREATE INDEX mortgage_statements_mortgage_idx ON mortgage_statements (mortgage_id, statement_date DESC);

-- What was done, and on whose word ------------------------------------------

-- Section 8.5 routes every mutation through this, and section 5.4 rests on it:
-- an auto-applied receipt is only safe because the row it wrote can be found
-- and undone. M4 writes it from the apply path; the rest of the web's
-- mutations join later.
--
-- before and after are JSON snapshots rather than a diff. A diff has to be
-- interpreted against a schema that may since have changed; a snapshot is what
-- the row was.
CREATE TABLE audit_log (
    id          INTEGER PRIMARY KEY,
    user_id     INTEGER REFERENCES users (id) ON DELETE SET NULL,
    -- Who acted. 'system' is the machine acting on its own, which is what an
    -- auto-applied proposal is, and the value that makes those findable.
    actor       TEXT NOT NULL DEFAULT 'web'
                CHECK (actor IN ('web', 'telegram', 'system')),
    at          TEXT NOT NULL,
    -- A verb and its object: 'proposal.approve', 'transaction.create'.
    action      TEXT NOT NULL,
    entity_type TEXT NOT NULL DEFAULT '',
    entity_id   INTEGER,
    before      TEXT NOT NULL DEFAULT '',
    after       TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
) STRICT;

CREATE INDEX audit_log_entity_idx ON audit_log (entity_type, entity_id);
CREATE INDEX audit_log_at_idx ON audit_log (at DESC, id DESC);

-- Widening the link CHECK ---------------------------------------------------

-- document_links.entity_type carried a CHECK over the entities that existed at
-- M2. Three more exist now, and an applied proposal links its enclosure to
-- whatever it produced. Widening a CHECK means rebuilding the table, which is
-- the price of a typo'd type failing loudly instead of silently orphaning a
-- link -- the same rebuild 0003 did to jobs.
ALTER TABLE document_links RENAME TO document_links_old;

DROP INDEX document_links_entity_idx;
DROP INDEX document_links_document_id_idx;

CREATE TABLE document_links (
    id          INTEGER PRIMARY KEY,
    document_id INTEGER NOT NULL REFERENCES documents (id) ON DELETE CASCADE,
    entity_type TEXT NOT NULL
                CHECK (entity_type IN ('property', 'unit', 'transaction', 'repair',
                                       'repair_event', 'lease', 'tenant', 'vendor',
                                       'insurance_policy', 'mortgage', 'mortgage_statement')),
    entity_id   INTEGER NOT NULL,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    UNIQUE (document_id, entity_type, entity_id)
) STRICT;

INSERT INTO document_links (id, document_id, entity_type, entity_id, created_at, updated_at)
SELECT id, document_id, entity_type, entity_id, created_at, updated_at
FROM document_links_old;

DROP TABLE document_links_old;

CREATE INDEX document_links_entity_idx ON document_links (entity_type, entity_id);
CREATE INDEX document_links_document_id_idx ON document_links (document_id);
