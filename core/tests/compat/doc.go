// Package compat is EPIC E's FR-35 compatibility gate: the checkable form
// of "a deployment upgraded with a medium-free config shows zero
// behavioral difference".
//
// # What FR-35 actually claims
//
// docs/EPIC-E-alternative-storage.md, FR-35:
//
//	A configuration with no storage_mediums key and no medium key on any
//	tier SHALL behave byte for byte as today: identical validation
//	outcomes, identical retention verdicts, identical API responses except
//	for additive fields, identical CLI output except for additive columns
//	that render only when a non-local placement exists.
//
// Every clause of that is a surface an operator can observe, so this
// package observes them: it drives each one against a medium-free
// deployment, writes down exactly what came back, and compares that to a
// corpus checked in beside it. The corpus was captured before any of EPIC
// E's phase 2 landed. A difference is therefore either a behavior change
// EPIC E was not allowed to make, or a behavior change somebody meant to
// make and has to say so out loud by regenerating the corpus.
//
// # Why the corpus, rather than hand-written expectations
//
// Hand-written expectations say what the author believed the product did.
// A captured corpus says what it did. The difference matters most exactly
// where this gate matters most: nobody hand-writes 26 artifact columns
// correctly from memory, and a wrong expectation that happens to match a
// wrong implementation is a gate that certifies the bug.
//
// # Why the records come out of a real migrated database
//
// The verdict cells could have been computed from in-memory fixtures, and
// they would have been shorter and faster. They would also have been
// unable to fail for the one reason this gate exists: the planted
// violation named in the spec's section 4 table is "a migration variant
// that rewrites retention_tier during backfill", and a verdict computed
// from a fixture the migration never touched cannot notice that. So every
// record cell here is seeded through the public journal API into a real
// SQLite file, every migration this binary carries is applied to it, and
// the records are read back out before anything is decided from them.
// That is the difference between a suite that would catch the planted
// violation and one that only looks like it would.
//
// # Determinism, and the one place it is bought rather than assumed
//
// Every cell that can pin its clock does: the retention and prune verdicts
// are computed at a fixed instant, and every seeded record carries an
// absolute timestamp, so the corpus is stable whatever day it runs on.
//
// The CLI has no clock seam (internal/app.Service has a Now func, but
// backup-manager exposes no flag that reaches it), so `retention
// --dry-run` cannot be pinned from outside the process. Rather than
// normalize the verdicts away, which would have left a cell that certifies
// nothing, that one command runs against a second, separately seeded
// deployment whose chain is daily-only and whose records sit at whole-day
// offsets far from any window boundary. Those verdicts are the same on
// every calendar day, so the cell is deterministic without being empty.
// See captureCLIRetention.
package compat
