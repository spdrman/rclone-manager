package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// Issue #662 at the surface the operator actually met it on: `rbm` in a
// terminal.
//
// Every case here is a command that was really typed on the NAS, with the
// output and the exit code the issue records. The internal-package tests
// (internal/lifecycle, internal/reconcile, internal/app) pin the mechanism;
// these pin what a person sees, because "the product has no way out" is a
// claim about the CLI's vocabulary, not about a Go function's return value.
//
// The measured session, condensed:
//
//	rbm status
//	  cicd-pipeline/var-backups: FAILING
//	    a FAILED artifact has no retry scheduled and needs intervention
//	    current transfers: 0, pending deletes: 0, failures: 3
//	rbm fetch --backup-set … --dry-run
//	  dpkg.diversions.5.gz   294 bytes  (already known)      [x28]
//	  28 object(s) on the remote
//	rbm validate <artifact>
//	  rbm: app: validate: … is FAILED, not a durable restore point (…)
//	rbm quarantine reinstate <artifact>
//	  rbm: app: artifact is not quarantined: … is FAILED
//	rbm retry <artifact>
//	  … re-entering the pipeline (FAILED -> DISCOVERED)   then FAILED again
//
// Exit codes are asserted alongside the text, and separately: a refusal
// that exits 0 is its own defect and would be invisible to any assertion
// that only read stdout.
//
// Nothing here pins wording. Each case asks for a property -- the output
// names the stuck artifacts, or names a verb that can act, or reports that
// a plan will not touch them -- and any sentence satisfying it passes.

var epoch662CLI = time.Date(2026, 9, 7, 1, 18, 48, 0, time.UTC)

// payload662CLI is 294 bytes, the size of dpkg.diversions.5.gz in the
// issue.
var payload662CLI = func() string {
	b := make([]byte, 294)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return string(b)
}()

// sha256OfNothingCLI is the checksum the empty read-back left over that
// 294-byte file, in the journal and in the sidecar manifest alike.
const sha256OfNothingCLI = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// deadEnd662 is the deployment #662 was filed from, in miniature: one
// read-only backup set, one artifact whose good 294-byte copy sits at its
// final name under a journal row that records it as zero bytes, and the
// artifact quarantined.
type deadEnd662 struct {
	configPath string
	artifact   model.ArtifactID
	final      string
	diskSize   int64
	diskHash   string
}

