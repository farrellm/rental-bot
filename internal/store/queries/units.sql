-- name: ListUnitsByProperty :many
SELECT * FROM units WHERE property_id = ? ORDER BY label, id;

-- Occupancy is derived, never stored (DESIGN.md 3.2): a unit is occupied if it
-- has an active lease covering today. There is no is_occupied column to drift
-- out of sync with the leases that are the actual evidence, so the answer is
-- recomputed on every read from the dates themselves.
--
-- The join carries the lease id rather than a flag, so the screen can link to
-- the lease that is the reason for the answer. It is a LEFT JOIN because most
-- units on most days have no active lease, and that is not a missing row.
-- name: ListUnitsWithOccupancy :many
SELECT
    sqlc.embed(units),
    leases.id AS active_lease_id,
    leases.end_date AS active_lease_end_date
FROM units
LEFT JOIN leases ON leases.unit_id = units.id
    AND leases.status = 'active'
    AND leases.start_date <= sqlc.arg(today)
    AND (leases.end_date IS NULL OR leases.end_date >= sqlc.arg(today))
WHERE units.property_id = sqlc.arg(property_id)
ORDER BY units.label, units.id;

-- name: GetUnit :one
SELECT * FROM units WHERE id = ? LIMIT 1;

-- name: CreateUnit :one
INSERT INTO units (property_id, label, beds, baths, sqft, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: UpdateUnit :one
UPDATE units SET
    label      = ?,
    beds       = ?,
    baths      = ?,
    sqft       = ?,
    updated_at = ?
WHERE id = ?
RETURNING *;

-- name: DeleteUnit :execrows
DELETE FROM units WHERE id = ?;

-- Guards the invariant that every lease hangs off a unit (DESIGN.md 3.2):
-- a property can never be left with zero units.
-- name: CountUnitsByProperty :one
SELECT COUNT(*) FROM units WHERE property_id = ?;
