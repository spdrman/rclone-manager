package service

import (
	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
)

// This file is the shared tail of every configuration write: once the new
// config.yaml is durably on disk, this is what makes the running process
// agree with it.
//
// It is one function, and being one function is the whole point. Every
// write path (create, update, remove, enable, retention, settings, medium
// edits) has to rebuild the app.Service, carry alerting across, swap the
// {inner, revision} pair atomically and reconcile the removal holds, in
// that order. That sequence used to be typed out at each write site, with
// a comment at each copy saying it followed one of the others exactly.
// They did follow each other, which is exactly how the eighth copy came
// to be the only one with a step the other seven also needed.
//
// Nothing in here returns an error, and nothing in here is allowed to
// acquire one. By the time it runs, the file on disk has already changed,
// so there is no failure this could report that would not leave the
// process and its configuration disagreeing with each other and the
// caller unable to do anything about either.

// adoptConfig is the tail every configuration write shares once the file
// on disk holds the change: build the new *app.Service over cfg, carry
// alerting across, swap it in as one atomic {inner, revision} pair, and
// reconcile the removal holds against what cfg names. It returns the
// revision it stored, for the one caller (CreateBackupSet) that submits a
// run against it.
//
// Called with configMu held, after writeConfigBytesAtomically has
// succeeded and applyValidators has run, so nothing in here can fail and
// nothing in here is allowed to: the write is durable by now and a swap
// that raised an error would leave the file and the process disagreeing.
//
// It used to be written out by hand at every write site, each copy with
// a comment saying it followed one of the others exactly. They did, and
// that was the problem: the eighth copy (RemoveBackupSet) was the first
// one with a step the others needed too (see the removal holds below),
// and seven files is not a place to keep a step. One place is.
//
// # Alerting across the swap
//
// Alerting is re-decided from the configuration file the caller just
// re-read, then carried across the swap. This is the one moment an
// edited alerts.enabled can take effect in a running process, so it is
// the one moment it must not be ignored: an administrator who set
// alerts.enabled: false and then added a backup set kept getting
// notified until the next restart, and one who turned it on stayed
// silent, while repeated_failure_threshold from the same block did
// hot-reload. AdoptAlerts re-reads the opt-in and carries the dispatcher
// only if it is still on, because the dispatcher holds which conditions
// are currently firing (internal/alert's de-duplication state) and
// rebuilding it would re-alert every still-unresolved condition the next
// time a cycle ran, purely because somebody changed the configuration.
// When it declines (alerting was off before this reload, or has just been
// turned off), the question is settled from b.alertSink instead, which is
// what makes turning alerting ON take effect here too.
//
// # Removal holds
//
// A removal holds its id permanently, because a cycle in flight can run
// for hours on the pre-removal snapshot and the hold is the only thing
// that reaches it (edithold.go). The one event that makes that hold
// wrong is the id being configured again. CreateBackupSet is the obvious
// route back and was the only one considered; it is not the only one.
// config.yaml is hand-edited throughout the documentation, every write
// path re-reads it from disk for exactly that reason, and so any reload
// at all can bring a removed set back: restore the file by hand, toggle
// an unrelated set, and the restored one is configured, enabled, and
// was held until the process restarted, which is a backup silently not
// happening with nothing anywhere saying why. The invariant is "a set
// the configuration names is never removal-held", and this is the one
// place every reload passes through, so this is where it is kept.
//
// The reconcile runs after the swap, in the same order CreateBackupSet
// used to drop the hold, so a set brought back is live in the new
// snapshot by the time a cycle is allowed to reach it.
func (b *BackupService) adoptConfig(cfg *config.Config) string {
	// prevInner is read once, before the swap, purely to carry the
	// already-wired Transport and alert state forward. The caller is the
	// only writer of b.state while configMu is held, so this read cannot
	// itself race the swap below.
	prevInner := b.state.Load().inner
	newInner := app.New(cfg, b.journal, prevInner.Transport, b.logger)
	if !newInner.AdoptAlerts(prevInner.Alerts) && b.alertSink != nil {
		newInner.EnableAlerts(sinkAdapter{sink: b.alertSink})
	}
	revision := computeConfigRevision(cfg)
	b.state.Store(&configState{inner: newInner, revision: revision})
	b.holds.forgetRemovedNamedIn(cfg)
	return revision
}
