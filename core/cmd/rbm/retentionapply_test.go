package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/service"
)

// `rbm retention apply` (issue #602).
//
// `retention` previews and has never been able to do anything else, so
// until this verb the only way to run FR-20's deletion was the HTTP
// preview/apply pair, behind a destructive gate whose only shipped
// implementation answers false. That left the deletion unreachable from
// every surface at once, and it left an operator on a terminal, or a
// support conversation, with nothing to ask for.
//
// The claims here are the same ones core/service's own evidence makes,
// asked of the command an operator types: the previewed DELETE set is
// removed, the KEEP set survives byte for byte, a file the plan never
// mentioned is not touched, and the acknowledgement is required rather
// than assumed.

// retentionApplyFixture is one configured backup set on a real directory,
// with the artifacts this file needs already in its journal.
type retentionApplyFixture struct {
	configPath string
	localDir   string
	set        model.BackupSetID
}

// writeRetentionApplyConfig builds a deployment whose live chain is a
// single seven-day daily tier, with FR-19's protection left at its
// default (on).
//
// Seven days, and artifacts seeded at whole-day offsets of 0, 40 and 400,
// for the reason core/tests/compat's own retention cell gives: nothing
// here can pin this binary's clock, so a fixture anchored on a literal
// date drifts out of every window as the date passes and starts
// certifying something else. These offsets give the same one-in, two-out
// split on every day of the year.
func writeRetentionApplyConfig(t *testing.T) retentionApplyFixture {
	t.Helper()
	dir := t.TempDir()
	localDir := filepath.Join(dir, "local")
	remoteDir := filepath.Join(dir, "remote")
	for _, d := range []string{localDir, remoteDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", d, err)
		}
	}

	configPath := filepath.Join(dir, "config.yaml")
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + filepath.Join(dir, "state.db") + "\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: postgres-primary\n" +
		"        remote:\n" +
		"          type: local\n" +
		"        remote_path: " + remoteDir + "\n" +
		"        local_path: " + localDir + "\n" +
		"        include:\n" +
		"          - \"*.dump\"\n" +
		"        completion:\n" +
		"          strategy: rename\n" +
		"        stale_after: 24h\n" +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n" +
		"  tiers:\n" +
		"    - name: daily\n" +
		"      granularity: day\n" +
		"      keep: 7\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", configPath, err)
	}

	set, err := model.NewBackupSetID("production", "postgres-primary")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	return retentionApplyFixture{configPath: configPath, localDir: localDir, set: set}
}

// seedRetentionApplyArtifact writes a real local file and the two journal
// transitions that make it a final managed artifact, dated whole days
// back from now.
func (f retentionApplyFixture) seed(t *testing.T, name string, daysAgo int, content string) {
	t.Helper()
	ctx := context.Background()

	j, err := state.Open(ctx, filepath.Join(filepath.Dir(f.configPath), "state.db"))
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = j.Close() }()

	artifact, err := model.NewArtifactID(f.set, name)
	if err != nil {
		t.Fatalf("NewArtifactID(%q): %v", name, err)
	}
	local := filepath.Join(f.localDir, name)
	if err := os.WriteFile(local, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", local, err)
	}

	at := time.Now().UTC().Add(-time.Duration(daysAgo) * 24 * time.Hour)
	if _, err := j.Discover(ctx, artifact, name+"-discover", "/backups/"+name, state.RemoteIdentity{}, at); err != nil {
		t.Fatalf("Discover(%s): %v", name, err)
	}
	size := int64(len(content))
	if _, err := j.RecordTransition(ctx, state.Transition{
		Artifact: artifact, Key: name + "-complete",
		From: string(lifecycle.Discovered), To: string(lifecycle.Complete),
		OccurredAt: at, LocalPath: &local,
		Transfer: &state.TransferResult{BytesTransferred: size, Checksummed: true},
	}); err != nil {
		t.Fatalf("RecordTransition(complete %s): %v", name, err)
	}
}

// localTree fingerprints the backup set's directory, so a claim about
// what an apply removed is a claim about the whole of it rather than
// about the files this test remembered to name.
func (f retentionApplyFixture) localTree(t *testing.T) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(f.localDir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", f.localDir, err)
	}
	out := map[string]string{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(f.localDir, e.Name()))
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", e.Name(), err)
		}
		sum := sha256.Sum256(b)
		out[e.Name()] = hex.EncodeToString(sum[:])
	}
	return out
}

