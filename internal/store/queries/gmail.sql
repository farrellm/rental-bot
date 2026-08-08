-- The connected Gmail account: one row, id 1.
--
-- The writes are narrow on purpose. Connecting replaces the whole row; a sync,
-- a watch renewal, and a failure each touch only the columns they know about,
-- so a sync that finishes while a renewal is in flight cannot roll back the
-- watch expiry it never read.
--
-- Comments here are ASCII only, like every other query file.

-- name: GetGmailAccount :one
SELECT * FROM gmail_account WHERE id = 1 LIMIT 1;

-- Connecting, or reconnecting a different mailbox. The cursor comes from the
-- profile at connect time, so the first sync does not walk history that
-- predates the grant.
-- name: SaveGmailAccount :one
INSERT INTO gmail_account (
    id, address, refresh_token_enc, scopes, connected_at, history_id,
    watch_expires_at, last_sync_at, last_sync_count, last_error, status,
    created_at, updated_at
) VALUES (
    1, ?, ?, ?, ?, ?,
    NULL, NULL, 0, '', 'connected',
    ?, ?
)
ON CONFLICT (id) DO UPDATE SET
    address           = excluded.address,
    refresh_token_enc = excluded.refresh_token_enc,
    scopes            = excluded.scopes,
    connected_at      = excluded.connected_at,
    history_id        = excluded.history_id,
    watch_expires_at  = NULL,
    last_sync_at      = NULL,
    last_sync_count   = 0,
    last_error        = '',
    status            = 'connected',
    updated_at        = excluded.updated_at
RETURNING *;

-- Google rotates a refresh token occasionally. Losing the new one means the
-- grant dies at the next refresh, silently, days later.
-- name: SetGmailRefreshToken :exec
UPDATE gmail_account SET
    refresh_token_enc = sqlc.arg(refresh_token_enc),
    updated_at        = sqlc.arg(updated_at)
WHERE id = 1;

-- A sync always happened at a time, so last_sync_at is cast: the column is
-- nullable, and without the cast sqlc hands the caller a *string for a value it
-- can never not have. watch_expires_at below keeps its pointer, because a watch
-- that failed to register genuinely has no expiry.
-- name: SetGmailCursor :exec
UPDATE gmail_account SET
    history_id      = sqlc.arg(history_id),
    last_sync_at    = CAST(sqlc.arg(last_sync_at) AS TEXT),
    last_sync_count = sqlc.arg(last_sync_count),
    last_error      = '',
    status          = 'connected',
    updated_at      = sqlc.arg(updated_at)
WHERE id = 1;

-- name: SetGmailWatch :exec
UPDATE gmail_account SET
    watch_expires_at = sqlc.arg(watch_expires_at),
    updated_at       = sqlc.arg(updated_at)
WHERE id = 1;

-- degraded is recoverable and revoked is not, but neither is fatal to the
-- process: the last known state stays on the card, annotated with what went
-- wrong (docs/DESIGN.md section 6.1 makes the same argument for valuations).
-- name: SetGmailStatus :exec
UPDATE gmail_account SET
    status     = sqlc.arg(status),
    last_error = sqlc.arg(last_error),
    updated_at = sqlc.arg(updated_at)
WHERE id = 1;

-- name: DeleteGmailAccount :execrows
DELETE FROM gmail_account WHERE id = 1;
