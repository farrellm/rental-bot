// Package blob is the content-addressed store the documents live in.
//
// A file's SHA-256 is its name and its whole identity (docs/DESIGN.md §9.1).
// That buys three things for free rather than as features: re-forwarding the
// same PDF writes nothing the second time, a corrupted file is detectable by
// reading it, and a backup can be compared to the source without trusting a
// timestamp.
//
// Bytes never go in the database. The database holds the row; this holds the
// file, at blobs/<sha[0:2]>/<sha[2:4]>/<sha256>. The two-level fan-out keeps
// any one directory small enough that listing it is not a mistake.
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Permissions. The blob directory is never mapped by the reverse proxy and is
// served only through the authenticated handler, so nothing outside the service
// user has any business reading it (§9.2).
const (
	dirMode  os.FileMode = 0o700
	fileMode os.FileMode = 0o600
)

// tmpDir holds partial writes. It sits inside the root so the finishing rename
// is within one filesystem, which is what makes that rename atomic.
const tmpDir = "tmp"

// ErrNotFound reports that no blob is stored under that digest.
var ErrNotFound = errors.New("blob: not found")

// ErrBadDigest reports a digest that is not a SHA-256 in lowercase hex.
//
// This is the one place a caller-supplied string becomes a filesystem path, so
// the check is a boundary and not a formality: without it "../../secret.key" is
// a digest.
var ErrBadDigest = errors.New("blob: not a sha-256 digest")

// Store is a content-addressed directory of files.
type Store struct {
	root string
}

// Ref names stored content: what it hashes to, how long it is, and where it
// sits relative to the store root.
//
// Path is relative on purpose. It goes into documents.storage_path, and an
// absolute one would mean moving the data directory rewrites every row.
type Ref struct {
	SHA256 string
	Size   int64
	Path   string
}

// New prepares a store rooted at dir, creating it if it does not exist.
func New(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("blob: root directory is empty")
	}
	if err := os.MkdirAll(filepath.Join(dir, tmpDir), dirMode); err != nil {
		return nil, fmt.Errorf("blob: create %s: %w", dir, err)
	}
	return &Store{root: dir}, nil
}

// Root reports the directory this store writes into.
func (s *Store) Root() string { return s.root }

// Put stores everything r yields and returns what it hashed to.
//
// The digest is not known until the last byte has been read, so the content
// goes to a temporary file while it is hashed and is renamed into place
// afterward. A reader that fails halfway leaves nothing behind but the temp
// file, which is removed; a blob in the tree is therefore always complete and
// always matches the name it is filed under.
//
// Content already on file is left exactly as it is. That is the dedupe §9.1
// describes, and it falls out of the write path rather than being a feature
// somebody has to remember to call.
func (s *Store) Put(ctx context.Context, r io.Reader) (Ref, error) {
	tmp, err := os.CreateTemp(filepath.Join(s.root, tmpDir), "put-*")
	if err != nil {
		return Ref{}, fmt.Errorf("blob: create temp: %w", err)
	}
	tmpName := tmp.Name()
	// Removing a file that was renamed away is a no-op, so this covers both
	// the failure paths and the already-on-file one without a flag.
	defer os.Remove(tmpName)
	defer tmp.Close()

	if err := tmp.Chmod(fileMode); err != nil {
		return Ref{}, fmt.Errorf("blob: chmod temp: %w", err)
	}

	sum := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, sum), &contextReader{ctx: ctx, r: r})
	if err != nil {
		return Ref{}, fmt.Errorf("blob: write: %w", err)
	}
	// The rename below is atomic, but it does not promise the bytes reached the
	// disk. Without this a power cut can leave a correctly named, empty file --
	// which is worse than a missing one, because the row says it is there.
	if err := tmp.Sync(); err != nil {
		return Ref{}, fmt.Errorf("blob: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Ref{}, fmt.Errorf("blob: close temp: %w", err)
	}

	digest := hex.EncodeToString(sum.Sum(nil))
	rel := relPath(digest)
	final := filepath.Join(s.root, rel)

	if err := os.MkdirAll(filepath.Dir(final), dirMode); err != nil {
		return Ref{}, fmt.Errorf("blob: create %s: %w", filepath.Dir(rel), err)
	}

	// Already filed. The bytes are identical by definition -- that is what a
	// matching digest means -- so the copy on disk stays and the temp goes.
	if _, err := os.Stat(final); err == nil {
		return Ref{SHA256: digest, Size: size, Path: rel}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Ref{}, fmt.Errorf("blob: stat %s: %w", rel, err)
	}

	if err := os.Rename(tmpName, final); err != nil {
		return Ref{}, fmt.Errorf("blob: file %s: %w", rel, err)
	}
	return Ref{SHA256: digest, Size: size, Path: rel}, nil
}

// Open returns the content stored under digest.
//
// The result seeks, because http.ServeContent needs to in order to answer a
// range request -- which is how a browser scrubs a PDF without downloading all
// of it.
func (s *Store) Open(digest string) (*os.File, error) {
	path, err := s.Path(digest)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, digest)
	}
	if err != nil {
		return nil, fmt.Errorf("blob: open %s: %w", digest, err)
	}
	return f, nil
}

// Stat reports the size of stored content without opening it.
func (s *Store) Stat(digest string) (int64, error) {
	path, err := s.Path(digest)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("%w: %s", ErrNotFound, digest)
	}
	if err != nil {
		return 0, fmt.Errorf("blob: stat %s: %w", digest, err)
	}
	return info.Size(), nil
}

// Path returns the absolute path a digest is stored at, refusing anything that
// is not a digest.
func (s *Store) Path(digest string) (string, error) {
	if !ValidDigest(digest) {
		return "", fmt.Errorf("%w: %q", ErrBadDigest, digest)
	}
	return filepath.Join(s.root, relPath(digest)), nil
}

// ValidDigest reports whether s is a SHA-256 in lowercase hex.
//
// Lowercase specifically: accepting both cases would file the same content at
// two paths and quietly break the dedupe the whole design rests on.
func ValidDigest(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// relPath is the store-relative path for a digest. It is only ever called with
// a digest that has been validated, either by ValidDigest or by having just
// come out of sha256.
func relPath(digest string) string {
	return strings.Join([]string{digest[0:2], digest[2:4], digest}, "/")
}

// contextReader stops a copy when the request is cancelled, so a client that
// hangs up mid-upload does not leave the process writing to disk on its behalf.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
