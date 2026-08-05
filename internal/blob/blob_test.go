package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// storedFiles lists every blob in the tree, ignoring the temp directory.
func storedFiles(t *testing.T, s *Store) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(s.Root(), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == tmpDir {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(s.Root(), path)
		if err != nil {
			return err
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return out
}

func TestPutRoundTrips(t *testing.T) {
	s := newStore(t)
	content := []byte("a receipt, forwarded")

	ref, err := s.Put(t.Context(), bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	if ref.SHA256 != digestOf(content) {
		t.Errorf("SHA256 = %s, want %s", ref.SHA256, digestOf(content))
	}
	if ref.Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", ref.Size, len(content))
	}
	// The path is relative, so moving the data directory does not rewrite
	// every documents row.
	if filepath.IsAbs(ref.Path) {
		t.Errorf("Path = %q, want a store-relative path", ref.Path)
	}
	want := ref.SHA256[0:2] + "/" + ref.SHA256[2:4] + "/" + ref.SHA256
	if ref.Path != want {
		t.Errorf("Path = %q, want %q", ref.Path, want)
	}

	f, err := s.Open(ref.SHA256)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("read back %q, want %q", got, content)
	}

	size, err := s.Stat(ref.SHA256)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if size != int64(len(content)) {
		t.Errorf("Stat = %d, want %d", size, len(content))
	}
}

func TestPutDedupesIdenticalContent(t *testing.T) {
	// The point of content addressing: forwarding the same PDF twice is
	// normal, and the second one must not become a second file.
	s := newStore(t)
	content := []byte("the same lease, forwarded again")

	first, err := s.Put(t.Context(), bytes.NewReader(content))
	if err != nil {
		t.Fatalf("first Put: %v", err)
	}
	second, err := s.Put(t.Context(), bytes.NewReader(content))
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}

	if first != second {
		t.Errorf("second Put returned %+v, want %+v", second, first)
	}
	if files := storedFiles(t, s); len(files) != 1 {
		t.Errorf("%d files on disk, want 1: %v", len(files), files)
	}
}

func TestPutLeavesNothingBehindWhenTheReaderFails(t *testing.T) {
	// A client that hangs up mid-upload must not leave a partial file that a
	// later read would serve as if it were the document.
	s := newStore(t)
	boom := errors.New("connection reset")

	_, err := s.Put(t.Context(), io.MultiReader(
		strings.NewReader("the first half"),
		failingReader{boom},
	))
	if !errors.Is(err, boom) {
		t.Fatalf("Put returned %v, want the reader's error", err)
	}

	if files := storedFiles(t, s); len(files) != 0 {
		t.Errorf("a failed upload left %v behind, want nothing", files)
	}
	temps, err := os.ReadDir(filepath.Join(s.Root(), tmpDir))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(temps) != 0 {
		t.Errorf("%d temp files survived, want 0", len(temps))
	}
}

func TestPutStopsWhenTheRequestIsCancelled(t *testing.T) {
	s := newStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := s.Put(ctx, strings.NewReader("anything")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put returned %v, want context.Canceled", err)
	}
	if files := storedFiles(t, s); len(files) != 0 {
		t.Errorf("a cancelled upload left %v behind, want nothing", files)
	}
}

func TestDigestsAreValidatedBeforeTheyBecomePaths(t *testing.T) {
	// This is the only place a caller-supplied string becomes a filesystem
	// path. Without the check, "../../secret.key" is a digest.
	valid := digestOf([]byte("x"))

	tests := []struct {
		name   string
		digest string
		want   bool
	}{
		{name: "a real digest", digest: valid, want: true},
		{name: "traversal", digest: "../../../../etc/passwd", want: false},
		{name: "empty", digest: "", want: false},
		{name: "too short", digest: valid[:63], want: false},
		{name: "too long", digest: valid + "0", want: false},
		{name: "uppercase would file the same bytes twice", digest: strings.ToUpper(valid), want: false},
		{name: "not hex", digest: strings.Repeat("z", 64), want: false},
		{name: "a separator inside", digest: valid[:31] + "/" + valid[32:], want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ValidDigest(tt.digest); got != tt.want {
				t.Errorf("ValidDigest(%q) = %v, want %v", tt.digest, got, tt.want)
			}
		})
	}

	s := newStore(t)
	for _, bad := range []string{"../../../../etc/passwd", "", strings.ToUpper(valid)} {
		if _, err := s.Path(bad); !errors.Is(err, ErrBadDigest) {
			t.Errorf("Path(%q) error = %v, want ErrBadDigest", bad, err)
		}
		if _, err := s.Open(bad); !errors.Is(err, ErrBadDigest) {
			t.Errorf("Open(%q) error = %v, want ErrBadDigest", bad, err)
		}
	}
}

func TestOpenReportsAMissingBlobDistinctly(t *testing.T) {
	// A row that names a blob the disk does not have is a real condition after
	// a partial restore, and the handler has to tell it apart from a bad
	// request.
	s := newStore(t)

	_, err := s.Open(digestOf([]byte("never stored")))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Open error = %v, want ErrNotFound", err)
	}
	if _, err := s.Stat(digestOf([]byte("never stored"))); !errors.Is(err, ErrNotFound) {
		t.Errorf("Stat error = %v, want ErrNotFound", err)
	}
}

func TestStoredFilesAreNotReadableByAnyoneElse(t *testing.T) {
	// The blob directory is served only through the authenticated handler and
	// is never mapped by the reverse proxy (DESIGN.md 9.2).
	s := newStore(t)
	ref, err := s.Put(t.Context(), strings.NewReader("a lease"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	path, err := s.Path(ref.SHA256)
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("blob mode is %#o, want no group or other bits", perm)
	}

	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if perm := dir.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("blob directory mode is %#o, want no group or other bits", perm)
	}
}

func TestPutHandlesAnEmptyFile(t *testing.T) {
	// Zero bytes still hashes, and a zero-length attachment is a real thing to
	// receive rather than an error.
	s := newStore(t)

	ref, err := s.Put(t.Context(), bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if ref.Size != 0 {
		t.Errorf("Size = %d, want 0", ref.Size)
	}
	if ref.SHA256 != digestOf(nil) {
		t.Errorf("SHA256 = %s, want the empty digest %s", ref.SHA256, digestOf(nil))
	}
}

type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }
