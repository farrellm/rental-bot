package httpapi

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/farrellm/rental-bot/internal/auth"
	"github.com/farrellm/rental-bot/internal/blob"
	"github.com/farrellm/rental-bot/internal/domain"
	"github.com/farrellm/rental-bot/internal/store"
	"github.com/farrellm/rental-bot/internal/store/sqlc"
)

// documentKinds mirrors the CHECK constraint in migration 0002.
var documentKinds = []string{
	"lease", "insurance", "receipt", "statement",
	"tax", "photo", "correspondence", "other",
}

// linkEntityTypes mirrors the entity_type CHECK in migration 0002. Widening it
// is a migration, so the two lists have to move together.
var linkEntityTypes = []string{
	"property", "unit", "transaction", "repair",
	"repair_event", "lease", "tenant", "vendor",
}

// inlineTypes are the only content types served with an inline disposition.
//
// This is a security boundary, not a convenience. A document is served from
// the application's own origin, so an uploaded HTML file or SVG rendered
// inline would run script with the operator's session — the classic stored
// XSS through a file upload. Everything outside this list downloads instead,
// which is inert. The CSP and nosniff headers below are the second and third
// layers; this is the first.
var inlineTypes = map[string]bool{
	"application/pdf": true,
	"image/png":       true,
	"image/jpeg":      true,
	"image/gif":       true,
	"image/webp":      true,
	"text/plain":      true,
}

