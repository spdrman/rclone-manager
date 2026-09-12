// Package backupengine is the only package in this repository that imports
// Kopia, and it is a feasibility spike (#791, EPIC #779 phase 0) rather
// than a shipped feature.
//
// It exists to answer one question with running code: can a file that lives
// on a remote this product reaches through rclone be snapshotted by Kopia
// without ever being staged locally and without anyone pretending the
// remote is seekable? The answer, and the measurements behind it, are in
// docs/adr/0007-rclone-streaming-source-to-kopia.md.
//
// # The boundary
//
// Everything Kopia is stays inside this package. The types that cross its
// edge are StreamSource (an io.ReadCloser factory, a name and a modtime),
// Config, Snapshot and error. No Kopia type appears in any signature here,
// so nothing in api/v1, the CLI, the UI or the catalog can come to depend
// on Kopia's object model, and replacing the engine is a change to this
// package rather than a change to the product.
//
// That is the same shape internal/transport/rclone has for rclone, and for
// the same reason (FR-3, FR-4): upstream churn is confined to one adapter.
//
// # What the spike deliberately does not do
//
// It does not stage. It does not buffer the file. It does not implement
// Seek, and the entry it hands Kopia is checked by a test for NOT
// satisfying fs.File, because Kopia's own newDirEntry type-switches
// `case fs.File, fs.StreamingFile` with fs.File first: an entry that grew
// an Open method would silently leave the streaming path.
//
// It also does not resume. A stream that breaks is re-opened from byte
// zero, at most Config.MaxAttempts times, and a run that never completes
// saves no manifest, so a torn upload is never reachable as a snapshot.
package backupengine
