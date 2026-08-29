// Package migrations embeds the SQLite schema migrations for the lifecycle
// journal (FR-9, docs/EPIC.md) as version-controlled SQL files.
//
// Embedding them into the binary, rather than reading migrations/*.sql off
// disk at runtime, means the migration runner in internal/state never has to
// guess where it is running from. The UGREEN NAS deployment starts this
// binary from whatever working directory its init system chooses, and a
// runtime file lookup relative to the executable or the cwd would make the
// journal's schema-version check no more reliable than the filesystem layout
// on the day it runs, exactly the kind of thing FR-9 exists to rule out.
package migrations

import "embed"

// FS holds every *.sql file in this directory, keyed by its bare filename
// (no directory prefix, since the embed pattern below only matches files
// directly in this package's directory).
//
//go:embed *.sql
var FS embed.FS
