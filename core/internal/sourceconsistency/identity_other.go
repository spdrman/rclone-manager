//go:build !unix

package sourceconsistency

import "os"

// fileIdentity has no answer on a platform whose os.FileInfo carries no
// device and inode. Empty is the honest value: describeMovement reads it as
// "no opinion" and falls back to size and modification time, which loses
// the ability to distinguish a rename into place from an in-place rewrite
// and keeps the ability to detect that something moved.
func fileIdentity(os.FileInfo) string { return "" }
