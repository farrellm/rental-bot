-- 0003_ingestion -- the tables M3 touches.
--
-- The connected Gmail account, the job queue that drains the work, the archive
-- of every message that arrived, and what came attached to it. M4's proposal
-- gate reads exactly these rows, so the shape here is the one it inherits.
--
-- Conventions, from docs/DESIGN.md section 3, unchanged from 0001 and 0002:
--   * id INTEGER PRIMARY KEY, created_at, updated_at on every table.
--   * Timestamps are TEXT RFC3339 in UTC.
--   * Enums are TEXT with a CHECK. Booleans are INTEGER CHECK (x IN (0, 1)).
--
-- Comments in this file are ASCII only. sqlc's SQLite rewriter counts comment
-- offsets in runes and slices them in bytes, so one em dash truncates the
-- RETURNING clause that follows and the query stops parsing.

-- The connected account ---------------------------------------------------

-- One row, id 1. Section 3.2 puts the Gmail historyId and the last sync
-- timestamp in kv; they live here instead, with the account whose cursor they
-- are. Typed columns with a CHECK beat six stringly-typed kv rows, and
-- disconnecting is one DELETE rather than a list of keys to remember. kv stays
-- for what is genuinely singleton state with no table of its own.
--
-- refresh_token_enc is the first encrypted column in the schema: AES-GCM under
-- the key from RENTAL_BOT_SECRET_KEY (section 9.2). A database copy without the
-- key does not hand over the mailbox.
CREATE TABLE gmail_account (
    id                INTEGER PRIMARY KEY CHECK (id = 1),
    address           TEXT NOT NULL,
    refresh_token_enc TEXT NOT NULL,
    scopes            TEXT NOT NULL DEFAULT '',
    connected_at      TEXT NOT NULL,
    -- The history cursor. TEXT because it is an opaque uint64 that is never
    -- arithmetic, and because SQLite's INTEGER is signed.
    history_id        TEXT NOT NULL DEFAULT '',
    -- Google expires a watch after 7 days; the scheduler renews every 24h.
    watch_expires_at  TEXT,
    last_sync_at      TEXT,
    last_sync_count   INTEGER NOT NULL DEFAULT 0,
    -- The last thing that went wrong, kept so the intake screen and /readyz can
    -- say what rather than only that.
    last_error        TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL DEFAULT 'connected'
                      CHECK (status IN ('connected', 'degraded', 'revoked')),
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL
) STRICT;

-- The job queue -----------------------------------------------------------

-- jobs arrived in 0001 with nothing enqueuing onto it. M3 is the first
-- milestone that runs it, and running it exposes one wrong constraint:
-- dedupe_key was declared UNIQUE across the whole table, so a key could be
-- used once and never again. The poller enqueues "gmail.sync" every ten
-- minutes forever, and the second one would fail against the first one's
-- finished row.
--
-- What was wanted is at-most-one *pending* job per key, which SQLite spells as
-- a partial unique index and cannot spell as a column constraint. So the table
-- is rebuilt. Migrations are append-only -- 0001 is history and stays as it is
-- -- and this is the same rebuild that widening a CHECK would need.
ALTER TABLE jobs RENAME TO jobs_old;

DROP INDEX jobs_claim_idx;
DROP INDEX jobs_kind_idx;

-- A bounded worker pool drains this table (section 2). Everything the webhook
-- and the scheduler want done is enqueued rather than run inline, so a slow
-- Gmail call cannot hold a Pub/Sub push open past its deadline.
CREATE TABLE jobs (
    id           INTEGER PRIMARY KEY,
    kind         TEXT NOT NULL,
    payload      TEXT NOT NULL DEFAULT '{}',
    -- Set on a job that is pointless to queue twice. The partial unique index
    -- below is what makes that true; see the note on it.
    dedupe_key   TEXT,
    run_after    TEXT NOT NULL,
    attempts     INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 5,
    -- Held by a worker while it runs. A row still locked past the lease is one
    -- whose process died, and the runner returns it to pending.
    locked_at    TEXT,
    locked_by    TEXT,
    last_error   TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending', 'running', 'done', 'failed')),
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
) STRICT;

