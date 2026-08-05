-- name: ListUnitsByProperty :many
SELECT * FROM units WHERE property_id = ? ORDER BY label, id;

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
