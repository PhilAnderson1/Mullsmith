// Path safety (§9.7): the ONE way any file operation — HTTP uploads/downloads
// and all fs_* tools — maps a user-supplied relative path onto disk.
//
// Defense layers:
//  1. ParseRel rejects absolute paths, ".." in any segment, backslashes,
//     NUL/control characters and over-long inputs — before touching the FS.
//  2. Every open walks the path openat-style from the (trusted, ULID-derived)
//     user root with O_NOFOLLOW on every component, so neither the final
//     target nor any intermediate directory can be a symlink pointing out of
//     the tree — including swaps that race a resolution mid-flight.
//  3. The user root itself is only ever built from a validated ULID, so no
//     input can even address another user's directory.
package scratch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var (
	ErrPathUnsafe = errors.New("path rejected by scratchpad sandbox")
	ErrNotFound   = errors.New("not found")
	ErrExists     = errors.New("already exists")
	ErrNotDir     = errors.New("not a directory")
	ErrIsDir      = errors.New("is a directory")
)

const (
	maxRelLen     = 4096
	maxSegments   = 32
	maxSegmentLen = 255
)

// ParseRel validates a user-supplied relative path and splits it into
// segments. Returns an empty slice for "." (the user root itself).
func ParseRel(rel string) ([]string, error) {
	if rel == "" || len(rel) > maxRelLen {
		return nil, fmt.Errorf("%w: empty or too long", ErrPathUnsafe)
	}
	// Backslashes are rejected outright: on Linux they are legal filename
	// bytes but everywhere else they read as separators, and every "clever"
	// traversal case we care about involves them.
	if strings.ContainsAny(rel, "\x00\\") || strings.HasPrefix(rel, "/") {
		return nil, fmt.Errorf("%w: absolute path or backslash", ErrPathUnsafe)
	}
	segs := strings.Split(rel, "/")
	if len(segs) > maxSegments {
		return nil, fmt.Errorf("%w: too many path segments", ErrPathUnsafe)
	}
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		if s == "" || s == "." {
			continue // "a//b", "./a", trailing "/" all normalize away
		}
		if s == ".." {
			return nil, fmt.Errorf("%w: '..' segment", ErrPathUnsafe)
		}
		if len(s) > maxSegmentLen {
			return nil, fmt.Errorf("%w: segment too long", ErrPathUnsafe)
		}
		if strings.IndexFunc(s, func(r rune) bool { return r < 0x20 }) >= 0 {
			return nil, fmt.Errorf("%w: control character", ErrPathUnsafe)
		}
		// Windows volume prefixes ("C:", "c:") never match filepath.VolumeName
		// on Linux, so they would sail through as ordinary segment names.
		if len(s) >= 2 && s[1] == ':' &&
			((s[0] >= 'A' && s[0] <= 'Z') || (s[0] >= 'a' && s[0] <= 'z')) {
			return nil, fmt.Errorf("%w: volume prefix", ErrPathUnsafe)
		}
		out = append(out, s)
	}
	return out, nil
}

// Resolve returns the on-disk absolute path for rel inside the user's
// scratchpad. The result is purely lexical — safe for display, quota walking
// and the TTL sweeper — but NEVER open the returned string for I/O directly;
// use the Open*/Create/Remove helpers below, which are fd-pinned.
func Resolve(root, userID, rel string) (string, error) {
	segs, err := ParseRel(rel)
	if err != nil {
		return "", err
	}
	dir, err := UserRoot(root, userID)
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{dir}, segs...)...), nil
}

// --- fd-pinned operations ---------------------------------------------------

type openResult struct {
	parent *os.File // fd of the directory containing the final segment
	final  string   // final segment name ("" when the path IS the user root)
}

// openUserRoot pins the user root as an fd. O_NOFOLLOW applies to the root
// itself as well as the walk below it (astra finding 4): a ULID directory
// substituted for a symlink must not become a bridge into an external tree.
// The configured root above it is the documented trust boundary — it is
// resolved with ordinary symlink following, so a data root that is itself a
// symlink remains a supported deployment shape.
func openUserRoot(root, userID string) (*os.File, error) {
	dir, err := UserRoot(root, userID)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		// Linux reports ENOTDIR (not ELOOP) when O_DIRECTORY meets a symlink.
		if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("%w: user root is a symlink", ErrPathUnsafe)
		}
		return nil, mapOpenErr(err)
	}
	return os.NewFile(uintptr(fd), dir), nil
}