INSERT INTO jobs (
    id, kind, payload, dedupe_key, run_after, attempts, max_attempts,
    locked_at, locked_by, last_error, status, created_at, updated_at
)
SELECT
    id, kind, payload, dedupe_key, run_after, attempts, max_attempts,
    locked_at, locked_by, last_error, status, created_at, updated_at
FROM jobs_old;

DROP TABLE jobs_old;

-- The claim query: pending jobs whose time has come, oldest first.
CREATE INDEX jobs_claim_idx ON jobs (status, run_after);
CREATE INDEX jobs_kind_idx ON jobs (kind);

-- Two syncs queued at once do the same walk twice. Pub/Sub is at-least-once and
-- redelivers in storms, so this is the difference between one job and thirty.
-- Partial, over pending only: a dedupe key has to be reusable once the job it
-- named has run.
CREATE UNIQUE INDEX jobs_dedupe_pending_idx ON jobs (dedupe_key)
    WHERE dedupe_key IS NOT NULL AND status = 'pending';

-- What arrived ------------------------------------------------------------

-- gmail_message_id is UNIQUE, and that single constraint is the idempotency key
-- for the whole ingestion pipeline (section 4.2). The fallback poller walks the
-- same history the webhook already delivered; the overlap costs nothing because
-- of this line.
--
-- raw_path is relative to storage.raw_email, so moving the data directory does
-- not rewrite every row -- the same reason documents.storage_path is relative.
--
-- The status CHECK carries all seven values now. M3 writes received, ignored,
-- and failed; parsing, needs_review, applied, and rejected are M4's, and
-- widening a CHECK later means rebuilding the table.
CREATE TABLE email_messages (
    id                INTEGER PRIMARY KEY,
    gmail_message_id  TEXT NOT NULL UNIQUE,
    thread_id         TEXT NOT NULL DEFAULT '',
    from_addr         TEXT NOT NULL DEFAULT '',
    to_addr           TEXT NOT NULL DEFAULT '',
    subject           TEXT NOT NULL DEFAULT '',
    received_at       TEXT NOT NULL,
    snippet           TEXT NOT NULL DEFAULT '',
    raw_path          TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL DEFAULT 'received'
                      CHECK (status IN ('received', 'parsing', 'needs_review',
                                        'applied', 'rejected', 'ignored', 'failed')),
    error             TEXT NOT NULL DEFAULT '',
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL
) STRICT;

-- The register: newest first.
CREATE INDEX email_messages_received_idx ON email_messages (received_at DESC, id DESC);
CREATE INDEX email_messages_status_idx ON email_messages (status);

-- One row per attachment, whether or not its bytes were stored. An attachment
-- past the size cap is recorded with a skipped_reason and no document, because
-- "there was a 40 MB PDF and we did not take it" is a fact worth keeping
-- (section 4.3).
--
-- part_id is the MIME part's index within the message, which is what makes a
-- re-sync of the same message write nothing twice even when two attachments
-- share a filename.
CREATE TABLE email_attachments (
    id               INTEGER PRIMARY KEY,
    email_message_id INTEGER NOT NULL REFERENCES email_messages (id) ON DELETE CASCADE,
    part_id          TEXT NOT NULL,
    filename         TEXT NOT NULL DEFAULT '',
    mime             TEXT NOT NULL DEFAULT 'application/octet-stream',
    size_bytes       INTEGER NOT NULL DEFAULT 0,
    document_id      INTEGER REFERENCES documents (id) ON DELETE SET NULL,
    skipped_reason   TEXT NOT NULL DEFAULT '',
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    UNIQUE (email_message_id, part_id)
) STRICT;

CREATE INDEX email_attachments_document_id_idx ON email_attachments (document_id);

-- Provenance on the document itself. 0002 left this column out on purpose:
-- with foreign_keys=ON a reference to a table that does not exist yet fails at
-- INSERT rather than at CREATE, so the milestone that creates the target adds
-- the column together with its constraint.
--
-- SQLite allows ADD COLUMN with a REFERENCES clause only when the default is
-- NULL, which is exactly what a document uploaded through the web has.
ALTER TABLE documents ADD COLUMN source_message_id INTEGER
    REFERENCES email_messages (id) ON DELETE SET NULL;

CREATE INDEX documents_source_message_id_idx ON documents (source_message_id);
