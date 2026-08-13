-- Mortgages, and the statements that arrive against them.
--
-- A statement is append-only, which is what makes the amortization history a
-- consequence of the write path rather than a feature to build. The UNIQUE on
-- (mortgage_id, statement_date) is the idempotency key an apply reads before
-- it writes: the same statement forwarded twice is one statement.
--
-- Comments here are ASCII only, like every other query file.

-- name: ListMortgagesByProperty :many
SELECT * FROM mortgages
WHERE property_id = ?
ORDER BY origination_date DESC, id DESC;

-- name: GetMortgage :one
SELECT * FROM mortgages WHERE id = ? LIMIT 1;

-- A property usually has one mortgage, and a statement names its lender rather
-- than an id. Matching on the lender is what turns the second into the first.
-- name: FindMortgageByLender :one
SELECT * FROM mortgages
WHERE property_id = sqlc.arg(property_id) AND lender = sqlc.arg(lender)
ORDER BY id
LIMIT 1;

-- name: CreateMortgage :one
INSERT INTO mortgages (
    property_id, lender, loan_number_enc, original_principal_cents,
    interest_rate_bps, term_months, origination_date, monthly_pi_cents,
    escrow_monthly_cents, current_balance_cents, balance_as_of, payoff_date,
    notes, created_at, updated_at
) VALUES (
    ?, ?, ?, ?,
    ?, ?, ?, ?,
    ?, ?, ?, ?,
    ?, ?, ?
)
RETURNING *;

-- Every column is written, because PATCH is a read-modify-write in Go.
-- name: UpdateMortgage :one
UPDATE mortgages SET
    lender                   = ?,
    loan_number_enc          = ?,
    original_principal_cents = ?,
    interest_rate_bps        = ?,
    term_months              = ?,
    origination_date         = ?,
    monthly_pi_cents         = ?,
    escrow_monthly_cents     = ?,
    current_balance_cents    = ?,
    balance_as_of            = ?,
    payoff_date              = ?,
    notes                    = ?,
    updated_at               = ?
WHERE id = ?
RETURNING *;

-- The running balance, taken off the statement that carried it. It is a
-- separate write from the statement insert so that a statement out of order
-- cannot walk the balance backwards.
-- name: SetMortgageBalance :exec
UPDATE mortgages SET
    current_balance_cents = sqlc.narg(current_balance_cents),
    balance_as_of         = sqlc.arg(balance_as_of),
    updated_at            = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id)
  AND (balance_as_of IS NULL OR balance_as_of <= sqlc.arg(balance_as_of));

-- name: DeleteMortgage :execrows
DELETE FROM mortgages WHERE id = ?;

-- Statements ----------------------------------------------------------------

-- name: ListMortgageStatements :many
SELECT * FROM mortgage_statements
WHERE mortgage_id = ?
ORDER BY statement_date DESC, id DESC;

-- name: GetMortgageStatement :one
SELECT * FROM mortgage_statements WHERE id = ? LIMIT 1;

-- name: CreateMortgageStatement :one
INSERT INTO mortgage_statements (
    mortgage_id, statement_date, principal_balance_cents, payment_cents,
    principal_paid_cents, interest_paid_cents, escrow_paid_cents,
    document_id, created_at, updated_at
) VALUES (
    ?, ?, ?, ?,
    ?, ?, ?,
    ?, ?, ?
)
RETURNING *;
