-- Documents are listed newest first, so the keyset runs backwards over
-- (created_at, id): created_at is not unique when two files land in the same
-- second, and id breaks the tie.

-- name: ListDocumentsByPropertyFirstPage :many
SELECT * FROM documents
WHERE property_id = sqlc.arg(property_id)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_size);

-- name: ListDocumentsByPropertyAfter :many
SELECT * FROM documents
WHERE property_id = sqlc.arg(property_id)
  AND (created_at < sqlc.arg(after_created_at)
       OR (created_at = sqlc.arg(after_created_at) AND id < sqlc.arg(after_id)))
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_size);

-- name: GetDocument :one
SELECT * FROM documents WHERE id = ? LIMIT 1;

-- Content addressing means the hash is the identity: an upload that hashes to
-- a row already on file is the same document, not a second copy.
-- name: GetDocumentBySHA :one
SELECT * FROM documents WHERE sha256 = ? LIMIT 1;

-- source_message_id is null for a document the operator uploaded and set for
-- one that arrived attached to an email. It is provenance, not a link: what the
-- document evidences is document_links' business.
-- name: CreateDocument :one
INSERT INTO documents (
    property_id, kind, title, original_filename, mime, size_bytes,
    sha256, storage_path, extracted_text, uploaded_by, source_message_id,
    created_at, updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?,
    ?, ?, ?, ?, ?,
    ?, ?
)
RETURNING *;

-- Every column is written, because PATCH is a read-modify-write in Go.
-- name: UpdateDocument :one
UPDATE documents SET
    property_id       = ?,
    kind              = ?,
    title             = ?,
    original_filename = ?,
    extracted_text    = ?,
    updated_at        = ?
WHERE id = ?
RETURNING *;

-- name: DeleteDocument :execrows
DELETE FROM documents WHERE id = ?;

-- Links ------------------------------------------------------------------

-- name: ListDocumentLinks :many
SELECT * FROM document_links WHERE document_id = ? ORDER BY entity_type, entity_id;

-- Everything filed against one entity: the receipts on a repair, the lease PDF
-- on a lease.
-- name: ListDocumentsByEntity :many
SELECT documents.*
FROM documents
JOIN document_links ON document_links.document_id = documents.id
WHERE document_links.entity_type = sqlc.arg(entity_type)
  AND document_links.entity_id = sqlc.arg(entity_id)
ORDER BY documents.created_at DESC, documents.id DESC;

-- name: CreateDocumentLink :one
INSERT INTO document_links (document_id, entity_type, entity_id, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
RETURNING *;

-- name: DeleteDocumentLink :execrows
DELETE FROM document_links
WHERE document_id = sqlc.arg(document_id)
  AND entity_type = sqlc.arg(entity_type)
  AND entity_id = sqlc.arg(entity_id);