// stage662DeadEnd writes the config, puts the real bytes both on the
// "remote" and at the local final name, and walks the journal row down the
// real lifecycle graph to QUARANTINED carrying the empty read-back's
// numbers.
//
// The remote copy is real and complete, which matters: it is what makes
// `retry` a sensible thing for an operator to try, and it is what makes the
// FR-12 collision the thing that stops it rather than a missing source.
func stage662DeadEnd(t *testing.T, honestRecord bool) deadEnd662 {
	t.Helper()
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote")
	localDir := filepath.Join(dir, "local")
	for _, d := range []string{remoteDir, localDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}

	const name = "dpkg.diversions.5.gz"
	if err := os.WriteFile(filepath.Join(remoteDir, name), []byte(payload662CLI), 0o644); err != nil {
		t.Fatalf("WriteFile (remote): %v", err)
	}
	final := filepath.Join(localDir, name)
	if err := os.WriteFile(final, []byte(payload662CLI), 0o644); err != nil {
		t.Fatalf("WriteFile (local final): %v", err)
	}
	sum := sha256.Sum256([]byte(payload662CLI))
	diskHash := hex.EncodeToString(sum[:])
	diskSize := int64(len(payload662CLI))
	if diskSize == 0 {
		t.Fatal("fixture wrote an empty file; every assertion below would be about the wrong thing")
	}

	dbPath := filepath.Join(dir, "state.db")
	configPath := filepath.Join(dir, "config.yaml")
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + dbPath + "\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: postgres-primary\n" +
		"        read_only: true\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: " + remoteDir + "\n" +
		"        local_path: " + localDir + "\n" +
		"        include:\n" +
		"          - \"*.gz\"\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n" +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile (config): %v", err)
	}

	ctx := context.Background()
	j, err := state.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = j.Close() }()

	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	artifact, err := model.NewArtifactID(set, name)
	if err != nil {
		t.Fatalf("NewArtifactID: %v", err)
	}

	remoteSize := diskSize
	if _, err := j.Discover(ctx, artifact, name+"-discover", name,
		state.RemoteIdentity{Size: &remoteSize}, epoch662CLI); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	// The empty read-back's record: zero bytes transferred, the sha256 of
	// nothing recorded at VERIFIED. REMOTE_RETAINED is the read-only
	// lineage the field artifact was on, and QUARANTINED is where
	// reconciliation put it (defect 2).
	recordedSize := int64(0)
	recordedHash := sha256OfNothingCLI
	if honestRecord {
		recordedSize = diskSize
		recordedHash = diskHash
	}

	steps := []struct {
		from, to  lifecycle.State
		localPath *string
		transfer  *state.TransferResult
		hashes    *state.HashUpdate
	}{
		{from: lifecycle.Discovered, to: lifecycle.Transferring},
		{from: lifecycle.Transferring, to: lifecycle.Transferred, transfer: &state.TransferResult{BytesTransferred: recordedSize}},
		{from: lifecycle.Transferred, to: lifecycle.Verifying},
		{from: lifecycle.Verifying, to: lifecycle.Verified, hashes: &state.HashUpdate{Alg: "sha256", Hash: recordedHash}},
		{from: lifecycle.Verified, to: lifecycle.Committing},
		{from: lifecycle.Committing, to: lifecycle.Committed, localPath: &final},
		{from: lifecycle.Committed, to: lifecycle.RemoteRetained},
		{from: lifecycle.RemoteRetained, to: lifecycle.Quarantined},
	}
	for i, s := range steps {
		if _, err := j.RecordTransition(ctx, state.Transition{
			Artifact:   artifact,
			Key:        fmt.Sprintf("662-cli-%d-%s", i, s.to),
			From:       string(s.from),
			To:         string(s.to),
			LocalPath:  s.localPath,
			Transfer:   s.transfer,
			Hashes:     s.hashes,
			Detail:     "issue #662 fixture",
			OccurredAt: epoch662CLI.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("fixture %s -> %s: %v", s.from, s.to, err)
		}
	}

	fx := deadEnd662{configPath: configPath, artifact: artifact, final: final, diskSize: diskSize, diskHash: diskHash}
	if got := stateOf(t, configPath, artifact); got != string(lifecycle.Quarantined) {
		t.Fatalf("precondition: fixture is %s, want QUARANTINED", got)
	}
	return fx
}

// runCLI662 runs one command and hands back its exit code and everything it
// printed, both streams, because a refusal that exits 0 and a refusal
// nobody can read are two different defects and this file cares about both.
//
// FR-23's newline-delimited JSON event stream is stripped out of stdout.
// Every command opens with two structured startup records, and leaving them
// in would do two bad things at once: bury the sentence a transcript is
// meant to show, and let a search for an operator-facing remedy be
// satisfied by a machine-readable field nobody reads in a terminal.
func runCLI662(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	stderr = captureStderr(t, func() {
		stdout = captureStdout(t, func() { code = run(args) })
	})
	return code, withoutEventStream662(stdout), withoutEventStream662(stderr)
}

// withoutEventStream662 drops the FR-23 JSON lines, leaving what a person
// reading the terminal actually sees.
func withoutEventStream662(s string) string {
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "{\"time\":") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// drive662ToFailed does what the operator did first: retries the
// quarantined artifact and runs the set, which walks it into the FR-12
// collision and leaves it FAILED.
func (fx deadEnd662) drive662ToFailed(t *testing.T) {
	t.Helper()
	if code, _, err := runCLI662(t, "quarantine", "--config", fx.configPath, "retry", fx.artifact.String()); code != exitOK {
		t.Fatalf("quarantine retry = %d, want %d; stderr: %s", code, exitOK, err)
	}
	runCLI662(t, "fetch", "--config", fx.configPath, "--backup-set", "production/postgres-primary")
	if got := stateOf(t, fx.configPath, fx.artifact); got != string(lifecycle.Failed) {
		t.Fatalf("after retry + fetch the artifact is %s, want FAILED; this test's whole subject is the FAILED-after-collision state", got)
	}
}

