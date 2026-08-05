-- Repairs are listed newest first with an optional status filter, so the
-- docket screen can show the open work without reading years of closed jobs.
--
-- The CAST around the narg is what makes sqlc type it as *string rather than
-- interface{}; see the note in transactions.sql.

-- name: ListRepairsByProperty :many
SELECT * FROM repairs
WHERE property_id = sqlc.arg(property_id)
  AND (CAST(sqlc.narg(status) AS TEXT) IS NULL
       OR status = CAST(sqlc.narg(status) AS TEXT))
ORDER BY opened_on DESC, id DESC
LIMIT sqlc.arg(page_size);

-- name: GetRepair :one
SELECT * FROM repairs WHERE id = ? LIMIT 1;

-- name: CountOpenRepairsByProperty :one
SELECT CAST(COUNT(*) AS INTEGER) FROM repairs
WHERE property_id = ? AND status NOT IN ('done', 'wontfix');

-- name: CreateRepair :one
INSERT INTO repairs (
    property_id, unit_id, opened_on, closed_on, status, category, vendor_id,
    description, estimate_cents, actual_cents, is_capex, warranty_until,
    notes, created_at, updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?, ?,
    ?, ?, ?, ?, ?,
    ?, ?, ?
)
RETURNING *;

-- Every column is written, because PATCH is a read-modify-write in Go.
-- name: UpdateRepair :one
UPDATE repairs SET
    unit_id        = ?,
    opened_on      = ?,
    closed_on      = ?,
    status         = ?,
    category       = ?,
    vendor_id      = ?,
    description    = ?,
    estimate_cents = ?,
    actual_cents   = ?,
    is_capex       = ?,
    warranty_until = ?,
    notes          = ?,
    updated_at     = ?
WHERE id = ?
RETURNING *;

-- name: DeleteRepair :execrows
DELETE FROM repairs WHERE id = ?;

-- The timeline -----------------------------------------------------------

-- Oldest first: this is a sequence, and reading it backwards would tell the
-- story in reverse.
-- name: ListRepairEvents :many
SELECT * FROM repair_events WHERE repair_id = ? ORDER BY at, id;

-- name: GetRepairEvent :one
SELECT * FROM repair_events WHERE id = ? LIMIT 1;

-- name: CreateRepairEvent :one
INSERT INTO repair_events (repair_id, at, note, document_id, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: DeleteRepairEvent :execrows
DELETE FROM repair_events WHERE id = ?;
