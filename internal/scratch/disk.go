//go:build unix

package scratch

import "golang.org/x/sys/unix"

// DiskFloorOK reports whether free space above dir stays at or above the 5%
// floor (§9.7). Stat failures fail open here — per-user quota still bounds
// growth, and refusing service on a weird mount is worse than one more file.
func DiskFloorOK(dir string) bool {
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
	return total == 0 || free*20 >= total // free >= 5 %
}
