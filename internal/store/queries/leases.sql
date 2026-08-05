-- Every lease hangs off a unit, so a property's leases are reached through
-- its units. The unit label rides along, because a lease reads as "Apt 2,
-- 2026-01-01 to 2026-12-31" and a second query per row to learn the label
-- would be a page of round trips.

-- name: ListLeasesByProperty :many
SELECT sqlc.embed(leases), units.label AS unit_label
FROM leases
JOIN units ON units.id = leases.unit_id
WHERE units.property_id = sqlc.arg(property_id)
ORDER BY leases.start_date DESC, leases.id DESC;

-- name: ListLeasesByUnit :many
SELECT * FROM leases WHERE unit_id = ? ORDER BY start_date DESC, id DESC;

-- name: GetLease :one
SELECT * FROM leases WHERE id = ? LIMIT 1;

-- name: GetLeaseWithUnit :one
SELECT sqlc.embed(leases), units.label AS unit_label, units.property_id AS property_id
FROM leases
JOIN units ON units.id = leases.unit_id
WHERE leases.id = ?
LIMIT 1;

-- Two active leases on one unit make occupancy ambiguous, and occupancy is
-- derived rather than stored, so the write path is the only place that can
-- keep it unambiguous. A null end_date is an open-ended tenancy and overlaps
-- everything after its start.
-- name: CountOverlappingLeases :one
SELECT CAST(COUNT(*) AS INTEGER) FROM leases
WHERE unit_id = sqlc.arg(unit_id)
  AND status IN ('pending', 'active')
  AND id <> sqlc.arg(exclude_id)
  AND (CAST(sqlc.narg(end_date) AS TEXT) IS NULL
       OR start_date <= CAST(sqlc.narg(end_date) AS TEXT))
  AND (end_date IS NULL OR end_date >= sqlc.arg(start_date));

-- name: CreateLease :one
INSERT INTO leases (
    unit_id, start_date, end_date, rent_cents, deposit_cents, due_day,
    late_fee_cents, status, renewal_of_lease_id, document_id, notes,
    created_at, updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?,
    ?, ?, ?, ?, ?,
    ?, ?
)
RETURNING *;

-- Every column is written, because PATCH is a read-modify-write in Go.
-- name: UpdateLease :one
UPDATE leases SET
    unit_id             = ?,
    start_date          = ?,
    end_date            = ?,
    rent_cents          = ?,
    deposit_cents       = ?,
    due_day             = ?,
    late_fee_cents      = ?,
    status              = ?,
    renewal_of_lease_id = ?,
    document_id         = ?,
    notes               = ?,
    updated_at          = ?
WHERE id = ?
RETURNING *;

-- name: DeleteLease :execrows
DELETE FROM leases WHERE id = ?;

-- Who is on a lease ------------------------------------------------------

-- name: ListLeaseTenants :many
SELECT sqlc.embed(tenants), lease_tenants.role AS role
FROM lease_tenants
JOIN tenants ON tenants.id = lease_tenants.tenant_id
WHERE lease_tenants.lease_id = ?
ORDER BY lease_tenants.role, tenants.name;

-- name: AddLeaseTenant :one
INSERT INTO lease_tenants (lease_id, tenant_id, role, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
RETURNING *;

-- name: RemoveLeaseTenant :execrows
DELETE FROM lease_tenants
WHERE lease_id = sqlc.arg(lease_id) AND tenant_id = sqlc.arg(tenant_id);