// openParent opens the user root, then every intermediate segment as a
// directory with O_NOFOLLOW, and returns the containing-directory fd WITHOUT
// opening the final element (callers open it with the flags they need).
func openParent(root, userID, rel string) (*openResult, error) {
	segs, err := ParseRel(rel)
	if err != nil {
		return nil, err
	}
	cur, err := openUserRoot(root, userID)
	if err != nil {
		return nil, err
	}
	if len(segs) == 0 {
		return &openResult{parent: cur, final: ""}, nil
	}
	for _, s := range segs[:len(segs)-1] {
		nfd, err := unix.Openat(int(cur.Fd()), s,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			_ = cur.Close() // abandoning the fd chain either way
			// Linux reports ENOTDIR (not ELOOP) when O_DIRECTORY meets a
			// symlink under O_NOFOLLOW; either way it cannot be traversed.
			if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
				return nil, fmt.Errorf("%w: cannot traverse %q", ErrPathUnsafe, s)
			}
			return nil, mapOpenErr(err)
		}
		next := os.NewFile(uintptr(nfd), s)
		_ = cur.Close() // old fd is superseded; a close error leaks at most the fd
		cur = next
	}
	return &openResult{parent: cur, final: segs[len(segs)-1]}, nil
}

func mapOpenErr(err error) error {
	switch {
	case errors.Is(err, unix.ENOENT):
		return ErrNotFound
	case errors.Is(err, unix.EEXIST):
		return ErrExists
	case errors.Is(err, unix.ENOTDIR):
		return ErrNotDir
	case errors.Is(err, unix.EISDIR):
		return ErrIsDir
	case errors.Is(err, unix.ELOOP):
		return fmt.Errorf("%w: symlink in path", ErrPathUnsafe)
	case errors.Is(err, unix.ENXIO):
		// O_WRONLY|O_NONBLOCK on a FIFO nobody is reading: a pipe stands
		// at this path (audit finding 9).
		return fmt.Errorf("%w: a pipe or socket stands in the way", ErrPathUnsafe)
	default:
		return err
	}
}

// OpenFile opens an existing file inside the user's scratchpad. Only
// REGULAR files come out: the open carries O_NONBLOCK (so a script-made
// FIFO cannot block this very open awaiting a partner — audit finding 9)
// and the type check runs via fstat on the OPENED fd, so a swap racing
// code_run can't slip past a path-level pre-check the way a Stat-first
// guard can.
func OpenFile(root, userID, rel string) (*os.File, error) {
	or, err := openParent(root, userID, rel)
	if err != nil {
		return nil, err
	}
	defer or.parent.Close()
	if or.final == "" {
		return nil, ErrIsDir
	}
	fd, err := unix.Openat(int(or.parent.Fd()), or.final,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, mapOpenErr(err)
	}
	f := os.NewFile(uintptr(fd), or.final)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = f.Close()
		return nil, mapOpenErr(err)
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		return f, nil
	case unix.S_IFDIR:
		_ = f.Close()
		return nil, ErrIsDir
	default:
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s is a special file (pipe/socket/device)", ErrPathUnsafe, or.final)
	}
}

// CreateFile creates or truncates a file inside the user's scratchpad,
// creating parent directories as needed. perms applies to new files.
func CreateFile(root, userID, rel string, perms os.FileMode) (*os.File, error) {
	segs, err := ParseRel(rel)
	if err != nil {
		return nil, err
	}
	if len(segs) == 0 {
		return nil, ErrIsDir
	}
	or, err := openParent(root, userID, rel)
	if errors.Is(err, ErrNotFound) {
		if err := MkdirAll(root, userID, strings.Join(segs[:len(segs)-1], "/")); err != nil {
			return nil, err
		}
		or, err = openParent(root, userID, rel)
	}
	if err != nil {
		return nil, err
	}
	defer or.parent.Close()
	// O_NONBLOCK: a FIFO sitting at the target fails fast (ENXIO with no
	// reader) instead of blocking the open until some reader shows up.
	// O_TRUNC is deliberately ABSENT — the fd-level type check must run
	// before anything is truncated (audit finding 9); regular files are
	// truncated immediately after, via Ftruncate on the pinned fd.
	fd, err := unix.Openat(int(or.parent.Fd()), or.final,
		unix.O_WRONLY|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		uint32(perms.Perm()))
	if err != nil {
		return nil, mapOpenErr(err)
	}
	f := os.NewFile(uintptr(fd), or.final)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = f.Close()
		return nil, mapOpenErr(err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = f.Close()
		if st.Mode&unix.S_IFMT == unix.S_IFDIR {
			return nil, ErrIsDir
		}
		return nil, fmt.Errorf("%w: %s is a special file (pipe/socket/device); refusing to open or truncate",
			ErrPathUnsafe, or.final)
	}
	if err := unix.Ftruncate(fd, 0); err != nil {
		_ = f.Close()
		return nil, mapOpenErr(err)
	}
	return f, nil
}

// CreateExclusive creates a NEW regular file inside the user's scratchpad
// and fails with ErrExists when anything is already at the path — unlike
// CreateFile it never opens, truncates or touches whatever is there. It is
// the staging half of an atomic replacement (finding 3): the staged name is
// random, so EEXIST is a collision to retry, never the real target.
// Parent directories are created as needed, like CreateFile.
func CreateExclusive(root, userID, rel string, perms os.FileMode) (*os.File, error) {
	segs, err := ParseRel(rel)
	if err != nil {
		return nil, err
	}
	if len(segs) == 0 {
		return nil, ErrIsDir
	}
	or, err := openParent(root, userID, rel)
	if errors.Is(err, ErrNotFound) {
		if err := MkdirAll(root, userID, strings.Join(segs[:len(segs)-1], "/")); err != nil {
			return nil, err
		}
		or, err = openParent(root, userID, rel)
	}
	if err != nil {
		return nil, err
	}
	defer or.parent.Close()
	// O_EXCL never follows a symlink even when O_NOFOLLOW is also set — an
	// existing path of ANY type loses to EEXIST before a byte is written.
	fd, err := unix.Openat(int(or.parent.Fd()), or.final,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		uint32(perms.Perm()))
	if err != nil {
		if errors.Is(err, unix.EEXIST) {
			return nil, ErrExists
		}
		return nil, mapOpenErr(err)
	}
	return os.NewFile(uintptr(fd), or.final), nil
}

// RenameOver atomically moves srcRel onto dstRel INSIDE the user's
// scratchpad; both must live in the same directory, which makes the
// replace atomic without ever resolving a second path. The destination is
// never opened or followed: rename(2) swaps out a regular file OR a symlink
// sitting at dstRel, so nothing outside the tree can be touched — a
// directory destination fails (EISDIR/ENOTEMPTY, reported as ErrIsDir).
// This is the commit step of a staged replacement (finding 3); the original
// at dstRel survives every path where the staging write or the rename fails.
func RenameOver(root, userID, srcRel, dstRel string) error {
	srcSegs, err := ParseRel(srcRel)
	if err != nil {
		return err
	}
	dstSegs, err := ParseRel(dstRel)
	if err != nil {
		return err
	}
	if len(srcSegs) == 0 || len(dstSegs) == 0 {
		return ErrIsDir
	}
	if len(srcSegs) != len(dstSegs) {
		return fmt.Errorf("%w: rename across directories", ErrPathUnsafe)
	}
	for i := range srcSegs[:len(srcSegs)-1] {
		if srcSegs[i] != dstSegs[i] {
			return fmt.Errorf("%w: rename across directories", ErrPathUnsafe)
		}
	}
	or, err := openParent(root, userID, dstRel)
	if err != nil {
		return err
	}
	defer or.parent.Close()
	if err := unix.Renameat(int(or.parent.Fd()), srcSegs[len(srcSegs)-1],
		int(or.parent.Fd()), dstSegs[len(dstSegs)-1]); err != nil {
		if errors.Is(err, unix.EISDIR) || errors.Is(err, unix.ENOTEMPTY) {
			return ErrIsDir
		}
		return mapOpenErr(err)
	}
	return nil
}

// MkdirAll creates the directory chain rel inside the user's scratchpad,
// fd-pinned end to end (astra finding 1): the walk opens the user root,
// creates each component with mkdirat against the pinned parent fd, and
// descends into it with openat(O_DIRECTORY|O_NOFOLLOW) — it never re-resolves
// a reconstructed absolute string. That closes the parent-symlink race the
// old string-based walk had: a component swapped for a symlink BEFORE it is
// created is refused by the no-follow descend, and one swapped AFTER it is
// created cannot steer the walk either — the pinned fd still names the real
// directory, so the next mkdirat lands inside the tree regardless. Existing
// elements must be real directories; a symlink at any level aborts the
// walk (§9.7).
func MkdirAll(root, userID, rel string) error {
	segs, err := ParseRel(rel)
	if err != nil || len(segs) == 0 {
		if err == nil {
			return nil // "." — the user root must already exist
		}
		return err
	}
	cur, err := openUserRoot(root, userID)
	if err != nil {
		return err
	}
	for _, s := range segs {
		nfd, err := mkdiratDescend(int(cur.Fd()), s)
		if err != nil {
			_ = cur.Close()
			return err
		}
		next := os.NewFile(uintptr(nfd), s)
		_ = cur.Close() // old fd is superseded; a close error leaks at most the fd
		cur = next
		if h := testHookMkdirPinned; h != nil {
			h(s)
		}
	}
	_ = cur.Close() // traversal fd; nothing was written through it
	return nil
}

// mkdiratDescend creates name under parentFd (tolerating EEXIST) and returns
// an fd for the directory it names. Symlinks and non-directories are refused
// without ever resolving a path string.
func mkdiratDescend(parentFd int, name string) (int, error) {
	if err := unix.Mkdirat(parentFd, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, mapOpenErr(err)
	}
	nfd, err := unix.Openat(parentFd, name,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		// Linux reports ENOTDIR (not ELOOP) when O_DIRECTORY meets a symlink
		// under O_NOFOLLOW; either way the walk stops here. The no-follow
		// fstatat only classifies the object for the error the caller sees.
		if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
			var st unix.Stat_t
			if serr := unix.Fstatat(parentFd, name, &st, unix.AT_SYMLINK_NOFOLLOW); serr == nil &&
				st.Mode&unix.S_IFMT == unix.S_IFLNK {
				return -1, fmt.Errorf("%w: symlink at %q", ErrPathUnsafe, name)
			}
			return -1, ErrNotDir
		}
		return -1, mapOpenErr(err)
	}
	return nfd, nil
}

// testHookMkdirPinned, when non-nil, runs after MkdirAll has pinned the fd
// for one segment and before it creates the next — the exact moment the old
// string-based walk stopped trusting the path. Production never sets it; the
// parent-symlink race regression test (astra finding 1) uses it to interleave
// a hostile rename+symlink deterministically.
var testHookMkdirPinned func(segment string)

// OpenDir opens a directory inside the user's scratchpad; "" is the user root.
func OpenDir(root, userID, rel string) (*os.File, error) {
	or, err := openParent(root, userID, rel)
	if err != nil {
		return nil, err
	}
	if or.final == "" {
		return or.parent, nil // caller owns the root fd; do not close it
	}
	defer or.parent.Close()
	fd, err := unix.Openat(int(or.parent.Fd()), or.final,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, mapOpenErr(err)
	}
	return os.NewFile(uintptr(fd), or.final), nil
}

// ReadDirLimit lists at most limit entries of a directory inside the user's
// scratchpad, reporting whether more exist (astra finding 5). A hostile
// script can create millions of entries cheaply (each counts ~zero bytes),
// so no response path may load a whole listing into daemon memory: this
// reads limit+1 names and stops.
func ReadDirLimit(root, userID, rel string, limit int) (entries []fs.DirEntry, truncated bool, err error) {
	if limit <= 0 {
		return nil, false, errors.New("ReadDirLimit: limit must be positive")
	}
	f, err := OpenDir(root, userID, rel)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	entries, err = f.ReadDir(limit + 1) // directory order, as ReadDir(-1) has always returned
	if errors.Is(err, io.EOF) {
		// ReadDir(n) reports end-of-directory as an error when its slice
		// came back empty: an empty directory is an empty listing, not a
		// failure.
		err = nil
	}
	if err != nil {
		return nil, false, err
	}
	if len(entries) > limit {
		return entries[:limit], true, nil
	}
	return entries, false, nil
}

// Stat inspects an element without following a final symlink.
func Stat(root, userID, rel string) (os.FileInfo, error) {
	or, err := openParent(root, userID, rel)
	if err != nil {
		return nil, err
	}
	defer or.parent.Close()
	if or.final == "" {
		return or.parent.Stat()
	}
	var st unix.Stat_t
	if err := unix.Fstatat(int(or.parent.Fd()), or.final, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, mapOpenErr(err)
	}
	return &statInfo{st: st, name: or.final}, nil
}

