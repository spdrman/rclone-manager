//go:build unix

package capacity

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// StatPath reads the current capacity of the filesystem that contains path,
// via the POSIX statfs(2) family (unix.Statfs).
//
// This file builds for every GOOS the "unix" build-constraint pseudo-tag
// covers, which includes both platforms this project's CI actually
// exercises: linux/amd64 and linux/arm64 (the UGREEN cross-compile targets,
// CGO_ENABLED=0: unix.Statfs is a pure Go syscall wrapper with no cgo
// involved, so disabling cgo does not affect it) and darwin (local
// development). A single implementation covers both without a per-GOOS
// file: unix.Statfs_t's Blocks, Bfree and Bavail fields are uint64 on every
// platform this project targets, and while Bsize's width differs (int64 on
// Linux, uint32 on Darwin), converting either explicitly to uint64 is safe
// and portable, since block sizes are always small positive numbers.
func StatPath(path string) (Stat, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return Stat{}, fmt.Errorf("capacity: statfs %q: %w", path, err)
	}

	blockSize := uint64(st.Bsize)
	return Stat{
		TotalBytes:     blockSize * st.Blocks,
		FreeBytes:      blockSize * st.Bfree,
		AvailableBytes: blockSize * st.Bavail,
	}, nil
}
