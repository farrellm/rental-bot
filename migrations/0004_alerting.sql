-- 0004_alerting -- the tables M3.5 touches.
--
-- The channel alerts go out on, and the log of what went out. Section 8.3 says
-- deduplication and cooldown are mandatory rather than polish, and
-- notifications below is the whole of that mechanism: one open row per
-- condition, a tally of how many times it has been restated, and a resolved_at
-- that both closes the condition and frees its key for the next time.
--
-- docs/DESIGN.md section 3.2 also lists an alerts table. It is not here.
-- Every one of its kinds -- lease_expiring, policy_expiring, repair_stale,
-- valuation_stale, proposal_pending -- is a business alert that M6 computes,
-- and a migration that lands a table nothing writes is a migration that lands
-- a table nobody maintains.
--
-- Conventions, from section 3, unchanged since 0001:
--   * id INTEGER PRIMARY KEY, created_at, updated_at on every table.
--   * Timestamps are TEXT RFC3339 in UTC.
--   * Enums are TEXT with a CHECK.
--
-- Comments in this file are ASCII only. sqlc's SQLite rewriter counts comment
-- offsets in runes and slices them in bytes, so one em dash truncates the
-- clause that follows and the query stops parsing.

-- The channel ---------------------------------------------------------------

-- One row, id 1, the same shape gmail_account has and for the same reasons
-- section 3.2 gives: a cursor belongs with the account whose cursor it is, and
-- unpairing is one DELETE rather than a list of keys somebody has to remember.
--
-- A single chat_id is the entire authorization model (section 8.2). Every
-- update is checked against it and dropped otherwise, and re-pairing needs
-- server access -- nothing reachable from Telegram can change who Telegram
-- trusts.
--
-- pairing_code_hash holds the SHA-256 of the code, never the code. That is the
-- discipline sessions.token_hash follows, for the same reason: a copy of this
-- database should not hand over the ability to pair. The code is shown once,
-- in the log and on the intake screen, and is single-use.
--
-- The bot token is not here. Section 9.2 lists it under field encryption, but
-- it is a static credential the operator configures once, which makes it the
-- same kind of thing as the Gmail client secret -- it comes from
-- RENTAL_BOT_TELEGRAM_BOT_TOKEN. The Gmail refresh token is encrypted in the
-- database because OAuth produces it at runtime and there is nowhere else for
-- it to live.
CREATE TABLE telegram_state (
    id                 INTEGER PRIMARY KEY CHECK (id = 1),
    -- NULL until somebody pairs. Telegram chat ids are signed 64-bit.
    chat_id            INTEGER,
    -- The long-poll cursor, persisted so a restart neither replays a command
    -- nor drops one that arrived while the process was down (section 8.1).
    last_update_id     INTEGER NOT NULL DEFAULT 0,
    -- Set by /mute. Everything below critical is suppressed until it passes.
    muted_until        TEXT,
    paired_at          TEXT,
    pairing_code_hash  TEXT NOT NULL DEFAULT '',
    pairing_expires_at TEXT,
    last_sent_at       TEXT,
    -- The last thing that went wrong, kept so the intake screen and /readyz can
    -- say what rather than only that.
    last_error         TEXT NOT NULL DEFAULT '',
    status             TEXT NOT NULL DEFAULT 'unpaired'
                       CHECK (status IN ('unpaired', 'paired', 'degraded')),
    created_at         TEXT NOT NULL,
    updated_at         TEXT NOT NULL
) STRICT;

-- What went out -------------------------------------------------------------

-- The delivery log, and the thing that makes a flapping condition send once and
-- then stay quiet.
--
-- A notification is entered once per condition. When the same condition has to
-- be restated after its cooldown, send_count goes up and last_sent_at moves --
-- the row is not written twice. resolved_at closes it, which is what an
-- explicit recovery message is sent from.
--
-- severity carries all three of section 8.3's values and channel carries both
-- the ones that exist, because widening a CHECK later means rebuilding the
-- table. 'log' is the channel a process with no Telegram configured still
-- records against, so the dispatch register is not empty on a host that has
-- never paired.
CREATE TABLE notifications (
    id            INTEGER PRIMARY KEY,
    -- The stable key for the condition, not for the occurrence. Two reports of
    -- the same lapsed watch share it; two different lapsed watches do not.
    dedupe_key    TEXT NOT NULL,
    channel       TEXT NOT NULL
                  CHECK (channel IN ('telegram', 'log')),
    severity      TEXT NOT NULL
                  CHECK (severity IN ('info', 'warning', 'critical')),
    title         TEXT NOT NULL,
    detail        TEXT NOT NULL DEFAULT '',
    first_seen_at TEXT NOT NULL,
    -- NULL when the row was recorded but delivery has not been attempted, which
    -- is the state a spooled critical alert sits in.
    last_sent_at  TEXT,
    send_count    INTEGER NOT NULL DEFAULT 0,
    resolved_at   TEXT,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
) STRICT;

-- At most one open notification per key per channel. A key has to be reusable
-- once the condition it named has cleared, which SQLite spells as a partial
-- unique index and cannot spell as a column constraint -- the same shape
-- jobs.dedupe_key needed in 0003.
CREATE UNIQUE INDEX notifications_open_idx ON notifications (dedupe_key, channel)
    WHERE resolved_at IS NULL;

-- The dispatch register: newest first.
CREATE INDEX notifications_seen_idx ON notifications (first_seen_at DESC, id DESC);
