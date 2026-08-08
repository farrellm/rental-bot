-- What arrived, and what came attached to it.
--
-- gmail_message_id is the idempotency key for the whole pipeline. The read
-- before the insert is the same shape the document upload uses against sha256:
-- a message already on file is the same message, not a second copy of it.
--
-- Comments here are ASCII only, like every other query file.

-- name: GetEmailMessage :one
SELECT * FROM email_messages WHERE id = ? LIMIT 1;

-- name: GetEmailMessageByGmailID :one
SELECT * FROM email_messages WHERE gmail_message_id = ? LIMIT 1;

-- name: CreateEmailMessage :one
INSERT INTO email_messages (
    gmail_message_id, thread_id, from_addr, to_addr, subject, received_at,
    snippet, raw_path, status, error, created_at, updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?,
    ?, ?, ?, ?, ?, ?
)
RETURNING *;

-- name: SetEmailMessageStatus :exec
UPDATE email_messages SET
    status     = sqlc.arg(status),
    error      = sqlc.arg(error),
    updated_at = sqlc.arg(updated_at)
WHERE id = sqlc.arg(id);

-- The register, newest first. The keyset runs backwards over
-- (received_at, id): two messages can share a second, and id breaks the tie.
-- name: ListEmailMessagesFirstPage :many
SELECT * FROM email_messages
ORDER BY received_at DESC, id DESC
LIMIT sqlc.arg(page_size);

-- name: ListEmailMessagesAfter :many
SELECT * FROM email_messages
WHERE (received_at < sqlc.arg(after_received_at)
       OR (received_at = sqlc.arg(after_received_at) AND id < sqlc.arg(after_id)))
ORDER BY received_at DESC, id DESC
LIMIT sqlc.arg(page_size);

-- name: CountEmailMessagesByStatus :many
SELECT status, COUNT(*) AS count FROM email_messages GROUP BY status;

-- Attachments --------------------------------------------------------------

-- name: ListEmailAttachments :many
SELECT * FROM email_attachments
WHERE email_message_id = ?
ORDER BY id;

-- name: CreateEmailAttachment :one
INSERT INTO email_attachments (
    email_message_id, part_id, filename, mime, size_bytes, document_id,
    skipped_reason, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;