type documentResponse struct {
	ID               int64  `json:"id"`
	PropertyID       *int64 `json:"property_id"`
	Kind             string `json:"kind"`
	Title            string `json:"title"`
	OriginalFilename string `json:"original_filename"`
	Mime             string `json:"mime"`
	SizeBytes        int64  `json:"size_bytes"`
	SHA256           string `json:"sha256"`
	UploadedBy       *int64 `json:"uploaded_by"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
	// Links is present on the single-document response and absent from a list.
	Links []documentLink `json:"links,omitempty"`
}

type documentLink struct {
	EntityType string `json:"entity_type"`
	EntityID   int64  `json:"entity_id"`
}

type documentList struct {
	Items      []documentResponse `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

// uploadResponse is a document plus whether these bytes were already on file.
//
// The operator forwarded the same receipt twice and deserves to be told so,
// rather than being shown a document that looks new and is not.
type uploadResponse struct {
	documentResponse
	Deduplicated bool `json:"deduplicated"`
}

func newDocumentResponse(d sqlc.Document) documentResponse {
	return documentResponse{
		ID: d.ID, PropertyID: d.PropertyID, Kind: d.Kind, Title: d.Title,
		OriginalFilename: d.OriginalFilename, Mime: d.Mime, SizeBytes: d.SizeBytes,
		SHA256: d.Sha256, UploadedBy: d.UploadedBy,
		CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
	}
}

func (s *server) routeDocuments(mux *http.ServeMux) {
	route(mux, "/api/v1/documents", methods{
		http.MethodPost: s.guarded(s.handleUploadDocument),
	})
	route(mux, "/api/v1/documents/{id}", methods{
		http.MethodGet:    s.guarded(s.handleGetDocument),
		http.MethodPatch:  s.guarded(s.handleUpdateDocument),
		http.MethodDelete: s.guarded(s.handleDeleteDocument),
	})
	route(mux, "/api/v1/documents/{id}/content", methods{
		http.MethodGet: s.guarded(s.handleDocumentContent),
	})
	route(mux, "/api/v1/documents/{id}/links", methods{
		http.MethodPost:   s.guarded(s.handleLinkDocument),
		http.MethodDelete: s.guarded(s.handleUnlinkDocument),
	})
	route(mux, "/api/v1/properties/{id}/documents", methods{
		http.MethodGet: s.guarded(s.handleListDocuments),
	})
}

// handleUploadDocument takes a multipart form: the file, plus what it is and
// what it evidences.
//
// The body is capped before anything is read, so an oversized upload costs one
// rejected request rather than a disk full of a file nobody wanted.
func (s *server) handleUploadDocument(w http.ResponseWriter, r *http.Request) {
	if s.blobs == nil {
		WriteProblem(w, r, http.StatusServiceUnavailable, "The document store is not configured.")
		return
	}
	ctx := r.Context()

	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Storage.MaxUploadBytes)
	// The multipart reader streams; only the small text fields are buffered.
	form, err := r.MultipartReader()
	if err != nil {
		WriteProblem(w, r, http.StatusBadRequest,
			"Send the document as a multipart form with a file part.")
		return
	}

	var (
		fields       = map[string]string{}
		ref          blob.Ref
		gotFile      bool
		filename     string
		declaredType string
	)

	for {
		part, err := form.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.uploadReadError(w, r, err)
			return
		}

		if part.FormName() != "file" {
			// A text field. Bounded so a form cannot become a memory attack.
			value, err := io.ReadAll(io.LimitReader(part, 4<<10))
			part.Close()
			if err != nil {
				s.uploadReadError(w, r, err)
				return
			}
			fields[part.FormName()] = string(value)
			continue
		}

		if gotFile {
			part.Close()
			WriteProblem(w, r, http.StatusBadRequest, "Send one file per request.")
			return
		}
		filename = filepath.Base(part.FileName())
		declaredType = part.Header.Get("Content-Type")

		ref, err = s.blobs.Put(ctx, part)
		part.Close()
		if err != nil {
			s.uploadReadError(w, r, err)
			return
		}
		gotFile = true
	}

	if !gotFile {
		WriteProblem(w, r, http.StatusBadRequest, "The form has no file part.")
		return
	}

	kind := strings.TrimSpace(fields["kind"])
	if kind == "" {
		kind = "other"
	}
	if !slices.Contains(documentKinds, kind) {
		WriteProblem(w, r, http.StatusUnprocessableEntity,
			"Kind has to be one of "+strings.Join(documentKinds, ", ")+".")
		return
	}

	propertyID, ok := optionalID(w, r, fields["property_id"], "property_id")
	if !ok {
		return
	}
	if propertyID != nil {
		if _, err := s.repo.Read().GetProperty(ctx, *propertyID); err != nil {
			s.propertyReadError(w, r, err)
			return
		}
	}

	link, ok := parseLinkFields(w, r, fields)
	if !ok {
		return
	}

	title := strings.TrimSpace(fields["title"])
	if title == "" {
		title = filename
	}
	if len(title) > 200 {
		WriteProblem(w, r, http.StatusUnprocessableEntity, "The title is longer than 200 characters.")
		return
	}

	var (
		doc     sqlc.Document
		deduped bool
	)
	now := timestamp()
	uploader := userID(r)

	err = s.repo.Tx(ctx, func(q *sqlc.Queries) error {
		// The digest is the identity. Bytes already on file are the same
		// document, not a second copy of it, so the existing row is the answer
		// and the upload only adds whatever link came with it.
		existing, err := q.GetDocumentBySHA(ctx, ref.SHA256)
		switch {
		case err == nil:
			doc, deduped = existing, true
		case store.NotFound(err):
			doc, err = q.CreateDocument(ctx, sqlc.CreateDocumentParams{
				PropertyID:       propertyID,
				Kind:             kind,
				Title:            title,
				OriginalFilename: filename,
				Mime:             contentType(declaredType, filename),
				SizeBytes:        ref.Size,
				Sha256:           ref.SHA256,
				StoragePath:      ref.Path,
				UploadedBy:       uploader,
				CreatedAt:        now,
				UpdatedAt:        now,
			})
			if err != nil {
				return err
			}
		default:
			return err
		}

		if link == nil {
			return nil
		}
		_, err = q.CreateDocumentLink(ctx, sqlc.CreateDocumentLinkParams{
			DocumentID: doc.ID, EntityType: link.EntityType, EntityID: link.EntityID,
			CreatedAt: now, UpdatedAt: now,
		})
		// Filing the same document against the same thing twice is not an
		// error; the link already says what the caller wanted it to say.
		if store.Conflict(err) {
			return nil
		}
		return err
	})
	if err != nil {
		loggerFrom(ctx).Error("upload document", "error", err, "sha256", ref.SHA256)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not file the document.")
		return
	}

	status := http.StatusCreated
	if deduped {
		status = http.StatusOK
	}
	w.Header().Set("Location", "/api/v1/documents/"+strconv.FormatInt(doc.ID, 10))
	writeJSON(w, r, status, uploadResponse{
		documentResponse: newDocumentResponse(doc),
		Deduplicated:     deduped,
	})
}

