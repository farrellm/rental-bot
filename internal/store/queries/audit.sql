-- The audit log.
--
-- Section 5.4 rests on this: an auto-applied receipt is only safe because the
-- row it wrote can be found and undone. The write is inside the same
-- transaction as the thing it records, so a log entry without its effect --
-- or an effect without its entry -- is not a state this database can be in.
--
-- There is no update and no delete on purpose. A record of what was done that
-- can be edited is not one.
--
-- Comments here are ASCII only, like every other query file.

-- name: RecordAudit :one
INSERT INTO audit_log (
    user_id, actor, at, action, entity_type, entity_id, before, after,
    created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: ListAuditForEntity :many
SELECT * FROM audit_log
WHERE entity_type = sqlc.arg(entity_type) AND entity_id = sqlc.arg(entity_id)
ORDER BY at DESC, id DESC
LIMIT sqlc.arg(page_size);
