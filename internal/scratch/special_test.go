package scratch

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// --- audit finding 9: special files must never block or be truncated -------

// mustRunWithin fails the test if f stalls: pre-fix, an open on a FIFO
// blocks this goroutine forever, which IS the regression proof.
func mustRunWithin(t *testing.T, name string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: blocked — special-file handling must never hang", name)
	}
}

func mkfifoUnder(t *testing.T, root, rel string) string {
	t.Helper()
	p := filepath.Join(root, testULID, rel)
	if err := unix.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOpenFileRejectsFifoWithoutBlocking(t *testing.T) {
	root := newRoot(t)
	mkfifoUnder(t, root, "work/pipe")

	mustRunWithin(t, "OpenFile(fifo)", func() {
		_, err := OpenFile(root, testULID, "work/pipe")
		if !errors.Is(err, ErrPathUnsafe) {
			t.Errorf("err = %v, want ErrPathUnsafe", err)
		}
	})
}

func TestCreateFileRejectsFifo(t *testing.T) {
	root := newRoot(t)
	p := mkfifoUnder(t, root, "work/pipe")

	// No reader: the O_NONBLOCK open must fail fast, not wait for one.
	mustRunWithin(t, "CreateFile(fifo, no reader)", func() {
		if _, err := CreateFile(root, testULID, "work/pipe", 0o600); err == nil {
			t.Error("CreateFile on a fifo succeeded")
		}
	})

	// With a waiting reader the open itself SUCCEEDS — the fd-level fstat
	// must still refuse, before any truncate touches the path.
	rd, err := unix.Open(p, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(rd)
	mustRunWithin(t, "CreateFile(fifo, waiting reader)", func() {
		_, err := CreateFile(root, testULID, "work/pipe", 0o600)
		if !errors.Is(err, ErrPathUnsafe) {
			t.Errorf("err = %v, want ErrPathUnsafe", err)
		}
	})

	info, err := os.Lstat(p)
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("fifo disturbed: %v %v, must remain untouched", info, err)
	}
}

func TestOpenFileOnDirectoryIsErrIsDir(t *testing.T) {
	root := newRoot(t)
	if _, err := OpenFile(root, testULID, "uploads"); !errors.Is(err, ErrIsDir) {
		t.Errorf("err = %v, want ErrIsDir", err)
	}
}

func TestCreateFileStillTruncatesRegularFiles(t *testing.T) {
	root := newRoot(t)
	f, err := CreateFile(root, testULID, "work/a.txt", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("0123456789")
	f.Close()

	f, err = CreateFile(root, testULID, "work/a.txt", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("abc")
	f.Close()

	info, err := os.Stat(filepath.Join(root, testULID, "work", "a.txt"))
	if err != nil || info.Size() != 3 {
		t.Errorf("size = %v, %v, want 3 — O_TRUNC semantics must be preserved", info, err)
	}
}
