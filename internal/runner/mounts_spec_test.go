package runner

import "testing"

// The root-litter contract, pinned without executing: root RO, work+
// artifacts rw, uploads ro, and the parent bind ordered before the
// overlays.
func argPair(args []string, a, b string) int {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == a && args[i+1] == b {
			return i
		}
	}
	return -1
}

// The root-litter contract, pinned without executing: root RO, work+
// artifacts rw, uploads ro, and the parent bind ordered before the overlays.
func TestBwrapArgsMountSpec(t *testing.T) {
	args := bwrapArgs("/sp", "/sp/work", "/usr/bin/sh", false)

	if argPair(args, "--ro-bind", "/sp") < 0 {
		t.Error("scratchpad root must be --ro-bind")
	}
	if argPair(args, "--bind", "/sp/work") < 0 {
		t.Error("work dir must be overlaid --bind (read-write)")
	}
	if argPair(args, "--ro-bind", "/sp/uploads") < 0 {
		t.Error("uploads must be a hard --ro-bind overlay")
	}
	if argPair(args, "--bind", "/sp/artifacts") < 0 {
		t.Error("artifacts must be a hard --bind overlay (finished output lands there)")
	}
	// A stray writable bind of the ROOT itself would undo everything.
	if i := argPair(args, "--bind", "/sp"); i >= 0 {
		t.Error("root must never be bound read-write")
	}
	// Ordering: the root bind must precede the overlays so they stack
	// inside it, not get covered by it.
	rootAt := argPair(args, "--ro-bind", "/sp")
	for _, overlay := range [][2]string{{"--bind", "/sp/work"}, {"--ro-bind", "/sp/uploads"}, {"--bind", "/sp/artifacts"}} {
		if argPair(args, overlay[0], overlay[1]) < rootAt {
			t.Errorf("overlay %s %s applied before the root bind", overlay[0], overlay[1])
		}
	}
	// HOME must ride the writable dir (the root cannot accept dotfiles
	// or caches — that was the litter).
	if argPair(args, "--setenv", "HOME") < 0 || args[argPair(args, "--setenv", "HOME")+2] != "/scratchpad/work" {
		t.Error("HOME must point at the writable work dir")
	}
}
