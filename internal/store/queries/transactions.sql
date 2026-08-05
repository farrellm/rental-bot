-- The ledger, newest entry first. The keyset runs backwards over
-- (occurred_on, id), because a day holds more than one entry and the date
-- alone would skip or repeat rows at a page boundary.
--
-- The three filters are optional in one query rather than eight: sqlc.narg
-- makes each a nullable parameter, and a NULL one drops out of the WHERE
-- clause. The totals query repeats the same predicate, so a filtered page and
-- its totals always describe the same set of rows.
--
-- The CAST around each narg is load-bearing, not noise. Without it sqlc's
-- SQLite inference gives up and types the parameter as interface{}, which
-- lets a call site pass anything at all; with it the parameter is *string and
-- a wrong type is a compile error. Do not remove them.

-- name: ListTransactionsFirstPage :many
SELECT * FROM transactions
WHERE property_id = sqlc.arg(property_id)
  AND (CAST(sqlc.narg(from_date) AS TEXT) IS NULL
       OR occurred_on >= CAST(sqlc.narg(from_date) AS TEXT))
  AND (CAST(sqlc.narg(to_date) AS TEXT) IS NULL
       OR occurred_on <= CAST(sqlc.narg(to_date) AS TEXT))
  AND (CAST(sqlc.narg(category) AS TEXT) IS NULL
       OR category = CAST(sqlc.narg(category) AS TEXT))
ORDER BY occurred_on DESC, id DESC
LIMIT sqlc.arg(page_size);

-- name: ListTransactionsAfter :many
SELECT * FROM transactions
WHERE property_id = sqlc.arg(property_id)
  AND (CAST(sqlc.narg(from_date) AS TEXT) IS NULL
       OR occurred_on >= CAST(sqlc.narg(from_date) AS TEXT))
  AND (CAST(sqlc.narg(to_date) AS TEXT) IS NULL
       OR occurred_on <= CAST(sqlc.narg(to_date) AS TEXT))
  AND (CAST(sqlc.narg(category) AS TEXT) IS NULL
       OR category = CAST(sqlc.narg(category) AS TEXT))
  AND (occurred_on < sqlc.arg(after_date)
       OR (occurred_on = sqlc.arg(after_date) AND id < sqlc.arg(after_id)))
ORDER BY occurred_on DESC, id DESC
LIMIT sqlc.arg(page_size);

-- The foot of the ledger sheet. Income and expense are the same signed column
-- split by sign, and the net is their sum, so the three can never disagree.
-- The CAST is also what makes sqlc type these as int64 rather than interface{}.
-- name: SumTransactions :one
SELECT
    CAST(COALESCE(SUM(CASE WHEN amount_cents > 0 THEN amount_cents ELSE 0 END), 0) AS INTEGER) AS income_cents,
    CAST(COALESCE(SUM(CASE WHEN amount_cents < 0 THEN amount_cents ELSE 0 END), 0) AS INTEGER) AS expense_cents,
    CAST(COALESCE(SUM(amount_cents), 0) AS INTEGER) AS net_cents,
    CAST(COUNT(*) AS INTEGER) AS entry_count
FROM transactions
WHERE property_id = sqlc.arg(property_id)
  AND (CAST(sqlc.narg(from_date) AS TEXT) IS NULL
       OR occurred_on >= CAST(sqlc.narg(from_date) AS TEXT))
  AND (CAST(sqlc.narg(to_date) AS TEXT) IS NULL
       OR occurred_on <= CAST(sqlc.narg(to_date) AS TEXT))
  AND (CAST(sqlc.narg(category) AS TEXT) IS NULL
       OR category = CAST(sqlc.narg(category) AS TEXT));

-- name: GetTransaction :one
SELECT * FROM transactions WHERE id = ? LIMIT 1;

-- name: CreateTransaction :one
INSERT INTO transactions (
    property_id, occurred_on, amount_cents, category, description,
    counterparty, payment_method, unit_id, lease_id, repair_id,
    vendor_id, document_id, source, confidence, needs_review,
    created_at, updated_at
) VALUES (
    ?, ?, ?, ?, ?,
    ?, ?, ?, ?, ?,
    ?, ?, ?, ?, ?,
    ?, ?
)
RETURNING *;

-- Every column is written, because PATCH is a read-modify-write in Go.
-- name: UpdateTransaction :one
UPDATE transactions SET
    occurred_on    = ?,
    amount_cents   = ?,
    category       = ?,
    description    = ?,
    counterparty   = ?,
    payment_method = ?,
    unit_id        = ?,
    lease_id       = ?,
    repair_id      = ?,
    vendor_id      = ?,
    document_id    = ?,
    needs_review   = ?,
    updated_at     = ?
WHERE id = ?
RETURNING *;

-- name: DeleteTransaction :execrows
DELETE FROM transactions WHERE id = ?;