// Remove deletes a file or empty directory; a symlink is unlinked, not
// followed to its target.
func Remove(root, userID, rel string) error {
	or, err := openParent(root, userID, rel)
	if err != nil {
		return err
	}
	defer or.parent.Close()
	if or.final == "" {
		return fmt.Errorf("%w: cannot remove user root", ErrPathUnsafe)
	}
	if err := unix.Unlinkat(int(or.parent.Fd()), or.final, 0); err != nil {
		if errors.Is(err, unix.EISDIR) {
			if rerr := unix.Unlinkat(int(or.parent.Fd()), or.final, unix.AT_REMOVEDIR); rerr != nil {
				return mapOpenErr(rerr)
			}
			return nil
		}
		return mapOpenErr(err)
	}
	return nil
}

// RemoveTree deletes a directory and everything under it: leaves first,
// then directories deepest-up. Every element is removed through Remove, so
// the same fd-pinned sandbox guards the whole descent — a symlinked
// component is unlinked as a link, never traversed, and no path can leave
// the user root.
//
// The walk completes BEFORE anything is deleted: if any part of the tree
// cannot be listed (a chmod-000 directory), RemoveTree returns that error
// and touches nothing — half-deleting while claiming "everything inside is
// gone" would hide surviving bytes behind a success answer. Entries that
// vanish between walk and delete (a script still running) are tolerated:
// those bytes are genuinely gone.
// removeTreeEntryBudget caps how many entries a RemoveTree plan may hold
// (astra finding 5). The plan-first design needs the whole listing in RAM;
// only a deliberately flooded folder reaches this count, and for it the
// fail-closed answer is "refuse, delete nothing". The hourly TTL sweep,
// which streams per directory, remains the backstop for actually clearing
// the junk. A var so tests can run small.
var removeTreeEntryBudget = 1_000_000

func RemoveTree(root, userID, rel string) error {
	fi, err := Stat(root, userID, rel)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: RemoveTree target %q is not a directory", ErrNotDir, rel)
	}
	// The plan is built by the streaming pinned walker (astra2 finding 2):
	// batched reads mean even an over-budget flood is refused before a
	// whole directory has been loaded, instead of after.
	var files, dirs []string
	err = scanUserTree(context.Background(), root, userID, rel, removeTreeEntryBudget, func(e scanEntry) error {
		if e.st.Mode&unix.S_IFMT == unix.S_IFDIR {
			dirs = append(dirs, e.rel)
		} else {
			files = append(files, e.rel) // regular, symlink, special: unlinked as leaves
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errScanBudget) {
			return fmt.Errorf("remove %q: over %d entries, refusing to plan a delete this large",
				rel, removeTreeEntryBudget)
		}
		return err // unreadable anything ⇒ incomplete plan ⇒ abort
	}
	for _, f := range files {
		if rerr := Remove(root, userID, f); rerr != nil && !errors.Is(rerr, ErrNotFound) {
			return rerr
		}
	}
	// Pre-order collected dirs: parents precede children, so reverse order
	// removes children first and every rmdir meets an empty directory.
	for i := len(dirs) - 1; i >= 0; i-- {
		if rerr := Remove(root, userID, dirs[i]); rerr != nil && !errors.Is(rerr, ErrNotFound) {
			return rerr
		}
	}
	return Remove(root, userID, rel)
}

// statInfo adapts unix.Stat_t to fs.FileInfo for the fields we consume.
type statInfo struct {
	st   unix.Stat_t
	name string
}

func (s *statInfo) Name() string       { return s.name }
func (s *statInfo) Size() int64        { return s.st.Size }
func (s *statInfo) IsDir() bool        { return s.st.Mode&unix.S_IFMT == unix.S_IFDIR }
func (s *statInfo) Sys() any           { return &s.st }
func (s *statInfo) ModTime() time.Time { return time.Unix(s.st.Mtim.Sec, int64(s.st.Mtim.Nsec)) }
func (s *statInfo) Mode() fs.FileMode {
	m := fs.FileMode(s.st.Mode & 0o777)
	// Full type bits, or IsRegular() would answer true for a FIFO (its type
	// would come out as 0 = "regular") and a caller would happily open one
	// for read and block until a writer appears.
	switch s.st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		m |= fs.ModeDir
	case unix.S_IFLNK:
		m |= fs.ModeSymlink
	case unix.S_IFIFO:
		m |= fs.ModeNamedPipe
	case unix.S_IFSOCK:
		m |= fs.ModeSocket
	case unix.S_IFCHR:
		m |= fs.ModeDevice
	case unix.S_IFBLK:
		m |= fs.ModeDevice | fs.ModeCharDevice
	}
	return m
}
