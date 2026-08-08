package gmail

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestArchiveFilesByYearAndMonth(t *testing.T) {
	archive := newArchive(t)
	at := time.Date(2026, 3, 7, 14, 2, 0, 0, time.UTC)

	path, err := archive.Put("18f0c0ffee", at, []byte("From: me@example.com\r\n\r\nbody"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if want := "2026/03/18f0c0ffee.eml"; path != want {
		t.Errorf("path = %q, want %q", path, want)
	}

	info, err := os.Stat(filepath.Join(archive.Root(), path))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// Forwarded mail is as private as the documents it carries.
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %#o, want 0600", perm)
	}
}

func TestArchiveIsIdempotent(t *testing.T) {
	archive := newArchive(t)
	at := time.Now()

	first, err := archive.Put("m1", at, []byte("original"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	// The bytes Gmail holds for a message id do not change, so a re-sync must
	// not rewrite the file.
	second, err := archive.Put("m1", at, []byte("something else entirely"))
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}
	if first != second {
		t.Errorf("paths differ: %q and %q", first, second)
	}

	content, err := os.ReadFile(filepath.Join(archive.Root(), first))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(content) != "original" {
		t.Errorf("content = %q, want the first write to stand", content)
	}
}

// The id becomes a filename, so it is a boundary and not a formality --
// blob.ValidDigest plays the same role for documents.
func TestArchiveRefusesAMessageIDThatIsAPath(t *testing.T) {
	archive := newArchive(t)

	for _, id := range []string{"../../secret.key", "a/b", "", "with space", "sub/../x"} {
		if _, err := archive.Put(id, time.Now(), []byte("x")); err == nil {
			t.Errorf("Put(%q) succeeded; that id is a path, not a message id", id)
		}
	}
}

// The stored path comes back out of the database, which is not a boundary that
// never moves.
func TestArchivePathRefusesAnEscape(t *testing.T) {
	archive := newArchive(t)

	for _, stored := range []string{"../../../etc/passwd", "/etc/passwd", "..", ""} {
		if _, err := archive.Path(stored); err == nil {
			t.Errorf("Path(%q) resolved; it escapes the archive", stored)
		}
	}

	if _, err := archive.Path("2026/03/m1.eml"); err != nil {
		t.Errorf("Path rejected a legitimate stored path: %v", err)
	}
}

func TestArchiveOpenReadsBack(t *testing.T) {
	archive := newArchive(t)
	path, err := archive.Put("m1", time.Now(), []byte("the original bytes"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	f, err := archive.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	if _, err := archive.Open("2026/03/never-written.eml"); err == nil {
		t.Error("Open succeeded for a message that was never archived")
	}
}

func newArchive(t *testing.T) *Archive {
	t.Helper()
	archive, err := NewArchive(filepath.Join(t.TempDir(), "raw-email"))
	if err != nil {
		t.Fatalf("NewArchive: %v", err)
	}
	return archive
}
