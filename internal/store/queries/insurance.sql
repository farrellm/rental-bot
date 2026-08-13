-- Insurance policies, one property at a time.
--
-- The policy number is stored encrypted and is decrypted at the one call site
-- that has the key. Nothing here ever selects it for a list.
--
-- Comments here are ASCII only, like every other query file.

-- name: ListInsurancePoliciesByProperty :many
SELECT * FROM insurance_policies
WHERE property_id = ?
ORDER BY expiration_date DESC, id DESC;

-- name: GetInsurancePolicy :one
SELECT * FROM insurance_policies WHERE id = ? LIMIT 1;

-- A carrier and a policy number identify a policy; the same declaration page
-- forwarded twice is the same policy. The apply path reads this before it
-- writes, the way the ingestion pipeline reads gmail_message_id.
-- name: FindInsurancePolicy :one
SELECT * FROM insurance_policies
WHERE property_id = sqlc.arg(property_id)
  AND carrier = sqlc.arg(carrier)
  AND policy_number_enc = sqlc.arg(policy_number_enc)
LIMIT 1;

-- name: CreateInsurancePolicy :one
INSERT INTO insurance_policies (
    property_id, carrier, policy_number_enc, type, agent_name, agent_phone,
    agent_email, effective_date, expiration_date, annual_premium_cents,
    dwelling_coverage_cents, liability_coverage_cents, deductible_cents,
    document_id, notes, created_at, updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?,
    ?, ?, ?, ?,
    ?, ?, ?,
    ?, ?, ?, ?
)
RETURNING *;

-- Every column is written, because PATCH is a read-modify-write in Go.
-- name: UpdateInsurancePolicy :one
UPDATE insurance_policies SET
    carrier                  = ?,
    policy_number_enc        = ?,
    type                     = ?,
    agent_name               = ?,
    agent_phone              = ?,
    agent_email              = ?,
    effective_date           = ?,
    expiration_date          = ?,
    annual_premium_cents     = ?,
    dwelling_coverage_cents  = ?,
    liability_coverage_cents = ?,
    deductible_cents         = ?,
    document_id              = ?,
    notes                    = ?,
    updated_at               = ?
WHERE id = ?
RETURNING *;

-- name: DeleteInsurancePolicy :execrows
DELETE FROM insurance_policies WHERE id = ?;
