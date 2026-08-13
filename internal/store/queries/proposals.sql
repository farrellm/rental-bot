-- The proposal gate: what a model claimed, before anybody agreed to it.
--
-- A row is written at classify time and enriched at extract time, so the two
-- stages are an insert and an update rather than two tables. Nothing here reads
-- into payload; it is opaque JSON whose shape the Go struct fixes.
--
-- Comments here are ASCII only, like every other query file.

-- name: GetProposal :one
SELECT * FROM ingest_proposals WHERE id = ? LIMIT 1;

-- The pipeline reads this before it enqueues, so re-running a classify for a
-- message that already has a proposal writes nothing. That read is what makes
-- the sweep free to walk the same messages the direct enqueue already covered.
-- name: GetProposalByMessage :one
SELECT * FROM ingest_proposals
WHERE email_message_id = ?
ORDER BY id DESC
LIMIT 1;

-- name: CreateProposal :one
INSERT INTO ingest_proposals (
    email_message_id, kind, payload, llm_model, prompt_tokens, completion_tokens,
    confidence, property_hint, property_id, reasoning, status,
    created_at, updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?,
    ?, ?, ?, ?, ?,
    ?, ?
)
RETURNING *;

-- What the extract stage learned. The token counts are added to rather than
-- replaced, because classify already spent some and the sum is what the budget
-- breaker reads.
-- name: RecordProposalExtract :one
UPDATE ingest_proposals SET
    payload           = sqlc.arg(payload),
    llm_model         = sqlc.arg(llm_model),
    prompt_tokens     = prompt_tokens + sqlc.arg(prompt_tokens),
    completion_tokens = completion_tokens + sqlc.arg(completion_tokens),
    confidence        = sqlc.arg(confidence),
    property_id       = sqlc.narg(property_id),
    error             = sqlc.arg(error),
    updated_at        = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id)
RETURNING *;

-- Correcting a proposal before approving it. Every column the operator can
-- touch is written, because PATCH is a read-modify-write in Go.
-- name: UpdateProposal :one
UPDATE ingest_proposals SET
    kind        = sqlc.arg(kind),
    payload     = sqlc.arg(payload),
    property_id = sqlc.narg(property_id),
    updated_at  = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id)
RETURNING *;

-- The transition, guarded in the statement rather than in Go.
--
-- A proposal leaves 'pending' exactly once. Two approvals arriving together
-- would both pass a read-then-write, and the second would file the receipt a
-- second time; the WHERE clause is what makes the loser see zero rows. This is
-- the discipline PairTelegram follows, for the same reason.
-- name: SettleProposal :one
UPDATE ingest_proposals SET
    status              = sqlc.arg(status),
    reviewed_by         = sqlc.narg(reviewed_by),
    reviewed_at         = sqlc.arg(reviewed_at),
    applied_entity_type = sqlc.narg(applied_entity_type),
    applied_entity_id   = sqlc.narg(applied_entity_id),
    error               = sqlc.arg(error),
    updated_at          = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id) AND status = 'pending'
RETURNING *;

-- An apply that could not go through. The proposal stays pending -- the
-- operator has something to fix and try again -- and the reason rides on the
-- row so the screen can say what rather than only that.
-- name: SetProposalError :exec
UPDATE ingest_proposals SET
    error      = sqlc.arg(error),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id);

-- The review queue, newest first. The keyset runs backwards over
-- (created_at, id): two proposals can share a second, and id breaks the tie.
-- A NULL status filter drops out of the WHERE clause, and the CAST around it
-- is load-bearing -- without it sqlc's inference types the parameter
-- interface{} rather than *string.
-- name: ListProposalsFirstPage :many
SELECT * FROM ingest_proposals
WHERE (CAST(sqlc.narg(status) AS TEXT) IS NULL
       OR status = CAST(sqlc.narg(status) AS TEXT))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_size);

-- name: ListProposalsAfter :many
SELECT * FROM ingest_proposals
WHERE (CAST(sqlc.narg(status) AS TEXT) IS NULL
       OR status = CAST(sqlc.narg(status) AS TEXT))
  AND (created_at < sqlc.arg(after_created_at)
       OR (created_at = sqlc.arg(after_created_at) AND id < sqlc.arg(after_id)))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_size);

-- name: CountProposalsByStatus :many
SELECT status, COUNT(*) AS count FROM ingest_proposals GROUP BY status;

-- Every message that arrived, was allowed through, and has no proposal against
-- it. This is what the sweep walks: the direct enqueue at sync time makes
-- ingestion fast, and this makes it reliable.
-- name: ListMessagesAwaitingProposal :many
SELECT email_messages.* FROM email_messages
LEFT JOIN ingest_proposals ON ingest_proposals.email_message_id = email_messages.id
WHERE email_messages.status = 'received'
  AND ingest_proposals.id IS NULL
ORDER BY email_messages.received_at, email_messages.id
LIMIT sqlc.arg(page_size);

-- The budget breaker's ledger. Every call the pipeline makes lands on a
-- proposal row, so one sum over a month is the whole spend. The CAST is what
-- makes sqlc type this int64 rather than interface{}.
-- name: SumProposalTokensSince :one
SELECT CAST(COALESCE(SUM(prompt_tokens + completion_tokens), 0) AS INTEGER)
FROM ingest_proposals
WHERE created_at >= sqlc.arg(since);
