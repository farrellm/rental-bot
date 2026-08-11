package httpapi

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// Keyset pagination, shared by every index in this API.
//
// Nothing pages yet -- each screen fetches one page and ignores next_cursor --
// but the endpoints issue cursors from the start, so the day a ledger gets long
// enough to need paging is a frontend change rather than an API change.

const (
	defaultPageSize = 50
	maxPageSize     = 200
)

// pageSize reads the limit query parameter.
func pageSize(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return defaultPageSize, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		WriteProblem(w, r, http.StatusBadRequest, "limit has to be a positive whole number.")
		return 0, false
	}
	return min(n, maxPageSize), true
}

var errBadCursor = errors.New("httpapi: malformed cursor")

// encodeCursor names the last row of a page by its sort key.
//
// Every keyset in this API sorts on a text column and breaks the tie on id, so
// the cursor carries both: properties sort by nickname and documents by
// created_at, and neither is unique. A cursor that carried only the first would
// skip or repeat rows wherever two rows share it.
func encodeCursor(sortKey string, id int64) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(sortKey + "\x00" + strconv.FormatInt(id, 10)))
}

func decodeCursor(cursor string) (string, int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", 0, errBadCursor
	}
	sortKey, rest, found := strings.Cut(string(raw), "\x00")
	if !found {
		return "", 0, errBadCursor
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return "", 0, errBadCursor
	}
	return sortKey, id, nil
}
