//go:build unix

package sourceconsistency

import (
	"os"
	"strconv"
	"syscall"
)

// fileIdentity names the object behind a path as device and inode, which is
// what distinguishes an in-place rewrite from a rename into place. Both are
// mutations; only the second means the bytes just read came from a file that
// no longer answers to the path.
//
// A stat that does not carry a unix Stat_t (a synthetic os.FileInfo from a
// test double or an in-memory filesystem) yields the empty string, which
// DescribeMovement reads as "no opinion".
func fileIdentity(fi os.FileInfo) string {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}

	return strconv.FormatUint(uint64(st.Dev), 10) + ":" + strconv.FormatUint(uint64(st.Ino), 10)
}

// openNoFollow makes an open of a symlink fail rather than traverse it,
// which is what keeps the symlink decision out of this reader even when a
// link is put in place between the stat that classified the path and the
// open that reads it.
const openNoFollow = syscall.O_NOFOLLOW