// uploadReadError reports a failure to read the uploaded body.
func (s *server) uploadReadError(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		WriteProblem(w, r, http.StatusRequestEntityTooLarge,
			"That document is larger than the "+
				strconv.FormatInt(s.cfg.Storage.MaxUploadBytes>>20, 10)+" MB limit.")
		return
	}
	loggerFrom(r.Context()).Error("read upload", "error", err)
	WriteProblem(w, r, http.StatusBadRequest, "The upload could not be read.")
}

func (s *server) handleListDocuments(w http.ResponseWriter, r *http.Request) {
	propertyID, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	size, ok := pageSize(w, r)
	if !ok {
		return
	}
	ctx := r.Context()

	if _, err := s.repo.Read().GetProperty(ctx, propertyID); err != nil {
		s.propertyReadError(w, r, err)
		return
	}

	rows, err := s.documentPage(ctx, propertyID, r.URL.Query().Get("cursor"), size+1)
	if err != nil {
		if errors.Is(err, errBadCursor) {
			WriteProblem(w, r, http.StatusBadRequest, "The cursor is not one this endpoint issued.")
			return
		}
		loggerFrom(ctx).Error("list documents", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not read the documents.")
		return
	}

	out := documentList{Items: make([]documentResponse, 0, size)}
	for i, d := range rows {
		if i == size {
			last := rows[i-1]
			out.NextCursor = encodeCursor(last.CreatedAt, last.ID)
			break
		}
		out.Items = append(out.Items, newDocumentResponse(d))
	}
	writeJSON(w, r, http.StatusOK, out)
}

func (s *server) documentPage(ctx context.Context, propertyID int64, cursor string, limit int) ([]sqlc.Document, error) {
	if cursor == "" {
		return s.repo.Read().ListDocumentsByPropertyFirstPage(ctx,
			sqlc.ListDocumentsByPropertyFirstPageParams{PropertyID: &propertyID, PageSize: int64(limit)})
	}
	createdAt, id, err := decodeCursor(cursor)
	if err != nil {
		return nil, err
	}
	return s.repo.Read().ListDocumentsByPropertyAfter(ctx, sqlc.ListDocumentsByPropertyAfterParams{
		PropertyID:     &propertyID,
		AfterCreatedAt: createdAt,
		AfterID:        id,
		PageSize:       int64(limit),
	})
}

func (s *server) handleGetDocument(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ctx := r.Context()

	doc, err := s.repo.Read().GetDocument(ctx, id)
	if err != nil {
		s.documentReadError(w, r, err)
		return
	}
	links, err := s.repo.Read().ListDocumentLinks(ctx, id)
	if err != nil {
		loggerFrom(ctx).Error("list document links", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not read what this document is filed against.")
		return
	}

	out := newDocumentResponse(doc)
	out.Links = make([]documentLink, 0, len(links))
	for _, l := range links {
		out.Links = append(out.Links, documentLink{EntityType: l.EntityType, EntityID: l.EntityID})
	}
	writeJSON(w, r, http.StatusOK, out)
}

// handleDocumentContent serves the bytes, for a signed-in operator only.
//
// A document URL without a session is a 401, not a file (docs/DESIGN.md §9.2):
// the blob directory is never mapped by the reverse proxy, and this handler is
// the only way in.
func (s *server) handleDocumentContent(w http.ResponseWriter, r *http.Request) {
	if s.blobs == nil {
		WriteProblem(w, r, http.StatusServiceUnavailable, "The document store is not configured.")
		return
	}
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	ctx := r.Context()

	doc, err := s.repo.Read().GetDocument(ctx, id)
	if err != nil {
		s.documentReadError(w, r, err)
		return
	}

	f, err := s.blobs.Open(doc.Sha256)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			// The row says the file is there and it is not. That is a real
			// condition after a partial restore, and it is worth saying plainly.
			loggerFrom(ctx).Error("blob missing", "document_id", id, "sha256", doc.Sha256)
			WriteProblem(w, r, http.StatusNotFound,
				"This document's file is missing from the store.")
			return
		}
		loggerFrom(ctx).Error("open blob", "error", err, "document_id", id)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not read the document.")
		return
	}
	defer f.Close()

	// Three layers, in order of how much they are trusted. The disposition
	// decides whether a browser will ever render it; nosniff stops the browser
	// second-guessing the type we declared; the CSP means that even if
	// something renders, it can load nothing and run nothing.
	w.Header().Set("Content-Type", doc.Mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Disposition", disposition(doc))

	// ServeContent seeks to size the body and answers range requests, which is
	// how a browser scrubs a PDF without downloading all of it.
	http.ServeContent(w, r, doc.OriginalFilename, domain.ParseStamp(doc.UpdatedAt), f)
}

// disposition decides whether a document may render in the browser.
func disposition(doc sqlc.Document) string {
	how := "attachment"
	if inlineTypes[baseType(doc.Mime)] {
		how = "inline"
	}
	name := doc.OriginalFilename
	if name == "" {
		name = "document"
	}
	return mime.FormatMediaType(how, map[string]string{"filename": name})
}

var documentPatchFields = []string{"property_id", "kind", "title", "original_filename"}

func (s *server) handleUpdateDocument(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	p, ok := decodePatch(w, r, documentPatchFields...)
	if !ok {
		return
	}
	ctx := r.Context()

	var updated sqlc.Document
	err := s.repo.Tx(ctx, func(q *sqlc.Queries) error {
		current, err := q.GetDocument(ctx, id)
		if err != nil {
			return err
		}

		if err := p.nullable("property_id", &current.PropertyID); err != nil {
			return validationError{err.Error()}
		}
		for _, apply := range []func() error{
			func() error { return p.required("kind", &current.Kind) },
			func() error { return p.required("title", &current.Title) },
			func() error { return p.required("original_filename", &current.OriginalFilename) },
		} {
			if err := apply(); err != nil {
				return validationError{err.Error()}
			}
		}

		current.Title = strings.TrimSpace(current.Title)
		if !slices.Contains(documentKinds, current.Kind) {
			return validationError{"Kind has to be one of " + strings.Join(documentKinds, ", ") + "."}
		}
		if len(current.Title) > 200 {
			return validationError{"The title is longer than 200 characters."}
		}
		if current.PropertyID != nil {
			if _, err := q.GetProperty(ctx, *current.PropertyID); err != nil {
				if store.NotFound(err) {
					return validationError{"No property has that id."}
				}
				return err
			}
		}

		updated, err = q.UpdateDocument(ctx, sqlc.UpdateDocumentParams{
			PropertyID:       current.PropertyID,
			Kind:             current.Kind,
			Title:            current.Title,
			OriginalFilename: current.OriginalFilename,
			ExtractedText:    current.ExtractedText,
			UpdatedAt:        timestamp(),
			ID:               id,
		})
		return err
	})
	if err != nil {
		s.documentWriteError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, newDocumentResponse(updated))
}

// handleDeleteDocument removes the record and its links.
//
// The blob stays. A digest is the only name content has, several rows across
// later milestones can point at one, and a restore should still find the file
// where the backup put it. Reclaiming unreferenced blobs is a sweep that
// belongs with M7's backup work, not with a single delete.
func (s *server) handleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}

	rows, err := s.repo.Write().DeleteDocument(r.Context(), id)
	if err != nil {
		loggerFrom(r.Context()).Error("delete document", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not remove the document.")
		return
	}
	if rows == 0 {
		WriteProblem(w, r, http.StatusNotFound, "No such document.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type linkRequest struct {
	EntityType string `json:"entity_type"`
	EntityID   int64  `json:"entity_id"`
}

func (s *server) handleLinkDocument(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var req linkRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !slices.Contains(linkEntityTypes, req.EntityType) {
		WriteProblem(w, r, http.StatusUnprocessableEntity,
			"entity_type has to be one of "+strings.Join(linkEntityTypes, ", ")+".")
		return
	}
	if req.EntityID < 1 {
		WriteProblem(w, r, http.StatusUnprocessableEntity, "entity_id has to name a record.")
		return
	}
	ctx := r.Context()

	if _, err := s.repo.Read().GetDocument(ctx, id); err != nil {
		s.documentReadError(w, r, err)
		return
	}

	now := timestamp()
	_, err := s.repo.Write().CreateDocumentLink(ctx, sqlc.CreateDocumentLinkParams{
		DocumentID: id, EntityType: req.EntityType, EntityID: req.EntityID,
		CreatedAt: now, UpdatedAt: now,
	})
	// Already filed against that thing. The caller's intent is satisfied.
	if err != nil && !store.Conflict(err) {
		loggerFrom(ctx).Error("link document", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not file the document against that record.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleUnlinkDocument(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	var req linkRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	rows, err := s.repo.Write().DeleteDocumentLink(r.Context(), sqlc.DeleteDocumentLinkParams{
		DocumentID: id, EntityType: req.EntityType, EntityID: req.EntityID,
	})
	if err != nil {
		loggerFrom(r.Context()).Error("unlink document", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not unfile the document.")
		return
	}
	if rows == 0 {
		WriteProblem(w, r, http.StatusNotFound, "This document is not filed against that record.")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseLinkFields reads the optional link target off an upload form.
func parseLinkFields(w http.ResponseWriter, r *http.Request, fields map[string]string) (*linkRequest, bool) {
	entityType := strings.TrimSpace(fields["entity_type"])
	rawID := strings.TrimSpace(fields["entity_id"])
	if entityType == "" && rawID == "" {
		return nil, true
	}
	if entityType == "" || rawID == "" {
		WriteProblem(w, r, http.StatusUnprocessableEntity,
			"Filing a document against a record needs both entity_type and entity_id.")
		return nil, false
	}
	if !slices.Contains(linkEntityTypes, entityType) {
		WriteProblem(w, r, http.StatusUnprocessableEntity,
			"entity_type has to be one of "+strings.Join(linkEntityTypes, ", ")+".")
		return nil, false
	}
	id, err := strconv.ParseInt(rawID, 10, 64)
	if err != nil || id < 1 {
		WriteProblem(w, r, http.StatusUnprocessableEntity, "entity_id has to name a record.")
		return nil, false
	}
	return &linkRequest{EntityType: entityType, EntityID: id}, true
}

// contentType settles on a type for stored content.
//
// The browser's declared type is a hint from the client and is not trusted
// past this point: it decides only what we record, and the disposition
// allowlist decides what a browser is allowed to do with it.
func contentType(declared, filename string) string {
	if t := baseType(declared); t != "" && t != "application/octet-stream" {
		return t
	}
	if ext := filepath.Ext(filename); ext != "" {
		if byExt := baseType(mime.TypeByExtension(ext)); byExt != "" {
			return byExt
		}
	}
	return "application/octet-stream"
}

// baseType strips any parameters from a media type and lowercases it, so
// "TEXT/PLAIN; charset=utf-8" and "text/plain" are one thing.
func baseType(value string) string {
	t, _, err := mime.ParseMediaType(value)
	if err != nil {
		return ""
	}
	return strings.ToLower(t)
}

// userID reports who is signed in, for the uploaded_by column.
func userID(r *http.Request) *int64 {
	user, ok := auth.UserFrom(r.Context())
	if !ok {
		return nil
	}
	return &user.ID
}

func (s *server) documentReadError(w http.ResponseWriter, r *http.Request, err error) {
	if store.NotFound(err) {
		WriteProblem(w, r, http.StatusNotFound, "No such document.")
		return
	}
	loggerFrom(r.Context()).Error("read document", "error", err)
	WriteProblem(w, r, http.StatusInternalServerError, "Could not read the document.")
}

func (s *server) documentWriteError(w http.ResponseWriter, r *http.Request, err error) {
	var invalid validationError
	switch {
	case errors.As(err, &invalid):
		WriteProblem(w, r, http.StatusUnprocessableEntity, invalid.detail)
	case store.NotFound(err):
		WriteProblem(w, r, http.StatusNotFound, "No such document.")
	default:
		loggerFrom(r.Context()).Error("write document", "error", err)
		WriteProblem(w, r, http.StatusInternalServerError, "Could not save the document.")
	}
}
