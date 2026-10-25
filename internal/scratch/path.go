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
	"errors"
	"fmt"
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

// Within reports whether abs is dir itself or below it (both cleaned paths).
func Within(dir, abs string) bool {
	rel, err := filepath.Rel(dir, abs)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// --- fd-pinned operations ---------------------------------------------------

type openResult struct {
	parent *os.File // fd of the directory containing the final segment
	final  string   // final segment name ("" when the path IS the user root)
}

// openParent opens the user root, then every intermediate segment as a
// directory with O_NOFOLLOW, and returns the containing-directory fd WITHOUT
// opening the final element (callers open it with the flags they need).
func openParent(root, userID, rel string) (*openResult, error) {
	segs, err := ParseRel(rel)
	if err != nil {
		return nil, err
	}
	dir, err := UserRoot(root, userID)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, mapOpenErr(err)
	}
	cur := os.NewFile(uintptr(fd), dir)
	if len(segs) == 0 {
		return &openResult{parent: cur, final: ""}, nil
	}
	for _, s := range segs[:len(segs)-1] {
		nfd, err := unix.Openat(int(cur.Fd()), s,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			cur.Close()
			// Linux reports ENOTDIR (not ELOOP) when O_DIRECTORY meets a
			// symlink under O_NOFOLLOW; either way it cannot be traversed.
			if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
				return nil, fmt.Errorf("%w: cannot traverse %q", ErrPathUnsafe, s)
			}
			return nil, mapOpenErr(err)
		}
		next := os.NewFile(uintptr(nfd), s)
		cur.Close()
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
	default:
		return err
	}
}

// OpenFile opens an existing file inside the user's scratchpad.
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
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, mapOpenErr(err)
	}
	return os.NewFile(uintptr(fd), or.final), nil
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
	fd, err := unix.Openat(int(or.parent.Fd()), or.final,
		unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		uint32(perms.Perm()))
	if err != nil {
		return nil, mapOpenErr(err)
	}
	return os.NewFile(uintptr(fd), or.final), nil
}

// MkdirAll creates the directory chain rel inside the user's scratchpad.
// Existing elements must be real directories — a pre-existing symlink at any
// level aborts the walk (§9.7).
func MkdirAll(root, userID, rel string) error {
	segs, err := ParseRel(rel)
	if err != nil || len(segs) == 0 {
		if err == nil {
			return nil // "." — the user root must already exist
		}
		return err
	}
	dir, err := UserRoot(root, userID)
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString(dir)
	for _, s := range segs {
		b.WriteString(string(filepath.Separator))
		b.WriteString(s)
		path := b.String()
		if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		fi, err := os.Lstat(path)
		if err != nil {
			return mapOpenErr(err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: symlink at %q", ErrPathUnsafe, s)
		}
		if !fi.IsDir() {
			return ErrNotDir
		}
	}
	return nil
}

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

// ReadDir lists a directory inside the user's scratchpad.
func ReadDir(root, userID, rel string) ([]fs.DirEntry, error) {
	f, err := OpenDir(root, userID, rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
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
	switch s.st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		m |= fs.ModeDir
	case unix.S_IFLNK:
		m |= fs.ModeSymlink
	}
	return m
}
