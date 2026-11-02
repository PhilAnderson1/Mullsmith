//go:build unix

package scratch

import "testing"

func TestDiskFloorAllowsGrowth(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		total, free, additional uint64
		want                    bool
	}{
		{"unknown total", 0, 0, ^uint64(0), true},
		{"exact floor", 1000, 50, 0, true},
		{"below floor", 1000, 49, 0, false},
		{"growth leaves floor", 1000, 150, 100, true},
		{"growth breaches floor", 1000, 149, 100, false},
		{"round floor upward", 101, 6, 0, true},
		{"rounded floor short", 101, 5, 0, false},
		{"overflow safe", ^uint64(0), ^uint64(0), ^uint64(0), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := diskFloorAllows(tc.total, tc.free, tc.additional); got != tc.want {
				t.Fatalf("diskFloorAllows(%d, %d, %d) = %v, want %v",
					tc.total, tc.free, tc.additional, got, tc.want)
			}
		})
	}
}
