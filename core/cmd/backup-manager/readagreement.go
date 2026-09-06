package main

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/model"
)

// The five questions the read commands put to the engine, one per surface
// issue #544 names.
//
// Each one is a comparison rather than a rendering, for the reason
// readmode.go's doc gives at length: the contract cannot express several
// of the lines these commands print, so a route that re-rendered from the
// wire would answer with less than the direct route does, which is two
// surfaces disagreeing in a new place. What the engine CAN answer is
// whether it sees the same things, and that is what is asked.
//
// The projections are deliberately small and are always compared as SETS,
// both directions at once. A check that only looked for its own ids in the
// engine's answer would pass while the engine held twice as much, which is
// #535's actual shape: the engine had less than the CLI, and either way
// round is a deployment showing two worlds.
//
// Every one of them is a no-op unless the mode is engine-attached, so a
// command calls its question unconditionally and cannot forget the guard.
// An engine that answered getSystemVersion a moment ago and cannot answer
// this is a refusal, not a downgrade: the mode has already been announced
// as engine-attached, and answering from the file underneath that
// announcement would make the announcement a lie.

// agreeOnBackupSets is `sources`: does the serving process have the same
// backup sets, enabled and read-only the same way?
func (d readDecision) agreeOnBackupSets(ctx context.Context, mine []string) error {
	if !d.attached() {
		return nil
	}
	answer, err := d.client.ListBackupSets(ctx)
	if err != nil {
		return d.unanswered("which backup sets it has", err)
	}
	theirs := make([]string, 0, len(answer.BackupSets))
	for _, bs := range answer.BackupSets {
		theirs = append(theirs, describeBackupSet(bs.ID, bs.Disabled, bs.ReadOnly))
	}
	if differ(mine, theirs) {
		return disagreement(d.engine, "which backup sets this deployment has", mine, theirs)
	}
	return nil
}

// describeBackupSet is the one spelling both sides of that comparison use,
// so the two can never be built differently.
func describeBackupSet(id string, disabled, readOnly bool) string {
	status := "enabled"
	if disabled {
		status = "disabled"
	}
	if readOnly {
		status += ",read_only"
	}
	return id + " " + status
}

// agreeOnArtifacts is `artifacts`: does the serving process's journal hold
// the same backups, in the same states?
//
// setID narrows both sides to one backup set when the command was given a
// filter that names one, which is the only shape GET /backups can filter
// on; an empty setID compares the whole listing. The unfiltered listing
// includes the artifacts of removed backup sets on both sides (#391), so
// the comparison covers the rows `artifacts` marks as ungoverned rather
// than quietly leaving them out.
func (d readDecision) agreeOnArtifacts(ctx context.Context, setID string, mine []string) error {
	if !d.attached() {
		return nil
	}
	answer, err := d.client.ListArtifacts(ctx, setID)
	if err != nil {
		return d.unanswered("which backups it holds", err)
	}
	theirs := make([]string, 0, len(answer.Artifacts))
	for _, a := range answer.Artifacts {
		theirs = append(theirs, a.ID+" "+a.State)
	}
	about := "which backups this deployment holds"
	if setID != "" {
		about += " in " + setID
	}
	if differ(mine, theirs) {
		return disagreement(d.engine, about, mine, theirs)
	}
	return nil
}

// agreeOnArtifact is `artifacts <source/set/name>`: the same one backup,
// in the same state.
func (d readDecision) agreeOnArtifact(ctx context.Context, id model.ArtifactID, mine string) error {
	if !d.attached() {
		return nil
	}
	a, err := d.client.GetArtifact(ctx, id.Set.Source, id.Set.Set, id.Name)
	if err != nil {
		return d.unanswered("what state "+id.String()+" is in", err)
	}
	theirs := a.ID + " " + a.State
	if differ([]string{mine}, []string{theirs}) {
		return disagreement(d.engine, "what state "+id.String()+" is in", []string{mine}, []string{theirs})
	}
	return nil
}

// agreeOnHealth is `status`: the same backup sets, each in the same FR-24
// state.
//
// State only, not the counts and not the ages. An age is derived from a
// timestamp at the moment of rendering, so two processes asked a
// round trip apart legitimately differ in the last digits of one, and a
// check that compared those would fire on nothing at all. The state is the
// verdict an operator reads and the one the exit code is computed from.
func (d readDecision) agreeOnHealth(ctx context.Context, mine []string) error {
	if !d.attached() {
		return nil
	}
	answer, err := d.client.SystemHealth(ctx)
	if err != nil {
		return d.unanswered("how healthy this deployment is", err)
	}
	theirs := make([]string, 0, len(answer.BackupSets))
	for _, bs := range answer.BackupSets {
		theirs = append(theirs, bs.BackupSetID+" "+bs.State)
	}
	if differ(mine, theirs) {
		return disagreement(d.engine, "which backup sets are healthy", mine, theirs)
	}
	return nil
}

// agreeOnRetention is `retention`: for one backup set, the same verdict on
// the same backups.
//
// This is the one question here with a side effect, and it is worth naming
// because the read set is not free of them (mode.go's own closing note
// says as much about `restore`): GET .../retention/preview issues a
// plan_id and the engine keeps that plan in a bounded, expiring cache so
// that a later apply can be checked against a plan somebody reviewed.
// Nothing is deleted, nothing is written to the journal, and this command
// never applies anything -- `retention` previews in both its modes by
// design -- but the engine does hold one more plan for a while.
func (d readDecision) agreeOnRetention(ctx context.Context, set model.BackupSetID, mine []string) error {
	if !d.attached() {
		return nil
	}
	plan, err := d.client.PreviewRetention(ctx, set.Source, set.Set)
	if err != nil {
		return d.unanswered("what its retention policy would do to "+set.String(), err)
	}
	theirs := make([]string, 0, len(plan.Verdicts))
	for _, v := range plan.Verdicts {
		theirs = append(theirs, v.Artifact+" "+v.Action)
	}
	if differ(mine, theirs) {
		return disagreement(d.engine, "what retention would do to "+set.String(), mine, theirs)
	}
	return nil
}

// unanswered is an engine that took the question and could not answer it.
//
// Deliberately not a downgrade to the file. By the time any of the five
// above runs, the mode has been announced as engine-attached, and printing
// the file's answer under that announcement would tell an operator the
// serving process agreed when nobody asked it anything.
func (d readDecision) unanswered(about string, err error) error {
	return fmt.Errorf("the process serving this deployment (state database %s) was asked %s and did not answer, so nothing was printed rather than an answer it has not agreed to: %w", d.engine.StateDatabase, about, err)
}
