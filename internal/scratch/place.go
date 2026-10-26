// Collision-free file placement (spec §8, disk-as-truth): the one way a
// host-side temp file lands inside a user's tree WITHOUT ever overwriting
// what is already there. Uploads and files_share keep the real filename,
// so two writers racing for "report.pdf" must produce "report (1).pdf"
// atomically — a Stat-then-Rename check would reopen the file under a
// third writer between the two calls.
package scratch

import (
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// PlaceNoReplace moves the file at tempHostPath (an absolute host path —
// normally a ".upload-*"/".share-*" temp inside the scratch root, hence
// the SAME filesystem as the destination) to rel inside the user's tree,
// refusing to overwrite: if the destination exists it returns ErrExists
// and the temp stays intact for a retry under another name (BumpName).
//
// The destination directory is reached through the same fd-pinned chain as
// every other operation (§9.7), and the landing rename is
// renameat2(AT_RENAME_NOREPLACE) — collision-proof and atomic, with no
// window to swap the target. This adds Linux ≥ 3.15 as a hard requirement,
// far below the user-namespaces floor the code-execution jail already
// sets.
func PlaceNoReplace(root, userID, rel, tempHostPath string) error {
	or, err := openParent(root, userID, rel)
	if err != nil {
		return err
	}
	defer or.parent.Close()
	if or.final == "" {
		return fmt.Errorf("%w: cannot place onto the user root", ErrPathUnsafe)
	}
	if err := unix.Renameat2(unix.AT_FDCWD, tempHostPath,
		int(or.parent.Fd()), or.final, unix.RENAME_NOREPLACE); err != nil {
		return mapOpenErr(err)
	}
	return nil
}

// BumpName returns the nth collision alternative of a filename, the way
// desktop OSes do when a name is taken: "report.pdf" at n=1 becomes
// "report (1).pdf", at n=2 "report (2).pdf". The bump lands before the
// extension so the type suffix survives; names without an extension (and
// dotfiles, which the upload sanitizer already renames) append plainly.
// Callers loop PlaceNoReplace with n = 0, 1, 2… — the loop is racy-proof
// because each try is itself an atomic NOREPLACE rename.
func BumpName(name string, n int) string {
	if n <= 0 {
		return name
	}
	ext := ""
	if dot := strings.LastIndexByte(name, '.'); dot > 0 {
		ext = name[dot:]
	}
	return name[:len(name)-len(ext)] + " (" + strconv.Itoa(n) + ")" + ext
}
