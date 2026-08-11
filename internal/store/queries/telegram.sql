-- The Telegram channel: one row, id 1.
--
-- The writes are narrow on purpose, like gmail.sql's. Pairing, the long-poll
-- cursor, a mute, a delivery and a failure each touch only the columns they
-- know about, so a send that lands while a mute is being set cannot roll back
-- the muted_until it never read.
--
-- Comments here are ASCII only, like every other query file.

-- name: GetTelegramState :one
SELECT * FROM telegram_state WHERE id = 1 LIMIT 1;

-- The row is created before anything is stored in it, so every narrow UPDATE
-- below has something to update. Issuing a pairing code is the first thing
-- that happens to a fresh install, and it should not also have to know how to
-- create the row.
-- name: EnsureTelegramState :exec
INSERT INTO telegram_state (
    id, chat_id, last_update_id, muted_until, paired_at,
    pairing_code_hash, pairing_expires_at, last_sent_at, last_error, status,
    created_at, updated_at
) VALUES (
    1, NULL, 0, NULL, NULL,
    '', NULL, NULL, '', 'unpaired',
    ?, ?
)
ON CONFLICT (id) DO NOTHING;

-- Only the hash is stored. The code itself is shown once, in the log and on the
-- intake screen, and is never recoverable from the database.
-- name: SetTelegramPairingCode :exec
UPDATE telegram_state SET
    pairing_code_hash  = sqlc.arg(pairing_code_hash),
    pairing_expires_at = CAST(sqlc.arg(pairing_expires_at) AS TEXT),
    updated_at         = sqlc.arg(updated_at)
WHERE id = 1;

-- Pairing consumes the code: the hash and its expiry are cleared in the same
-- statement that stores the chat, so a captured code cannot be replayed against
-- a second chat.
--
-- The guard in the WHERE clause is what makes this single-use even when two
-- updates arrive at once. A caller reads execrows to find out whether it won.
-- The casts are the ones CLAUDE.md warns about: chat_id, paired_at and
-- pairing_expires_at are all nullable columns, and without them sqlc types the
-- parameters as pointers to values the caller can never not have.
-- name: PairTelegram :execrows
UPDATE telegram_state SET
    chat_id            = CAST(sqlc.arg(chat_id) AS INTEGER),
    paired_at          = CAST(sqlc.arg(paired_at) AS TEXT),
    pairing_code_hash  = '',
    pairing_expires_at = NULL,
    last_error         = '',
    status             = 'paired',
    updated_at         = sqlc.arg(updated_at)
WHERE id = 1
  AND chat_id IS NULL
  AND pairing_code_hash = sqlc.arg(pairing_code_hash)
  AND pairing_expires_at IS NOT NULL
  AND pairing_expires_at > CAST(sqlc.arg(now) AS TEXT);

-- Unpairing is server access only (section 8.2). It drops the whole row rather
-- than nulling the chat, because a half-cleared pairing is a state nothing else
-- in this file knows how to read.
-- name: DeleteTelegramState :execrows
DELETE FROM telegram_state WHERE id = 1;

-- name: SetTelegramCursor :exec
UPDATE telegram_state SET
    last_update_id = sqlc.arg(last_update_id),
    updated_at     = sqlc.arg(updated_at)
WHERE id = 1;

-- muted_until keeps its pointer: not muted is a real state, and NULL is how it
-- is spelled.
-- name: SetTelegramMute :exec
UPDATE telegram_state SET
    muted_until = sqlc.narg(muted_until),
    updated_at  = sqlc.arg(updated_at)
WHERE id = 1;

-- A delivery that lands clears the degradation in the same statement, the way
-- SetGmailCursor does. Recovery costs no second write and cannot be forgotten.
-- name: RecordTelegramSent :exec
UPDATE telegram_state SET
    last_sent_at = CAST(sqlc.arg(last_sent_at) AS TEXT),
    last_error   = '',
    status       = 'paired',
    updated_at   = sqlc.arg(updated_at)
WHERE id = 1;

-- degraded is recoverable and is not fatal to the process: the last known state
-- stays on the card, annotated with what went wrong.
-- name: SetTelegramStatus :exec
UPDATE telegram_state SET
    status     = sqlc.arg(status),
    last_error = sqlc.arg(last_error),
    updated_at = sqlc.arg(updated_at)
WHERE id = 1;
