-- Listing carries the unit count as a correlated subquery rather than a
-- second round trip per row: the index screen shows it on every card.
--
-- Pagination is a keyset over (nickname, id), which is also the sort order.
-- nickname is not unique, so id breaks the tie and keeps the cursor stable
-- when two properties share a name.

-- name: ListPropertiesFirstPage :many
SELECT
    sqlc.embed(properties),
    (SELECT COUNT(*) FROM units WHERE units.property_id = properties.id) AS unit_count
FROM properties
ORDER BY properties.nickname, properties.id
LIMIT ?;

-- name: ListPropertiesAfter :many
SELECT
    sqlc.embed(properties),
    (SELECT COUNT(*) FROM units WHERE units.property_id = properties.id) AS unit_count
FROM properties
WHERE properties.nickname > sqlc.arg(after_nickname)
   OR (properties.nickname = sqlc.arg(after_nickname) AND properties.id > sqlc.arg(after_id))
ORDER BY properties.nickname, properties.id
LIMIT sqlc.arg(page_size);

-- name: GetProperty :one
SELECT * FROM properties WHERE id = ? LIMIT 1;

-- Everything the property matcher needs and nothing else.
--
-- The comparison is deterministic Go over the folded address (docs/DESIGN.md
-- section 5.3), not SQL, because "close enough" is an edit distance and SQLite
-- cannot spell one. The portfolio is tens of rows, so loading all of them and
-- comparing in memory is cheaper than the index that would let the database
-- try.
-- name: ListPropertyMatchKeys :many
SELECT id, nickname, normalized_address FROM properties
ORDER BY id;

-- name: CreateProperty :one
INSERT INTO properties (
    nickname, address_line1, address_line2, city, state, postal_code, county,
    normalized_address, purchase_date, purchase_price_cents, beds, baths,
    sqft, year_built, status, zpid, notes, created_at, updated_at
) VALUES (
    ?, ?, ?, ?, ?, ?, ?,
    ?, ?, ?, ?, ?,
    ?, ?, ?, ?, ?, ?, ?
)
RETURNING *;

-- Every column is written, because PATCH is applied as a read-modify-write in
-- Go rather than as a pile of COALESCE clauses. A nullable column has three
-- states on the wire (absent, null, and set) and COALESCE cannot express the
-- difference between the first two. Read-modify-write is safe here because the
-- writer pool is a single connection by design.
-- name: UpdateProperty :one
UPDATE properties SET
    nickname             = ?,
    address_line1        = ?,
    address_line2        = ?,
    city                 = ?,
    state                = ?,
    postal_code          = ?,
    county               = ?,
    normalized_address   = ?,
    purchase_date        = ?,
    purchase_price_cents = ?,
    beds                 = ?,
    baths                = ?,
    sqft                 = ?,
    year_built           = ?,
    status               = ?,
    zpid                 = ?,
    notes                = ?,
    updated_at           = ?
WHERE id = ?
RETURNING *;

-- name: DeleteProperty :execrows
DELETE FROM properties WHERE id = ?;
