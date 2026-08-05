-- name: CreateSession :one
INSERT INTO sessions (
    user_id, token_hash, expires_at, user_agent, ip, last_seen_at,
    created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- The session and its user in one read: every authenticated request needs
-- both, and this lookup is on the hot path for all of them.
-- name: GetSessionByTokenHash :one
SELECT sqlc.embed(sessions), sqlc.embed(users)
FROM sessions
JOIN users ON users.id = sessions.user_id
WHERE sessions.token_hash = ?
LIMIT 1;

-- name: TouchSession :exec
UPDATE sessions
SET last_seen_at = ?, expires_at = ?, updated_at = ?
WHERE id = ?;

-- name: DeleteSessionByTokenHash :execrows
DELETE FROM sessions WHERE token_hash = ?;

-- name: DeleteSessionsForUser :execrows
DELETE FROM sessions WHERE user_id = ?;

-- Sessions outlive their usefulness silently, so something has to sweep them.
-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions WHERE expires_at < ?;