func namesOf(tree map[string]string) []string {
	out := make([]string, 0, len(tree))
	for k := range tree {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestRun_RetentionApplyRemovesExactlyThePreviewedDeleteSet is the verb
// doing the thing it exists for, on a real directory.
//
// unmanaged-by-anything.txt is in the same directory and no journal row
// mentions it. FR-20 never lists a directory to find something to delete,
// so it has to survive; it is here because an apply that reached beyond
// its plan is invisible to any assertion that only looks up the files the
// plan named.
func TestRun_RetentionApplyRemovesExactlyThePreviewedDeleteSet(t *testing.T) {
	f := writeRetentionApplyConfig(t)
	f.seed(t, "today.dump", 0, "inside the daily window")
	f.seed(t, "forty-days-old.dump", 40, "outside every window")
	f.seed(t, "four-hundred-days-old.dump", 400, "outside every window as well")
	unmanaged := filepath.Join(f.localDir, "unmanaged-by-anything.txt")
	if err := os.WriteFile(unmanaged, []byte("no journal row mentions this"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	before := f.localTree(t)

	var code int
	out := captureStdout(t, func() {
		code = run([]string{"retention", "--config", f.configPath, "apply", f.set.String(), "--acknowledge"})
	})
	if code != 0 {
		t.Fatalf("retention apply: %d, want 0\n%s", code, out)
	}

	// The itemisation an operator reads back afterwards has to name what
	// went, or the command's own receipt describes a different run.
	for _, name := range []string{"forty-days-old.dump", "four-hundred-days-old.dump"} {
		if !strings.Contains(out, name) {
			t.Errorf("the output does not name %s, which it deleted.\ngot:\n%s", name, out)
		}
	}

	after := f.localTree(t)
	if got := namesOf(after); !equalNames(got, []string{"today.dump", "unmanaged-by-anything.txt"}) {
		t.Fatalf("after the apply the backup set holds %v; want exactly [today.dump unmanaged-by-anything.txt]\n%s", got, out)
	}
	for _, name := range []string{"today.dump", "unmanaged-by-anything.txt"} {
		if after[name] != before[name] {
			t.Errorf("%s survived the apply and is not the same file: it was %s and is now %s", name, before[name], after[name])
		}
	}
}

// TestRun_RetentionApplyRefusesWithoutTheAcknowledgement is the
// confirmation, and it is the whole of what stands between a mistyped
// command line and a deleted restore point.
//
// Nothing is opened and nothing is deleted, so the exit code is the
// "nothing ran, the command line was wrong" one rather than an ordinary
// failure. The refusal has to say what the flag is for: a refusal that
// only names a missing flag teaches an operator to add it without ever
// reading what it consents to.
func TestRun_RetentionApplyRefusesWithoutTheAcknowledgement(t *testing.T) {
	f := writeRetentionApplyConfig(t)
	f.seed(t, "forty-days-old.dump", 40, "outside every window")
	before := f.localTree(t)

	var code int
	stderr := captureStderr(t, func() {
		code = run([]string{"retention", "--config", f.configPath, "apply", f.set.String()})
	})
	if code != exitUsage {
		t.Fatalf("retention apply without --acknowledge: %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "--acknowledge") {
		t.Errorf("the refusal does not name the flag that is missing.\ngot: %s", stderr)
	}
	if !strings.Contains(stderr, "delete") {
		t.Errorf("the refusal does not say what the acknowledgement is for, so it teaches an operator to add the flag without reading it.\ngot: %s", stderr)
	}
	if got := namesOf(f.localTree(t)); !equalNames(got, namesOf(before)) {
		t.Errorf("the backup set changed under a refused apply: it was %v and is now %v", namesOf(before), got)
	}
}

// TestRun_RetentionApplyRefusesAnIdThatNamesNoConfiguredSet mirrors what
// `retention <id>` already does with the same operand, for the reason
// that command's own doc gives: it is the same operand, spelled the same
// way, on a command an operator moves to and from.
func TestRun_RetentionApplyRefusesAnIdThatNamesNoConfiguredSet(t *testing.T) {
	f := writeRetentionApplyConfig(t)
	f.seed(t, "forty-days-old.dump", 40, "outside every window")
	before := namesOf(f.localTree(t))

	var code int
	var stderr string
	stdout := captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			code = run([]string{"retention", "--config", f.configPath, "apply", "production/not-a-set", "--acknowledge"})
		})
	})
	if code != exitFailure {
		t.Fatalf("retention apply on an unknown set: %d, want %d", code, exitFailure)
	}
	// No plan, for the reason `retention <id>`'s own refusal prints
	// nothing: a plan rendered beside a refusal is a confidently wrong
	// answer about a set nobody asked about. The startup announcement
	// this binary writes on every invocation is not one.
	if strings.Contains(stdout, "plan retplan_") || strings.Contains(stdout, "postgres-primary") {
		t.Errorf("a plan was printed for a backup set that is not configured:\n%s", stdout)
	}
	if !strings.Contains(stderr, "not-a-set") {
		t.Errorf("the refusal does not name the id that was refused.\ngot: %s", stderr)
	}
	if got := namesOf(f.localTree(t)); !equalNames(got, before) {
		t.Errorf("the backup set changed under a refused apply: it was %v and is now %v", before, got)
	}
}

// TestRun_RetentionApplyRefusesTheWrongOperandShape keeps the verb's own
// operand rules where `retention`'s already are: two ids, or a string
// that is not shaped like one, is a usage mistake and exits 2 before a
// configuration is loaded.
func TestRun_RetentionApplyRefusesTheWrongOperandShape(t *testing.T) {
	f := writeRetentionApplyConfig(t)
	for _, args := range [][]string{
		{"retention", "--config", f.configPath, "apply", "--acknowledge"},
		{"retention", "--config", f.configPath, "apply", "not-an-id", "--acknowledge"},
		{"retention", "--config", f.configPath, "apply", "production/a", "production/b", "--acknowledge"},
	} {
		got := run(args)
		if got != exitUsage {
			t.Errorf("run(%v) = %d, want %d", args, got, exitUsage)
		}
	}
}

// TestRetentionApplyRefusalNamesAStalePlan is the staleness refusal at
// this boundary.
//
// It drives the renderer rather than racing a journal, and that is said
// out loud rather than dressed up: the window between the preview and the
// apply is inside one invocation of this command, so there is no
// black-box way to widen it from a test. What core/service does with a
// plan whose inputs moved is pinned where it happens (retention_test.go
// and apps/common/webhost/settings_gate_test.go, at both boundaries, each
// asserting zero deletions). What is unpinned without this is whether the
// terminal turns that refusal into something an operator can act on, and
// the failure mode is specific: RETENTION_PLAN_STALE rendered as a bare
// "an internal error occurred" reads as a broken deployment rather than
// as "look again and re-run", and the operator's next move is wrong.
func TestRetentionApplyRefusalNamesAStalePlan(t *testing.T) {
	stale := fmt.Errorf("%w: backup set production/postgres-primary changed since plan retplan_x was previewed", service.ErrRetentionPlanStale)

	for _, tc := range []struct {
		name string
		err  error
		want []string
	}{
		{
			name: "a stale plan",
			err:  stale,
			want: []string{"nothing was deleted", "changed", "again"},
		},
		{
			name: "the plan this deployment is already applying",
			err:  service.ErrRetentionApplyBusy,
			want: []string{"nothing was deleted"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			stderr := captureStderr(t, func() { code = failRetentionApply(tc.err) })
			if code != exitFailure {
				t.Errorf("exit = %d, want %d", code, exitFailure)
			}
			for _, want := range tc.want {
				if !strings.Contains(stderr, want) {
					t.Errorf("the refusal does not say %q, so an operator cannot tell what to do next.\ngot: %s", want, stderr)
				}
			}
		})
	}
}

// TestRun_RetentionApplyIsNotTheBarePreview keeps the two surfaces apart.
//
// `retention` previews in both its modes and says so, and that sentence
// is pinned by core/tests/compat and by the black-box suite in
// spdrman/rclone-manager-tests. Adding a verb beside it must not turn the
// bare form into something that deletes, which is the exact confusion
// `retention --dry-run` being inert was designed to avoid.
func TestRun_RetentionApplyIsNotTheBarePreview(t *testing.T) {
	f := writeRetentionApplyConfig(t)
	f.seed(t, "forty-days-old.dump", 40, "outside every window")
	before := namesOf(f.localTree(t))

	for _, args := range [][]string{
		{"retention", "--config", f.configPath},
		{"retention", "--config", f.configPath, "--dry-run"},
		{"retention", "--config", f.configPath, f.set.String()},
	} {
		captureStdout(t, func() {
			if got := run(args); got != 0 {
				t.Errorf("run(%v) = %d, want 0", args, got)
			}
		})
		if got := namesOf(f.localTree(t)); !equalNames(got, before) {
			t.Fatalf("run(%v) deleted something: the backup set was %v and is now %v", args, before, got)
		}
	}
}

func equalNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestUsage_NamesEveryRetentionVerb closes for `retention` the level
// TestUsage_EveryRegisteredCommandIsPinned cannot reach on its own, the
// way TestUsage_NamesEveryMediumVerb and TestUsage_NamesEveryBackupSetVerb
// already do for theirs.
//
// The dispatch map in main.go has one entry for `retention`, so everything
// over there is satisfied the moment the bare form is listed and pinned,
// and stays satisfied forever after. A second verb added to retentionVerbs
// tomorrow would be dispatchable, absent from usage(), invisible to the
// black-box verb guard in the tests repository, and pinned by nothing,
// which is exactly the shape #549 is about. The verbs come off the table
// rather than off a list typed here, so adding one is checked without
// anybody remembering this test exists.
func TestUsage_NamesEveryRetentionVerb(t *testing.T) {
	verbs := retentionVerbNames()
	if len(verbs) == 0 {
		t.Fatal("retentionVerbNames() is empty, so this test would check nothing and pass. Either retentionVerbs lost its entries, in which case `retention` dispatches no verb at all, or the table moved and this test is reading the wrong one.")
	}
	out := captureStderr(t, usage)
	for _, verb := range verbs {
		if !strings.Contains(out, "retention "+verb+" ") {
			t.Errorf("usage() does not list \"retention %s\"; an operator cannot discover it, the black-box verb guard cannot see it, and TestUsage_EveryRegisteredCommandIsPinned cannot ask for its entry line to be pinned because there is no entry line.", verb)
		}
	}
}
