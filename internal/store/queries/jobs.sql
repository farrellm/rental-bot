-- The job queue. A bounded worker pool drains it; see internal/jobs.
--
-- Comments here are ASCII only, like every other query file: sqlc's SQLite
-- rewriter counts comment offsets in runes and slices them in bytes.

-- name: EnqueueJob :one
INSERT INTO jobs (
    kind, payload, dedupe_key, run_after, max_attempts, status,
    created_at, updated_at
) VALUES (?, ?, ?, ?, ?, 'pending', ?, ?)
RETURNING *;

-- Claim exactly one runnable job and mark it running in the same statement.
--
-- The inner SELECT picks the row and the UPDATE takes it, so two workers cannot
-- both come away with the same job. Writes serialize on one connection anyway
-- (docs/DESIGN.md section 2); this does not depend on that being true.
--
-- attempts increments on claim rather than on failure, so a worker that is
-- killed mid-job has still spent one of its tries. Otherwise a job that panics
-- the process retries forever.
--
-- The casts around the two arguments are load-bearing, the same way the ones in
-- transactions.sql are: locked_at and locked_by are nullable columns, so
-- without them sqlc infers both parameters nullable and hands the caller a
-- *string for a value it always has.
-- name: ClaimJob :one
UPDATE jobs SET
    status     = 'running',
    attempts   = attempts + 1,
    locked_at  = CAST(sqlc.arg(now) AS TEXT),
    locked_by  = CAST(sqlc.arg(worker) AS TEXT),
    updated_at = sqlc.arg(now)
WHERE id = (
    SELECT id FROM jobs
    WHERE status = 'pending' AND run_after <= sqlc.arg(now)
    ORDER BY run_after, id
    LIMIT 1
)
RETURNING *;

-- name: CompleteJob :exec
UPDATE jobs SET
    status     = 'done',
    locked_at  = NULL,
    locked_by  = NULL,
    last_error = '',
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id);

-- Back to pending with a later run_after: the caller computes the backoff.
-- name: RetryJob :exec
UPDATE jobs SET
    status     = 'pending',
    run_after  = sqlc.arg(run_after),
    locked_at  = NULL,
    locked_by  = NULL,
    last_error = sqlc.arg(last_error),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id);

-- Out of attempts. The row stays as the record of what went wrong.
-- name: FailJob :exec
UPDATE jobs SET
    status     = 'failed',
    locked_at  = NULL,
    locked_by  = NULL,
    last_error = sqlc.arg(last_error),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id);

-- A job still locked past the lease belongs to a worker that is gone -- the
-- process was killed, or the host rebooted mid-run. Returning it to pending is
-- what stops that work being stranded until someone notices.
-- name: ReclaimStaleJobs :execrows
UPDATE jobs SET
    status     = 'pending',
    locked_at  = NULL,
    locked_by  = NULL,
    last_error = 'reclaimed after the worker holding it stopped reporting',
    updated_at = sqlc.arg(updated_at)
-- Cast for the same reason ClaimJob casts: locked_at is nullable, and without
-- it the cutoff arrives as a *string.
WHERE status = 'running' AND locked_at < CAST(sqlc.arg(older_than) AS TEXT);

-- name: GetJob :one
SELECT * FROM jobs WHERE id = ? LIMIT 1;

-- Queue depth, for the status endpoint and the intake screen.
-- name: CountJobsByStatus :many
SELECT status, COUNT(*) AS count FROM jobs GROUP BY status;

-- name: ListRecentFailedJobs :many
SELECT * FROM jobs
WHERE status = 'failed'
ORDER BY updated_at DESC, id DESC
LIMIT sqlc.arg(page_size);

-- Finished work is not history worth keeping forever; a failed job is.
-- name: DeleteFinishedJobsBefore :execrows
DELETE FROM jobs WHERE status = 'done' AND updated_at < sqlc.arg(before);
