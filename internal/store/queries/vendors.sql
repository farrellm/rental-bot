-- name: ListVendors :many
SELECT * FROM vendors ORDER BY name, id;

-- name: GetVendor :one
SELECT * FROM vendors WHERE id = ? LIMIT 1;

-- name: CreateVendor :one
INSERT INTO vendors (name, trade, phone, email, notes, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: UpdateVendor :one
UPDATE vendors SET
    name       = ?,
    trade      = ?,
    phone      = ?,
    email      = ?,
    notes      = ?,
    updated_at = ?
WHERE id = ?
RETURNING *;

-- name: DeleteVendor :execrows
DELETE FROM vendors WHERE id = ?;
