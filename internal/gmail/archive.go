package gmail

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Permissions match internal/blob: the archive holds forwarded mail, which is
// as private as the documents it carries.
const (
	dirMode  os.FileMode = 0o700
	fileMode os.FileMode = 0o600
)

// Archive is the raw .eml store at storage.raw_email (docs/DESIGN.md §9.1).
//
// Its whole purpose is that a parser fix can be replayed against the bytes that
// actually arrived. That only works if the write happens before anything has a
// chance to fail — so sync archives first and inserts the row second, and the
// message that broke the parser is exactly the one whose original is on disk.
type Archive struct {
	root string
}

// NewArchive prepares the archive directory.
func NewArchive(root string) (*Archive, error) {
	if root == "" {
		return nil, errors.New("gmail: the raw email directory is empty")
	}
	if err := os.MkdirAll(root, dirMode); err != nil {
		return nil, fmt.Errorf("gmail: create %s: %w", root, err)
	}
	return &Archive{root: root}, nil
}

// Root reports the directory this archive writes into.
func (a *Archive) Root() string { return a.root }

// Put writes one message and returns its path relative to the archive root.
//
// Relative for the same reason documents.storage_path is: moving the data
// directory should not rewrite every row.
//
// The write is to a temporary file and a rename, so a process killed mid-write
// leaves no half-message under a name that claims to be a whole one.
func (a *Archive) Put(messageID string, receivedAt time.Time, raw []byte) (string, error) {
	name, err := archiveName(messageID, receivedAt)
	if err != nil {
		return "", err
	}
	full := filepath.Join(a.root, filepath.FromSlash(name))

	if err := os.MkdirAll(filepath.Dir(full), dirMode); err != nil {
		return "", fmt.Errorf("gmail: create %s: %w", filepath.Dir(name), err)
	}

	// Already archived. The bytes Gmail holds for a message id do not change,
	// so a re-sync rewriting the file would be work for no difference.
	if _, err := os.Stat(full); err == nil {
		return name, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("gmail: stat %s: %w", name, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(full), ".put-*")
	if err != nil {
		return "", fmt.Errorf("gmail: create temp in %s: %w", filepath.Dir(name), err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // a no-op once the rename has moved it away
	defer tmp.Close()

	if err := tmp.Chmod(fileMode); err != nil {
		return "", fmt.Errorf("gmail: chmod temp: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		return "", fmt.Errorf("gmail: write %s: %w", name, err)
	}
	// The rename is atomic but does not promise the bytes reached the disk.
	// Without this a power cut can leave a correctly named empty file, which is
	// worse than a missing one because the row says it is there.
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("gmail: sync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("gmail: close temp: %w", err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		return "", fmt.Errorf("gmail: file %s: %w", name, err)
	}
	return name, nil
}

// Open reads an archived message back.
func (a *Archive) Open(relative string) (*os.File, error) {
	path, err := a.Path(relative)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("gmail: no archived message at %s", relative)
	}
	if err != nil {
		return nil, fmt.Errorf("gmail: open %s: %w", relative, err)
	}
	return f, nil
}

// Path resolves a stored relative path, refusing anything that escapes the
// root.
//
// The value comes out of the database, and a row is not a trust boundary that
// never moves: this is the one place a stored string becomes a filesystem path,
// the same role blob.ValidDigest plays for documents.
func (a *Archive) Path(relative string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(relative))
	if relative == "" || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("gmail: %q is not a path inside the archive", relative)
	}
	return filepath.Join(a.root, clean), nil
}

// archiveName is raw-email/<yyyy>/<mm>/<gmail_message_id>.eml, minus the root.
//
// The year and month fan the directory out so no one of them grows past what
// is comfortable to list, and they make "delete everything before 2024" — the
// retention question §12 leaves open — a directory operation.
func archiveName(messageID string, receivedAt time.Time) (string, error) {
	if !validMessageID(messageID) {
		return "", fmt.Errorf("gmail: %q is not a message id", messageID)
	}
	if receivedAt.IsZero() {
		receivedAt = time.Now()
	}
	at := receivedAt.UTC()
	return fmt.Sprintf("%04d/%02d/%s.eml", at.Year(), int(at.Month()), messageID), nil
}

// validMessageID reports whether id is safe to use as a filename.
//
// Gmail message ids are lowercase hex, but the check is what makes that a
// guarantee rather than an assumption — this is a caller-supplied string
// becoming a path.
func validMessageID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := range len(id) {
		c := id[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}
