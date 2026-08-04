-- 0001_init — the tables M0 and M1 touch.
--
-- Conventions, from docs/DESIGN.md §3, applied without exception:
--   * id INTEGER PRIMARY KEY, created_at, updated_at on every table
--     except kv, which is a documented singleton store.
--   * Money is INTEGER cents. Never a float, never a decimal string.
--   * Calendar dates off documents are TEXT 'YYYY-MM-DD'.
--   * Timestamps are TEXT RFC3339 in UTC.
--   * Enums are TEXT with a CHECK constraint, so they read in a shell and
--     violations fail loudly instead of accumulating.
--
-- schema_migrations is not declared here: the migration runner owns it.

-- Small singleton state: the Gmail historyId, last sync timestamps, and
-- anything else that is one row rather than a table.
CREATE TABLE kv (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

-- Auth ------------------------------------------------------------------

CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    email         TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL,
    -- Encrypted at rest with the key from RENTAL_BOT_SECRET_KEY (§9.2).
    -- TOTP is designed for and deferred; the column carries no rows yet.
    totp_secret   TEXT,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
) STRICT;

-- Server-side sessions. Only the hash of the token is stored, so a database
-- copy does not hand over live sessions.
CREATE TABLE sessions (
    id           INTEGER PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash   TEXT NOT NULL UNIQUE,
    expires_at   TEXT NOT NULL,
    user_agent   TEXT NOT NULL DEFAULT '',
    ip           TEXT NOT NULL DEFAULT '',
    last_seen_at TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
) STRICT;

CREATE INDEX sessions_user_id_idx ON sessions (user_id);
CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

-- Properties and structure ----------------------------------------------

CREATE TABLE properties (
    id                  INTEGER PRIMARY KEY,
    nickname            TEXT NOT NULL,
    address_line1       TEXT NOT NULL,
    address_line2       TEXT NOT NULL DEFAULT '',
    city                TEXT NOT NULL DEFAULT '',
    state               TEXT NOT NULL DEFAULT '',
    postal_code         TEXT NOT NULL DEFAULT '',
    county              TEXT NOT NULL DEFAULT '',
    -- Abbreviation-folded, punctuation-collapsed form used to match an
    -- address an LLM read off a document (§5.3). Matching is deterministic
    -- Go against this column; the model never picks the property.
    normalized_address  TEXT NOT NULL DEFAULT '',
    purchase_date       TEXT,
    purchase_price_cents INTEGER,
    beds                INTEGER,
    -- Halves are real: 1.5 baths is not an integer count.
    baths               REAL,
    sqft                INTEGER,
    year_built          INTEGER,
    status              TEXT NOT NULL DEFAULT 'active'
                        CHECK (status IN ('active', 'sold', 'prospect')),
    -- Zillow property id, resolved once and persisted (§6.1).
    zpid                TEXT,
    notes               TEXT NOT NULL DEFAULT '',
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL
) STRICT;

CREATE INDEX properties_normalized_address_idx ON properties (normalized_address);
CREATE INDEX properties_status_idx ON properties (status);

-- Every lease hangs off a unit, so a single-family property gets one
-- implicit unit at creation and the query shape never forks (§3.2).
CREATE TABLE units (
    id          INTEGER PRIMARY KEY,
    property_id INTEGER NOT NULL REFERENCES properties (id) ON DELETE CASCADE,
    label       TEXT NOT NULL,
    beds        INTEGER,
    baths       REAL,
    sqft        INTEGER,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    UNIQUE (property_id, label)
) STRICT;

-- Background work --------------------------------------------------------

CREATE TABLE jobs (
    id           INTEGER PRIMARY KEY,
    kind         TEXT NOT NULL,
    payload      TEXT NOT NULL DEFAULT '{}',
    -- Set by an enqueuer that wants at-most-one pending job for a thing;
    -- this is what absorbs a Pub/Sub redelivery storm (§4.3).
    dedupe_key   TEXT UNIQUE,
    run_after    TEXT NOT NULL,
    attempts     INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 5,
    locked_at    TEXT,
    locked_by    TEXT,
    last_error   TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending', 'running', 'done', 'failed')),
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
) STRICT;

-- The claim query: pending jobs whose time has come, oldest first.
CREATE INDEX jobs_claim_idx ON jobs (status, run_after);
CREATE INDEX jobs_kind_idx ON jobs (kind);
