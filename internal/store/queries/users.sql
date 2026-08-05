-- Comments in this directory are ASCII only. sqlc's SQLite rewriter counts
-- comment offsets in runes but slices them in bytes, so a single multi-byte
-- character silently corrupts the query that follows it: one em dash turns
-- "RETURNING *" into "RETURNIN". See sqlc.yaml.

-- name: GetUserByUsername :one
SELECT * FROM users WHERE username = ? LIMIT 1;

-- name: GetUser :one
SELECT * FROM users WHERE id = ? LIMIT 1;

-- Used by `rental-bot -create-user`, which is the only way a user is ever
-- created; there is no registration endpoint. Re-running it for an existing
-- username resets that user's password rather than failing, so an operator
-- who has locked themselves out has a way back in.
-- name: UpsertUser :one
INSERT INTO users (username, email, password_hash, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT (username) DO UPDATE SET
    password_hash = excluded.password_hash,
    updated_at    = excluded.updated_at
RETURNING *;

-- name: CountUsers :one
SELECT COUNT(*) FROM users;
