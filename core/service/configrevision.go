package service

import (
	"github.com/spdrman/backupd/core/internal/config"
)

// Issue #544, Phase 2 of #536: letting a process that is NOT this
// deployment's engine ask whether the configuration it just loaded is the
// one the engine is serving.
//
// ConfigRevision (service.go) already answers that from the engine's side,
// and computeConfigRevision is how it is derived, but both hang off a
// *BackupService: a caller has to have opened the whole service, with its
// journal and its startup sequence, before it can ask. The CLI's read
// commands have not. `sources` loads a configuration and nothing else at
// all, and the rest go through internal/app.Service, which is built from
// an already-loaded *config.Config and has no notion of a revision.
//
// So the derivation is exported here as what it always was: a pure
// function of configuration content. Same input, same answer, in whichever
// process asks, which is the whole property the comparison rests on.
//
// # Why a CLI wants it
//
// Because #535 was two processes disagreeing about the configuration and
// neither one noticing. The engine read config.yaml when it started and
// has no watcher on it; a CLI reads it now. Every read surface this binary
// prints is derived from one of those two answers, and when they differ
// the operator is shown a world the Web UI does not have. Comparing this
// value against the engine's own GET /system/version is what lets a read
// command find that out before it prints, for the price of one request and
// no interpretation of what changed.

// ConfigRevisionOf identifies the exact configuration content cfg holds,
// with the same derivation and therefore the same value a BackupService
// built from that configuration reports through ConfigRevision.
//
// It changes if, and only if, the configuration's content changes, so two
// processes that loaded byte-for-byte equivalent configurations agree, and
// any difference at all -- a source added, a field edited, a retention
// tier renamed -- makes them disagree. That is a stronger comparison than
// any list of fields a caller could check by hand, and it covers the
// fields no API shape carries.
//
// cfg must be validated (config.LoadAndValidate, which is what both
// OpenConfigAndJournal and Open use). Validate fills defaults in place, so
// a revision taken before it and one taken after it are revisions of
// different content and would disagree for a reason that is not the
// deployment's.
func ConfigRevisionOf(cfg *config.Config) string {
	return computeConfigRevision(cfg)
}
