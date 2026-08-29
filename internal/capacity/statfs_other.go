//go:build !unix

package capacity

import (
	"fmt"
	"runtime"
)

// StatPath is the non-unix fallback: this project ships and is CI-tested
// only for linux/amd64, linux/arm64 and darwin (see statfs_unix.go), so
// there is no statfs-equivalent wired up for anything else. It fails loudly
// rather than silently reporting a fabricated capacity reading, consistent
// with this package's rule that a condition it cannot honestly assess is
// reported, never guessed at.
func StatPath(path string) (Stat, error) {
	return Stat{}, fmt.Errorf("capacity: filesystem capacity checks are not implemented on %s", runtime.GOOS)
}
