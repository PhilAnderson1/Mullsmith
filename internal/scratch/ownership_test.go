//go:build unix

package scratch

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestServiceOwnerIDWalksUp(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("test assumes an unprivileged runner: as root, nothing is 'not root-owned'")
	}
	tmp := t.TempDir() // exists, owned by the test user (non-root)
	// A scratchpad root that does not exist yet: the walk must fall back
	// to the nearest existing non-root ancestor.
	uid, gid, ok := serviceOwnerID(filepath.Join(tmp, "scratchpad"))
	if !ok {
		t.Fatal("no owner found next to an existing non-root directory")
	}
	st, err := os.Stat(tmp)
	if err != nil {
		t.Fatal(err)
	}
	rst, isStat := st.Sys().(*syscall.Stat_t)
	if !isStat || uid != int(rst.Uid) || gid != int(rst.Gid) {
		t.Errorf("owner = %d/%d, want %d/%d", uid, gid, rst.Uid, rst.Gid)
	}
	// A scratchpad root that already exists is authoritative — its own
	// owner wins over the parent's.
	root := filepath.Join(tmp, "scratchpad")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	uid, gid, ok = serviceOwnerID(root)
	st, _ = os.Stat(root)
	rst = st.Sys().(*syscall.Stat_t)
	if !ok || uid != int(rst.Uid) || gid != int(rst.Gid) {
		t.Errorf("existing root: owner = %d/%d, want %d/%d", uid, gid, rst.Uid, rst.Gid)
	}
}

func TestAdoptOwnershipNoOpForNonRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root-run behaviour cannot be asserted without wrecking the test tree")
	}
	root := newRoot(t)
	if err := EnsureUserDirs(root, testULID); err != nil {
		t.Fatal(err)
	}
	if err := AdoptOwnership(root, testULID); err != nil {
		t.Errorf("non-root AdoptOwnership = %v, want nil no-op", err)
	}
	dir := filepath.Join(root, testULID)
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st := info.Sys().(*syscall.Stat_t); int(st.Uid) != os.Getuid() {
		t.Errorf("tree owner changed to %d under a non-root caller", st.Uid)
	}
}
