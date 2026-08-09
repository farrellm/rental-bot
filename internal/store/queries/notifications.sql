-- The delivery log: what went out, when, and how many times.
--
-- One open row per condition per channel, enforced by the partial unique index
-- in 0004. Everything here follows from that: the bus looks for the open row,
-- and either inserts, restates, or stays quiet. A row is never written twice
-- for the same condition, which is what makes a flapping condition send once
-- and then go silent (docs/DESIGN.md section 8.3).
--
-- Comments here are ASCII only, like every other query file.

-- The condition as it currently stands, or nothing when it is not outstanding.
-- name: GetOpenNotification :one
SELECT * FROM notifications
WHERE dedupe_key = ? AND channel = ? AND resolved_at IS NULL
LIMIT 1;

-- name: InsertNotification :one
INSERT INTO notifications (
    dedupe_key, channel, severity, title, detail,
    first_seen_at, last_sent_at, send_count, resolved_at,
    created_at, updated_at
) VALUES (
    ?, ?, ?, ?, ?,
    ?, NULL, 0, NULL,
    ?, ?
)
RETURNING *;

-- A restatement: the same condition, said again after its cooldown. The tally
-- in the margin of the dispatch register is this column.
-- name: RecordNotificationSent :exec
UPDATE notifications SET
    last_sent_at = CAST(sqlc.arg(last_sent_at) AS TEXT),
    send_count   = send_count + 1,
    severity     = sqlc.arg(severity),
    title        = sqlc.arg(title),
    detail       = sqlc.arg(detail),
    updated_at   = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id);

-- Closing a condition. execrows is what tells the caller whether there was
-- anything to close, and therefore whether a recovery message is owed -- a
-- probe calls this on every sweep, and only the sweep that finds an open row
-- should say anything.
-- name: ResolveNotification :execrows
UPDATE notifications SET
    resolved_at = CAST(sqlc.arg(resolved_at) AS TEXT),
    updated_at  = sqlc.arg(updated_at)
WHERE dedupe_key = sqlc.arg(dedupe_key)
  AND channel = sqlc.arg(channel)
  AND resolved_at IS NULL;

-- The register, newest first. Keyset pagination on first_seen_at with id
-- breaking the tie, the same shape every other list in this schema uses.
-- name: ListNotificationsFirstPage :many
SELECT * FROM notifications
ORDER BY first_seen_at DESC, id DESC
LIMIT ?;

-- name: ListNotificationsAfter :many
SELECT * FROM notifications
WHERE (first_seen_at < sqlc.arg(after_first_seen_at))
   OR (first_seen_at = sqlc.arg(after_first_seen_at) AND id < sqlc.arg(after_id))
ORDER BY first_seen_at DESC, id DESC
LIMIT sqlc.arg(page_size);

-- The foot of the dispatch card: how much the register holds without counting
-- the lines. COUNT rather than SUM over a CASE, because SUM of no rows is NULL
-- and a tally of nothing is zero.
-- name: CountNotifications :one
SELECT
    COUNT(*) AS total,
    COUNT(CASE WHEN resolved_at IS NULL THEN 1 END) AS outstanding
FROM notifications;
