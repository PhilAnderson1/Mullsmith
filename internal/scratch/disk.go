//go:build unix

package scratch

import "golang.org/x/sys/unix"

// DiskFloorAllows reports whether consuming additional bytes would leave the
// filesystem above its 5% free-space floor (§9.7). Stat failures fail open
// here — per-user quota still bounds growth, and refusing service on a weird
// mount is worse than one more file.
func DiskFloorAllows(dir string, additional int64) bool {
	if additional < 0 {
		return false
	}
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return true
	}
	if st.Bsize <= 0 {
		return true // nonsensical statfs result: fail open, same policy
	}
	// #nosec G115 -- Bsize clamped positive above; Blocks/Bavail are counts
	total := st.Blocks * uint64(st.Bsize)
	free := st.Bavail * uint64(st.Bsize)
	return diskFloorAllows(total, free, uint64(additional))
}

// DiskFloorOK is the existing zero-growth check used by paths whose eventual
// size is not yet known (for example, an incoming upload stream).
func DiskFloorOK(dir string) bool { return DiskFloorAllows(dir, 0) }

func diskFloorAllows(total, free, additional uint64) bool {
	if total == 0 {
		return true
	}
	// Ceiling division: the floor is at least 5%, including filesystems whose
	// byte count is not exactly divisible by twenty. Written as subtraction
	// instead of free >= floor+additional to avoid overflow.
	floor := total / 20
	if total%20 != 0 {
		floor++
	}
	return free >= floor && additional <= free-floor
}