// TestIssue662_CLIARefusedArtifactIsToldWhatToDoInstead is defect 3 in the
// terminal.
//
// It walks the verbs an operator has, in the order the issue records, and
// asks for one thing: that the session ends either with the artifact back
// at a durable restore point, or with at least one refusal naming a verb
// that would get it there. That disjunction is the issue's own fix
// requirement -- "every state reachable by a documented verb must have an
// exit ... or the collision refusal must name the verb that resolves it" --
// so a fix that widens `validate`, one that keeps `reinstate` reachable,
// and one that merely adds the missing sentence all pass.
//
// The control case is the identical walk with defect 1 absent: the record
// describes the file, so `quarantine reinstate` succeeds on the second
// command and the operator is done. Without it, "no verb worked" would be
// satisfiable by a fixture no verb could ever have worked on.
func TestIssue662_CLIARefusedArtifactIsToldWhatToDoInstead(t *testing.T) {
	cases := []struct {
		name         string
		honestRecord bool
	}{
		{name: "an empty record over a good file, which is #662's state", honestRecord: false},
		{name: "control: the same artifact with a record that describes its file", honestRecord: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := stage662DeadEnd(t, tc.honestRecord)
			id := fx.artifact.String()

			// The order an operator works in: look, then trust, then
			// check, then re-fetch, then whatever FAILED still offers.
			attempts := [][]string{
				{"quarantine", "--config", fx.configPath, "revalidate", id},
				{"quarantine", "--config", fx.configPath, "reinstate", id},
				{"validate", "--config", fx.configPath, id},
				{"quarantine", "--config", fx.configPath, "retry", id},
				{"fetch", "--config", fx.configPath, "--backup-set", "production/postgres-primary"},
				{"retry", "--config", fx.configPath, id},
				{"fetch", "--config", fx.configPath, "--backup-set", "production/postgres-primary"},
				{"quarantine", "--config", fx.configPath, "reinstate", id},
				{"validate", "--config", fx.configPath, id},
				{"reconcile", "--config", fx.configPath},
			}

			var transcript []string
			toldWhatToDo := false
			for _, args := range attempts {
				code, out, errOut := runCLI662(t, args...)
				said := out + errOut
				transcript = append(transcript, fmt.Sprintf("$ rbm %s\n      exit %d\n    %s",
					strings.Join(stripConfig662(args), " "), code, strings.TrimSpace(indent662(said))))
				if namesAVerb662(said) {
					toldWhatToDo = true
				}
				if durableState662(stateOf(t, fx.configPath, fx.artifact)) {
					return // resolved, which is the best outcome there is.
				}
			}
			if len(transcript) != len(attempts) {
				t.Fatalf("only %d of %d commands ran; this test proves nothing", len(transcript), len(attempts))
			}

			if !toldWhatToDo {
				t.Errorf(
					"#662 defect 3: every verb refuses this artifact and none of them names one that would not.\n"+
						"  artifact: %s, %s\n"+
						"  on disk:  %s is %d bytes, sha256 %s, intact throughout\n\n  %s\n\n"+
						"An operator is told what this is not -- \"not a durable restore point\", \"not "+
						"quarantined\" -- and never once what to do. On a NAS there is no shell to finish the "+
						"job by hand, which is the whole premise of the product.",
					fx.artifact, stateOf(t, fx.configPath, fx.artifact),
					fx.final, fx.diskSize, fx.diskHash,
					strings.Join(transcript, "\n  "))
			}
		})
	}
}

// TestIssue662_CLIRetryMustNotMakeReinstateUnreachable is the one-way door where
// an operator walks through it.
//
// `rbm quarantine reinstate` is the verb for "I have looked at the file, it
// is good, believe it again". Before `rbm quarantine retry` it accepts the
// artifact; afterwards it refuses on state grounds alone, and the artifact
// is no closer to resolution than before. The natural first move is a
// one-way door out of the only state that offers the verb that fits.
//
// The control is the same command on the same fixture before the retry, and
// it is what makes the case discriminate: an assertion that reinstate
// "works" would be satisfied by an implementation where it never refused
// anything.
func TestIssue662_CLIRetryMustNotMakeReinstateUnreachable(t *testing.T) {
	fx := stage662DeadEnd(t, false)
	id := fx.artifact.String()

	// Control: from QUARANTINED the verb accepts the artifact and answers
	// with a verdict. It exits 1 because the recorded hash is the sha256
	// of nothing (defect 1) and so the check cannot pass, but it looked.
	code, out, errOut := runCLI662(t, "quarantine", "--config", fx.configPath, "reinstate", id)
	before := out + errOut
	if strings.Contains(before, "not quarantined") {
		t.Fatalf("control: reinstate already refuses a QUARANTINED artifact on state grounds (exit %d):\n%s\n"+
			"there is no forfeiture left for this test to measure", code, before)
	}
	if !strings.Contains(before, "checked=") {
		t.Fatalf("control: reinstate printed no verdict at all for a QUARANTINED artifact (exit %d):\n%s", code, before)
	}

	fx.drive662ToFailed(t)

	afterCode, afterOut, afterErr := runCLI662(t, "quarantine", "--config", fx.configPath, "reinstate", id)
	after := afterOut + afterErr

	if durableState662(stateOf(t, fx.configPath, fx.artifact)) {
		return
	}
	if strings.Contains(after, "not quarantined") {
		t.Errorf(
			"#662 defect 3, the one-way door: `rbm quarantine retry` made `rbm quarantine reinstate` unreachable "+
				"and resolved nothing.\n"+
				"  before retry (exit %d): %s\n"+
				"  after  retry (exit %d): %s\n"+
				"  artifact is now %s; %s is still %d bytes, sha256 %s\n"+
				"Retry is the first thing an operator reaches for and it is the only exit from the one state that "+
				"offers the verb this situation needs.",
			code, strings.TrimSpace(before), afterCode, strings.TrimSpace(after),
			stateOf(t, fx.configPath, fx.artifact), fx.final, fx.diskSize, fx.diskHash)
	}
}

// TestIssue662_CLIRetryLoopsAndTheRefusalNamesNoWayOut is the loop, measured,
// and the sentence the operator gets each time round it.
//
// `rbm retry` moves FAILED -> DISCOVERED, the next cycle hits the same
// FR-12 collision, and the artifact is FAILED again. "A hundred cycles
// produce the same three failures."
//
// Three things are asserted, and they are separate findings:
//
//   - the cycle's exit code, checked directly rather than through a pipe,
//     because a refusal that exits 0 is how a scheduled run hides this;
//   - that the loop really is a loop (the same state, three rounds running,
//     from the same refusal);
//   - that the refusal an operator reads off `rbm artifacts` -- the FR-12
//     collision message, which is CORRECT and must not change its verdict
//     -- names something they could do about it.
//
// The third is issue #662's own third fix option, verbatim: "or the
// collision refusal must name the verb that resolves it". FinalName
// CollisionError's doc already says only an operator can judge the file;
// what it does not say is which command lets them act on the judgement.
func TestIssue662_CLIRetryLoopsAndTheRefusalNamesNoWayOut(t *testing.T) {
	cases := []struct {
		name string
		// collision is whether the good file is sitting at the final name.
		// #662's state has one. The control clears it, so the same retry
		// on the same deployment succeeds and the loop never forms: that
		// is what proves the loop below is the defect and not the fixture.
		collision bool
	}{
		{name: "retry into the FR-12 collision, which is #662's state", collision: true},
		{name: "control: the same retry with the final name free", collision: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := stage662DeadEnd(t, false)
			fx.drive662ToFailed(t)
			id := fx.artifact.String()
			if !tc.collision {
				if err := os.Remove(fx.final); err != nil {
					t.Fatalf("clearing the final name for the control: %v", err)
				}
			}

			var rounds []string
			for round := 1; round <= 3; round++ {
				retryCode, retryOut, retryErr := runCLI662(t, "retry", "--config", fx.configPath, id)
				fetchCode, fetchOut, fetchErr := runCLI662(t, "fetch", "--config", fx.configPath,
					"--backup-set", "production/postgres-primary")
				st := stateOf(t, fx.configPath, fx.artifact)
				rounds = append(rounds, fmt.Sprintf(
					"round %d: `rbm retry` exit %d %q; `rbm fetch` exit %d %q; artifact %s",
					round, retryCode, oneLine662(retryOut+retryErr), fetchCode, oneLine662(fetchOut+fetchErr), st))

				if durableState662(st) {
					return // the loop terminated in the good way.
				}
				if fetchCode == exitOK {
					t.Errorf(
						"#662: a cycle that got nothing through exited %d.\n  %s\n"+
							"`rbm fetch` reporting success over a set whose only artifact came back FAILED is how "+
							"a scheduled run hides this defect indefinitely.",
						fetchCode, rounds[len(rounds)-1])
				}
			}

			// What the operator reads about the artifact, through the verb
			// the issue used to read it.
			_, detail, detailErr := runCLI662(t, "artifacts", "--config", fx.configPath, id)
			said := detail + detailErr
			if !strings.Contains(said, "refusing to overwrite an existing final-name file") {
				t.Fatalf("`rbm artifacts` does not show the collision refusal, so this case is not measuring "+
					"#662's artifact:\n%s", indent662(said))
			}
			if !namesAVerb662(said) {
				t.Errorf(
					"#662 defect 3: three retries, three identical collisions, and the refusal an operator reads "+
						"names no way out.\n  %s\n\n  what `rbm artifacts` shows:\n%s\n\n"+
						"The refusal itself is right -- FR-12 must not clobber a file this package cannot "+
						"identify -- and #662 says so. What is missing is the next line: which command lets the "+
						"operator, who CAN identify it, act on that.",
					strings.Join(rounds, "\n  "), indent662(said))
			}
		})
	}
}

// TestIssue662_CLIStatusNamesWhichArtifactsNeedInterventionAndWhat is the
// operator's first command, and the one that decides whether they can act.
//
// Measured output:
//
//	cicd-pipeline/var-backups: FAILING
//	  a FAILED artifact has no retry scheduled and needs intervention
//	  current transfers: 0, pending deletes: 0, failures: 3
//
// "Needs intervention" and then nothing: not which artifacts, not which
// verb. The count is there, so the operator knows how many, and has to go
// hunting through `rbm artifacts` to find out which.
func TestIssue662_CLIStatusNamesWhichArtifactsNeedInterventionAndWhat(t *testing.T) {
	fx := stage662DeadEnd(t, false)
	fx.drive662ToFailed(t)

	code, out, errOut := runCLI662(t, "status", "--config", fx.configPath)
	said := out + errOut

	// The controls, and they are the reason the two absences below mean
	// anything. A substring search over captured output can fail for two
	// uninteresting reasons -- nothing was captured, or the deployment is
	// not the one described -- and both would look exactly like the
	// defect. These prove the text is here and searchable, and that the
	// set really is in the state the issue reports.
	if !strings.Contains(said, "production/postgres-primary") {
		t.Fatalf("control: `rbm status` output does not even name the backup set (exit %d); nothing was captured "+
			"or nothing ran:\n%s", code, indent662(said))
	}
	if !strings.Contains(said, "FAILING") {
		t.Fatalf("control: the set is not FAILING (exit %d), so this is not #662's deployment:\n%s",
			code, indent662(said))
	}
	if code == exitOK {
		t.Errorf("#662: `rbm status` over a FAILING set exited %d, want non-zero", code)
	}

	namesTheArtifact := strings.Contains(said, fx.artifact.Name)
	namesAWay := namesAVerb662(said)
	if !namesTheArtifact || !namesAWay {
		t.Errorf(
			"#662 defect 3: `rbm status` says intervention is needed without saying on what or with what.\n"+
				"  names the stuck artifact (%s): %v\n"+
				"  names a verb to run:            %v\n"+
				"  output:\n%s\n"+
				"An operator reading \"a FAILED artifact has no retry scheduled and needs intervention\" has to "+
				"discover both facts somewhere else before they can type anything, and the product knows both.",
			fx.artifact.Name, namesTheArtifact, namesAWay, indent662(said))
	}
}

