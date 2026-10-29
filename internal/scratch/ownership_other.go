//go:build !unix

package scratch

// No uid/gid semantics outside unix; AdoptOwnership is a no-op there
// already because os.Geteuid() is -1, so these only need to compile.
func serviceOwnerID(string) (uid, gid int, ok bool) { return 0, 0, false }

func ownedByRoot(string) bool { return false }
