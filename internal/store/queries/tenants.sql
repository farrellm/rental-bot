-- name: ListTenants :many
SELECT * FROM tenants ORDER BY name, id;

-- name: GetTenant :one
SELECT * FROM tenants WHERE id = ? LIMIT 1;

-- An extracted lease names its tenants and nothing else. Matching on the name
-- is what keeps one person from becoming three rows when three leases are
-- forwarded; it is a weak key, and the apply path never merges on it silently
-- -- an operator sees the match before the lease is filed.
-- name: FindTenantByName :one
SELECT * FROM tenants WHERE name = ? ORDER BY id LIMIT 1;

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