// TestIssue662_CLIDryRunDisclosesTheFailuresItIsNotPlanning is the command that
// made the set look fine.
//
// Every object comes back "(already known)", the plan is empty, and the
// exit code is 0. The set has a FAILED artifact that no cycle will ever
// re-attempt, and the dry run -- the command whose entire job is to say
// what a run would do -- does not mention it. That is why the operator's
// question was "why does running it do nothing".
//
// The control below is the same command on the same deployment before the
// artifact was ever failed, which is the case where a silent, empty plan is
// the honest answer.
func TestIssue662_CLIDryRunDisclosesTheFailuresItIsNotPlanning(t *testing.T) {
	fx := stage662DeadEnd(t, false)

	// Control first, before any FAILED row exists: a dry run that says
	// nothing about failures is the honest answer here, and running it
	// establishes two things the red assertion below depends on. That
	// "(already known)" really is what a settled object looks like, so the
	// case below is measuring the same output the issue quotes; and that
	// the word the case below searches for is genuinely absent from a
	// healthy plan, so finding it later would mean something.
	_, controlOut, controlErr := runCLI662(t, "fetch", "--config", fx.configPath,
		"--backup-set", "production/postgres-primary", "--dry-run")
	control := controlOut + controlErr
	if !strings.Contains(control, "(already known)") {
		t.Fatalf("control: the dry run did not report the object as already known, so the fixture is not the one "+
			"#662 describes:\n%s", indent662(control))
	}
	if mentionsAFailure662(control) {
		t.Fatalf("control: a dry run over a set with no FAILED artifact already mentions a failure, so the "+
			"assertion below cannot discriminate:\n%s", indent662(control))
	}

	fx.drive662ToFailed(t)

	code, out, errOut := runCLI662(t, "fetch", "--config", fx.configPath,
		"--backup-set", "production/postgres-primary", "--dry-run")
	said := out + errOut

	if !strings.Contains(said, "(already known)") {
		t.Fatalf("the dry run over the failed set printed no plan at all (exit %d):\n%s", code, indent662(said))
	}
	if !mentionsAFailure662(said) {
		t.Errorf(
			"#662: `rbm fetch --dry-run` reports an empty plan over a set with a FAILED artifact and never "+
				"mentions it (exit %d).\n%s\n"+
				"Every object reads \"(already known)\", which is true and is exactly why the operator could not "+
				"tell a settled backup set apart from one that is stuck. A plan that will not touch a failed "+
				"artifact has to say so; \"why does running it do nothing\" was the operator's whole question.",
			code, indent662(said))
	}
}

// --- small helpers, kept local to this file ---

// namesAVerb662 reports whether text points the reader at a command they
// could actually type. It is the loosest honest reading of "the message
// names the verb that resolves it": any of the recovery subcommands
// spelled out, in any sentence.
//
// It deliberately does not look for a fixed phrase. What is being pinned is
// that the operator is given somewhere to go, not that the product adopted
// a particular wording.
func namesAVerb662(text string) bool {
	for _, verb := range []string{
		"quarantine reinstate", "quarantine revalidate", "quarantine retry",
		"rbm retry", "rbm validate", "rbm reconcile",
	} {
		if strings.Contains(text, verb) {
			return true
		}
	}
	return false
}

// mentionsAFailure662 reports whether text tells the reader that something
// in this backup set has failed. It is deliberately the loosest possible
// reading -- the word, in any case, anywhere -- so that any sentence a
// maintainer chooses satisfies it, and so a failure of this check is
// unambiguous: the output did not use the word at all.
func mentionsAFailure662(text string) bool {
	return strings.Contains(strings.ToLower(text), "fail")
}

// durableState662 is the same set of states `validate` calls a durable
// restore point. Reaching any of them is what "resolved" means for every
// case in this file.
func durableState662(s string) bool {
	switch lifecycle.State(s) {
	case lifecycle.Committed, lifecycle.RemoteDeletePending, lifecycle.Complete, lifecycle.RemoteRetained:
		return true
	}
	return false
}

// stripConfig662 removes the --config pair so a transcript in a failure
// message reads like the session in the issue rather than like a temp path.
func stripConfig662(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--config" {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func indent662(s string) string {
	return "    " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n    ")
}

func oneLine662(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
