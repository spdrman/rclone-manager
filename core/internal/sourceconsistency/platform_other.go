//go:build !unix

package sourceconsistency

import "os"

// fileIdentity has no answer on a platform whose os.FileInfo carries no
// device and inode. Empty is the honest value: DescribeMovement reads it as
// "no opinion" and falls back to size and modification time, which loses
// the ability to distinguish a rename into place from an in-place rewrite
// and keeps the ability to detect that something moved.
func fileIdentity(os.FileInfo) string { return "" }

// openNoFollow is zero where the platform's open has no such flag. The
// symlink boundary still holds from the other side: OSSource.Stat lstats,
// and the reader refuses to read anything the stat did not call a regular
// file. What is lost here is only the defence against a symlink appearing
// between that stat and the open, and a flag that does not exist cannot be
// emulated by this package.
const openNoFollow = 0
