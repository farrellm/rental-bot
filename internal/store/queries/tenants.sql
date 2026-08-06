-- name: ListTenants :many
SELECT * FROM tenants ORDER BY name, id;

-- name: GetTenant :one
SELECT * FROM tenants WHERE id = ? LIMIT 1;

-- name: CreateTenant :one
INSERT INTO tenants (name, email, phone, notes, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: UpdateTenant :one
UPDATE tenants SET
    name       = ?,
    email      = ?,
    phone      = ?,
    notes      = ?,
    updated_at = ?
WHERE id = ?
RETURNING *;

-- name: DeleteTenant :execrows
DELETE FROM tenants WHERE id = ?;
