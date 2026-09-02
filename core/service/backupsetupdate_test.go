package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/core/internal/config"
)

// The fixture writeTestConfigFile writes: one source, one backup set,
// remote.type local. Every test below edits that set, so this names it
// once rather than repeating the string.
const fixtureSetID = "production/postgres-primary"

func strPtr(s string) *string { return &s }

// readBackupSetFromDisk re-reads configPath and returns the named backup
// set as it is actually persisted, never this process's in-memory copy.
// Every assertion about durability in this file goes through it: a change
// only this process can see is exactly the "not saved" outcome issue #350
// exists to end.
func readBackupSetFromDisk(t *testing.T, configPath, sourceName, setName string) config.BackupSet {
	t.Helper()
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile(configPath): %v", err)
	}
	var onDisk config.Config
	if err := yaml.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("yaml.Unmarshal(configPath): %v", err)
	}
	for _, src := range onDisk.Sources {
		if src.Name != sourceName {
			continue
		}
		for _, bs := range src.BackupSets {
			if bs.Name == setName {
				return bs
			}
		}
	}
	t.Fatalf("the on-disk config has no backup set %s/%s:\n%s", sourceName, setName, raw)
	return config.BackupSet{}
}

// yamlNormalized round-trips one backup set through the same YAML encoder
// and decoder the config write path uses. It exists because that
// round-trip is not identity: yaml.Marshal writes every zero-valued
// field, so a nil []string comes back as an empty non-nil one, poll_interval
// "15m" comes back "15m0s", and so on. That expansion is pre-existing
// behaviour of the ONE write mechanism this issue was told to reuse (it
// happens identically on SetBackupSetEnabled, SetBackupSetReadOnly,
// UpdateSettings and CreateBackupSet, checked against the real file
// before this helper was written), so a test comparing a persisted set
// against a hand-built expectation has to compare like with like rather
// than quietly weaken itself to accommodate it.
func yamlNormalized(t *testing.T, bs config.BackupSet) config.BackupSet {
	t.Helper()
	raw, err := yaml.Marshal(bs)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	var out config.BackupSet
	if err := yaml.Unmarshal(raw, &out); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	return out
}

// TestUpdateBackupSet_PersistsAndIsImmediatelyVisible is this issue's
// central RED case: before it there was no update path anywhere, so the
// only way to change a configured backup set was to hand-edit
// config.yaml. It asserts the same three things
// TestCreateBackupSet_PersistsAndIsImmediatelyVisible does for creation,
// because an update that only half-satisfies them is the same "honest
// not-saved notice" with extra steps: the returned value carries the new
// setting, a read back through this SAME service sees it with no restart,
// and the file on disk actually holds it.
func TestUpdateBackupSet_PersistsAndIsImmediatelyVisible(t *testing.T) {
	svc, configPath := openTestService(t)
	revisionBefore := svc.ConfigRevision()

	newRemote := filepath.Join(t.TempDir(), "moved-remote")
	if err := os.MkdirAll(newRemote, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	updated, err := svc.UpdateBackupSet(context.Background(), fixtureSetID, UpdateBackupSetRequest{
		RemotePath: strPtr(newRemote),
	})
	if err != nil {
		t.Fatalf("UpdateBackupSet: %v", err)
	}
	if updated.RemotePath != newRemote {
		t.Errorf("returned RemotePath = %q, want %q", updated.RemotePath, newRemote)
	}

	if svc.ConfigRevision() == revisionBefore {
		t.Error("ConfigRevision did not change after UpdateBackupSet")
	}

	got, err := svc.GetBackupSet(context.Background(), fixtureSetID)
	if err != nil {
		t.Fatalf("GetBackupSet: %v", err)
	}
	if got.RemotePath != newRemote {
		t.Errorf("GetBackupSet RemotePath = %q, want %q", got.RemotePath, newRemote)
	}

	if onDisk := readBackupSetFromDisk(t, configPath, "production", "postgres-primary"); onDisk.RemotePath != newRemote {
		t.Errorf("on-disk remote_path = %q, want %q", onDisk.RemotePath, newRemote)
	}
}

// TestUpdateBackupSet_WritesOnlyTheFieldsTheRequestNames is the service
// half of the issue's "a per-box Save writes only that box". The UI
// promising to send only what changed would not be enough: a request that
// names one field must be structurally incapable of moving another, which
// is why every field on UpdateBackupSetRequest is a pointer and nil means
// "leave alone" rather than "set to the zero value".
//
// It compares the WHOLE persisted config.BackupSet against the baseline
// with only the edited field patched in, rather than spot-checking a
// couple of neighbours. Spot-checking is how this test first passed while
// an update to remote_path also silently rewrote local_path: the
// assertions happened to look at the other direction. Comparing the whole
// struct means any field this method learns to move by accident fails
// here, including one added to config.BackupSet later that nobody thought
// to list.
func TestUpdateBackupSet_WritesOnlyTheFieldsTheRequestNames(t *testing.T) {
	newRemote := t.TempDir()
	newLocal := filepath.Join(t.TempDir(), "moved-local")
	include := []string{"*.tar.zst"}
	stable := 90 * time.Second

	cases := []struct {
		name string
		req  UpdateBackupSetRequest
		// want patches the baseline with exactly the change this request
		// is allowed to make, and nothing else.
		want func(config.BackupSet) config.BackupSet
	}{
		{
			"remote_path", UpdateBackupSetRequest{RemotePath: strPtr(newRemote)},
			func(bs config.BackupSet) config.BackupSet { bs.RemotePath = newRemote; return bs },
		},
		{
			"local_path", UpdateBackupSetRequest{LocalPath: strPtr(newLocal)},
			func(bs config.BackupSet) config.BackupSet { bs.LocalPath = newLocal; return bs },
		},
		{
			"include", UpdateBackupSetRequest{Include: &include},
			func(bs config.BackupSet) config.BackupSet {
				bs.Include = append([]string(nil), include...)
				return bs
			},
		},
		{
			"stale_after", UpdateBackupSetRequest{StaleAfter: durationPtr(36 * time.Hour)},
			func(bs config.BackupSet) config.BackupSet {
				bs.StaleAfter = config.Duration(36 * time.Hour)
				return bs
			},
		},
		{
			"completion strategy and stable_for together",
			UpdateBackupSetRequest{CompletionStrategy: strPtr("stable"), StableFor: &stable},
			func(bs config.BackupSet) config.BackupSet {
				bs.Completion.Strategy = "stable"
				bs.Completion.StableFor = config.Duration(stable)
				return bs
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The RICH fixture, not openTestService's minimal one: a
			// whole-struct comparison only proves anything about fields
			// the fixture actually set, and
			// TestUpdateBackupSetIsolationFixtureExercisesEveryField
			// holds this one to setting all of them.
			configPath := writeRichTestConfigFile(t)
			svc := openServiceAt(t, configPath)
			baseline := readBackupSetFromDisk(t, configPath, "production", "postgres-primary")

			if _, err := svc.UpdateBackupSet(context.Background(), fixtureSetID, tc.req); err != nil {
				t.Fatalf("UpdateBackupSet: %v", err)
			}

			got := readBackupSetFromDisk(t, configPath, "production", "postgres-primary")
			want := yamlNormalized(t, tc.want(baseline))
			if !reflect.DeepEqual(got, want) {
				t.Errorf("the persisted backup set moved something this request did not name.\n got: %+v\nwant: %+v", got, want)
			}
		})
	}
}

// TestUpdateBackupSet_TwoSuccessiveSingleFieldUpdatesBothSurvive is the
// per-box Save sequence the issue describes, at the layer that actually
// persists: save box A, then save box B, and A must still be there. An
// implementation that rebuilt the whole set from its request each time
// would pass the test above and fail this one.
func TestUpdateBackupSet_TwoSuccessiveSingleFieldUpdatesBothSurvive(t *testing.T) {
	svc, configPath := openTestService(t)

	newRemote := filepath.Join(t.TempDir(), "remote-a")
	if err := os.MkdirAll(newRemote, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	newLocal := filepath.Join(t.TempDir(), "local-b")

	if _, err := svc.UpdateBackupSet(context.Background(), fixtureSetID, UpdateBackupSetRequest{
		RemotePath: strPtr(newRemote),
	}); err != nil {
		t.Fatalf("UpdateBackupSet(remote): %v", err)
	}
	if _, err := svc.UpdateBackupSet(context.Background(), fixtureSetID, UpdateBackupSetRequest{
		LocalPath: strPtr(newLocal),
	}); err != nil {
		t.Fatalf("UpdateBackupSet(local): %v", err)
	}

	onDisk := readBackupSetFromDisk(t, configPath, "production", "postgres-primary")
	if onDisk.RemotePath != newRemote {
		t.Errorf("on-disk remote_path = %q, want %q (the first save must survive the second)", onDisk.RemotePath, newRemote)
	}
	if onDisk.LocalPath != newLocal {
		t.Errorf("on-disk local_path = %q, want %q", onDisk.LocalPath, newLocal)
	}
}

// TestUpdateBackupSet_RefusesARequestThatNamesNothing: an update that
// changes nothing is a caller bug, not a no-op worth persisting and
// hot-reloading a whole configuration for. Refusing it also keeps the
// HTTP layer honest, since a client sending {} would otherwise get a 200
// that means nothing.
func TestUpdateBackupSet_RefusesARequestThatNamesNothing(t *testing.T) {
	svc, _ := openTestService(t)

	_, err := svc.UpdateBackupSet(context.Background(), fixtureSetID, UpdateBackupSetRequest{})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("UpdateBackupSet(empty) error = %v, want ErrInvalidRequest", err)
	}
}

// TestUpdateBackupSet_AppliesTheSameFieldRulesCreationDoes is the issue's
// "validation equal to creation's". Each case names a value
// validateCreateRequest already refuses on the create path; an update
// that accepted any of them would let an operator reach, through Edit, a
// configuration the wizard would have refused to create.
func TestUpdateBackupSet_AppliesTheSameFieldRulesCreationDoes(t *testing.T) {
	cases := []struct {
		name string
		req  UpdateBackupSetRequest
		want string
	}{
		{"empty host", UpdateBackupSetRequest{Host: strPtr("")}, "host"},
		{"empty user", UpdateBackupSetRequest{User: strPtr("")}, "user"},
		{"empty remote_path", UpdateBackupSetRequest{RemotePath: strPtr("")}, "remote_path"},
		{"empty local_path", UpdateBackupSetRequest{LocalPath: strPtr("")}, "local_path"},
		{"unknown completion strategy", UpdateBackupSetRequest{CompletionStrategy: strPtr("whenever")}, "completion_strategy"},
		{"stable with no stable_for", UpdateBackupSetRequest{CompletionStrategy: strPtr("stable")}, "stable_for"},
		{"unregistered validator", UpdateBackupSetRequest{ValidatorID: validatorPtr("/usr/bin/anything")}, "validator_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, configPath := openTestService(t)
			beforeRaw, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}

			_, err = svc.UpdateBackupSet(context.Background(), fixtureSetID, tc.req)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("error = %v, want ErrInvalidRequest", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the field %q that was wrong", err, tc.want)
			}

			// A refused update must not have written anything at all.
			afterRaw, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if string(beforeRaw) != string(afterRaw) {
				t.Errorf("a refused update rewrote the config file:\nbefore:\n%s\nafter:\n%s", beforeRaw, afterRaw)
			}
		})
	}
}

func validatorPtr(v string) *ValidatorID {
	id := ValidatorID(v)
	return &id
}

// TestUpdateBackupSet_RefusedByWholeConfigValidationLeavesTheFileAlone
// covers the other validation layer creation goes through: cfg.Validate
// over the WHOLE resulting configuration, not only the request's own
// fields. A relative remote_path passes every per-field rule and is
// refused by config.Validate, and the file must be untouched afterwards.
func TestUpdateBackupSet_RefusedByWholeConfigValidationLeavesTheFileAlone(t *testing.T) {
	svc, configPath := openTestService(t)
	beforeRaw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	_, err = svc.UpdateBackupSet(context.Background(), fixtureSetID, UpdateBackupSetRequest{
		RemotePath: strPtr("relative/not/absolute"),
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("error = %v, want ErrInvalidRequest", err)
	}

	afterRaw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(beforeRaw) != string(afterRaw) {
		t.Errorf("a config-validation refusal rewrote the file:\nbefore:\n%s\nafter:\n%s", beforeRaw, afterRaw)
	}

	// And the running service still serves the old value.
	got, err := svc.GetBackupSet(context.Background(), fixtureSetID)
	if err != nil {
		t.Fatalf("GetBackupSet: %v", err)
	}
	if got.RemotePath == "relative/not/absolute" {
		t.Error("the refused value reached the running service")
	}
}

// TestUpdateBackupSet_KeepsTheFilesOwnOmissions pins the same
// encode-before-Validate discipline CreateBackupSet, SetBackupSetEnabled
// and UpdateSettings all document: cfg.Validate resolves retention and
// alerts IN PLACE, so encoding after it would freeze this release's
// defaults into an operator's file merely because they edited an
// unrelated field. The fixture writes only timezone and week_starts_on
// under retention, so a resolved write would add tier lines that were
// never there.
func TestUpdateBackupSet_KeepsTheFilesOwnOmissions(t *testing.T) {
	svc, configPath := openTestService(t)

	beforeRaw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(beforeRaw), "tiers:") {
		t.Fatalf("fixture precondition failed: the config already carries resolved retention tiers:\n%s", beforeRaw)
	}

	if _, err := svc.UpdateBackupSet(context.Background(), fixtureSetID, UpdateBackupSetRequest{
		StaleAfter: durationPtr(36 * time.Hour),
	}); err != nil {
		t.Fatalf("UpdateBackupSet: %v", err)
	}

	afterRaw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(afterRaw), "tiers:") {
		t.Errorf("updating one backup set froze today's resolved retention tiers into the operator's file:\n%s", afterRaw)
	}
}

func durationPtr(d time.Duration) *time.Duration { return &d }

// TestUpdateBackupSet_UnknownSetIsNotFound: the same sentinel
// GetBackupSet and SetBackupSetEnabled already return, so the HTTP layer
// maps it to the same 404 without a second vocabulary.
func TestUpdateBackupSet_UnknownSetIsNotFound(t *testing.T) {
	svc, _ := openTestService(t)

	for _, id := range []string{"production/nope", "nope/postgres-primary", "no-slash", ""} {
		_, err := svc.UpdateBackupSet(context.Background(), id, UpdateBackupSetRequest{
			LocalPath: strPtr(filepath.Join(t.TempDir(), "x")),
		})
		if !errors.Is(err, ErrBackupSetNotFound) {
			t.Errorf("UpdateBackupSet(%q) error = %v, want ErrBackupSetNotFound", id, err)
		}
	}
}

// TestUpdateBackupSet_NeedsAConfigFileToPersistTo mirrors
// CreateBackupSet's own refusal: a BackupService built with New has
// nothing to write to, and saying so is better than reporting a change
// that lives only in memory.
func TestUpdateBackupSet_NeedsAConfigFileToPersistTo(t *testing.T) {
	svc := newTestService(t)

	_, err := svc.UpdateBackupSet(context.Background(), fixtureSetID, UpdateBackupSetRequest{
		LocalPath: strPtr(filepath.Join(t.TempDir(), "x")),
	})
	if !errors.Is(err, ErrConfigNotFileBacked) {
		t.Fatalf("error = %v, want ErrConfigNotFileBacked", err)
	}
}

// TestUpdateBackupSet_UpdatesEveryEditableField walks the whole editable
// surface in one call, so a field added to UpdateBackupSetRequest without
// being wired through to config.BackupSet fails here rather than silently
// doing nothing on the one screen that offers it.
func TestUpdateBackupSet_UpdatesEveryEditableField(t *testing.T) {
	svc, configPath := openTestService(t)

	newRemote := filepath.Join(t.TempDir(), "remote")
	if err := os.MkdirAll(newRemote, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	newLocal := filepath.Join(t.TempDir(), "local")

	include := []string{"*.tar.zst", "*.sql"}
	stable := 90 * time.Second
	_, err := svc.UpdateBackupSet(context.Background(), fixtureSetID, UpdateBackupSetRequest{
		RemotePath:         strPtr(newRemote),
		LocalPath:          strPtr(newLocal),
		Include:            &include,
		CompletionStrategy: strPtr("stable"),
		StableFor:          &stable,
		StaleAfter:         durationPtr(12 * time.Hour),
	})
	if err != nil {
		t.Fatalf("UpdateBackupSet: %v", err)
	}

	onDisk := readBackupSetFromDisk(t, configPath, "production", "postgres-primary")
	if onDisk.RemotePath != newRemote {
		t.Errorf("remote_path = %q, want %q", onDisk.RemotePath, newRemote)
	}
	if onDisk.LocalPath != newLocal {
		t.Errorf("local_path = %q, want %q", onDisk.LocalPath, newLocal)
	}
	if strings.Join(onDisk.Include, ",") != "*.tar.zst,*.sql" {
		t.Errorf("include = %v, want [*.tar.zst *.sql]", onDisk.Include)
	}
	if onDisk.Completion.Strategy != "stable" {
		t.Errorf("completion.strategy = %q, want %q", onDisk.Completion.Strategy, "stable")
	}
	if onDisk.Completion.StableFor.Duration() != stable {
		t.Errorf("completion.stable_for = %s, want %s", onDisk.Completion.StableFor, stable)
	}
	if onDisk.StaleAfter.Duration() != 12*time.Hour {
		t.Errorf("stale_after = %s, want 12h", onDisk.StaleAfter)
	}
}

// TestUpdateBackupSet_MovingOffStableClearsStableFor: `stable_for` only
// means anything under the "stable" strategy, and leaving the old value
// behind after a move to "rename" would put a number in the operator's
// file that nothing reads and that the next reader has to work out is
// dead. Creation never writes one for a non-stable strategy
// (newBackupSetFor), so neither does an edit.
func TestUpdateBackupSet_MovingOffStableClearsStableFor(t *testing.T) {
	svc, configPath := openTestService(t)

	stable := 45 * time.Second
	if _, err := svc.UpdateBackupSet(context.Background(), fixtureSetID, UpdateBackupSetRequest{
		CompletionStrategy: strPtr("stable"),
		StableFor:          &stable,
	}); err != nil {
		t.Fatalf("UpdateBackupSet(stable): %v", err)
	}
	if _, err := svc.UpdateBackupSet(context.Background(), fixtureSetID, UpdateBackupSetRequest{
		CompletionStrategy: strPtr("rename"),
	}); err != nil {
		t.Fatalf("UpdateBackupSet(rename): %v", err)
	}

	onDisk := readBackupSetFromDisk(t, configPath, "production", "postgres-primary")
	if onDisk.Completion.StableFor.Duration() != 0 {
		t.Errorf("completion.stable_for = %s after moving to the rename strategy, want it cleared", onDisk.Completion.StableFor)
	}
}

// openServiceAt is openTestService against a config file the caller chose,
// so a test can run the real service over the rich fixture rather than the
// minimal one.
func openServiceAt(t *testing.T, configPath string) *BackupService {
	t.Helper()
	svc, cleanup, err := Open(context.Background(), configPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })
	return svc
}

// writeRichTestConfigFile writes the fixture the isolation tests below
// run against: one backup set with every field it is possible to set from
// a config file actually set, rather than the ordinary fixture's minimum.
//
// The richness is the point, not thoroughness for its own sake. The
// isolation test compares the WHOLE persisted config.BackupSet, which
// only proves anything about a field the fixture actually gave a value
// to: a field left at its zero value compares equal on both sides
// whatever the patch applier did to it.
// TestUpdateBackupSetIsolationFixtureExercisesEveryField enforces that,
// and this function is what it enforces it against.
func writeRichTestConfigFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	remoteDir := filepath.Join(dir, "remote")
	if err := os.MkdirAll(remoteDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	content := "poll_interval: 15m\n" +
		"state:\n  database: " + filepath.Join(dir, "state.db") + "\n" +
		"sources:\n" +
		"  - id: production\n" +
		"    backup_sets:\n" +
		"      - id: postgres-primary\n" +
		"        remote:\n          type: local\n" +
		"        remote_path: " + remoteDir + "\n" +
		"        local_path: " + filepath.Join(dir, "local") + "\n" +
		"        include:\n          - \"*.dump\"\n" +
		"        completion:\n          strategy: rename\n" +
		"        stale_after: 24h\n" +
		"        read_only: true\n" +
		"        validation:\n          hash: sha256\n" +
		"        retention:\n" +
		"          daily_days: 90\n" +
		"          weekly_months: 24\n" +
		"          monthly_months: 60\n" +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n" +
		"  daily_days: 7\n" +
		"  weekly_months: 3\n" +
		"  monthly_months: 12\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath
}

// TestUpdateBackupSet_LeavesAPerSetRetentionOverrideAlone is the same
// per-box guarantee, aimed at the field this codebase gained most
// recently (issue #333/#336's per-set retention override). It is worth
// its own case rather than trusting the whole-struct comparison above,
// because this field is the one where getting it wrong is silent and
// expensive: a set retaining 90/24/60 that quietly reverts to the
// deployment's 7/3/12 deletes restore points the operator believes are
// kept, and reports nothing at all while doing it.
//
// It checks BOTH copies. On disk, because that is what outlives the
// process and what a rollback would read. And through the running
// service, because a write that kept the file right while leaving this
// process resolving under the old policy is the exact failure #336's own
// rule ("any mutation must be followed by Validate") names.
func TestUpdateBackupSet_LeavesAPerSetRetentionOverrideAlone(t *testing.T) {
	configPath := writeRichTestConfigFile(t)
	svc := openServiceAt(t, configPath)

	newLocal := filepath.Join(t.TempDir(), "moved-local")
	if _, err := svc.UpdateBackupSet(context.Background(), fixtureSetID, UpdateBackupSetRequest{
		LocalPath: strPtr(newLocal),
	}); err != nil {
		t.Fatalf("UpdateBackupSet: %v", err)
	}

	// The override is still the set's own, still whole, still on disk.
	onDisk := readBackupSetFromDisk(t, configPath, "production", "postgres-primary")
	if onDisk.RetentionConfig == nil {
		t.Fatalf("the set's own retention override was dropped by an update that never named it:\n%s", mustRead(t, configPath))
	}
	if got := onDisk.RetentionConfig.DailyDays; got != 90 {
		t.Errorf("on-disk daily_days = %d, want 90", got)
	}
	if got := onDisk.RetentionConfig.WeeklyMonths; got != 24 {
		t.Errorf("on-disk weekly_months = %d, want 24", got)
	}
	if got := onDisk.RetentionConfig.MonthlyMonths; got != 60 {
		t.Errorf("on-disk monthly_months = %d, want 60", got)
	}
	if onDisk.LocalPath != newLocal {
		t.Errorf("on-disk local_path = %q, want %q; the field the request DID name must still have landed", onDisk.LocalPath, newLocal)
	}

	// And this process is deciding under it, not under the deployment's
	// 7/3/12, which is what the hot reload's own Validate pass is for.
	resolved := svc.state.Load().inner.Config.Sources[0].BackupSets[0].Retention
	if resolved.DailyDays != 90 || resolved.WeeklyMonths != 24 || resolved.MonthlyMonths != 60 {
		t.Errorf("after the update this service resolves the set to %d/%d/%d, want 90/24/60: the hot reload left it deciding under the deployment's policy",
			resolved.DailyDays, resolved.WeeklyMonths, resolved.MonthlyMonths)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return string(raw)
}

// exemptFromIsolationFixture names each config.BackupSet field that is
// legitimately absent from the isolation fixture, with the reason.
//
// Everything not named here MUST be non-zero in that fixture, and
// TestUpdateBackupSetIsolationFixtureExercisesEveryField enforces it.
// The point is not tidiness, it is that a whole-struct comparison over a
// field that is nil on BOTH sides proves nothing about that field: it
// passes forever no matter what the patch applier does to it. That is
// not hypothetical. It is exactly what happened to RetentionConfig, whose
// isolation was "covered" by a comparison that could never have caught
// it being dropped, until issue #333/#336 landed the field and a
// deliberate fixture was written for it.
//
// So a field added to config.BackupSet from here on fails this test until
// somebody decides which it is: exercised by the fixture, or exempt for a
// reason written down. That is a ledger, deliberately, because the thing
// being ledgered is a judgement someone has to make at the moment the
// field lands rather than a violation to be waved through.
var exemptFromIsolationFixture = map[string]string{
	"ID":           "assigned by Validate from the source and name, never read off the file, so a fixture cannot set it",
	"Retention":    "the RESOLVED policy, filled in by Validate and carrying yaml:\"-\", so it is never on disk to compare",
	"ReadOnly":     "the RESOLVED answer, filled in by Validate from ReadOnlyConfig; the override is what the fixture sets",
	"Disabled":     "a bool whose zero value IS its ordinary state (an enabled set), so \"non-zero\" cannot be required of it. UpdateBackupSetRequest cannot reach it either: enabling and disabling is POST /enabled's own route",
	"Revalidation": "issue #315's re-check schedule, which no update-path request field can reach and which config.Validate does not require",
}

// TestUpdateBackupSetIsolationFixtureExercisesEveryField is a control on
// the isolation test above rather than a test of the product.
//
// TestUpdateBackupSet_WritesOnlyTheFieldsTheRequestNames compares the
// WHOLE persisted config.BackupSet, which is what makes it grow with the
// schema. That is only true while the fixture actually SETS the fields it
// is comparing: a field the fixture leaves at its zero value is equal on
// both sides whatever the patch applier does to it, so the guarantee is
// hollow for exactly the fields nobody thought about.
//
// A fixture can therefore hollow out a test without anything about the
// test looking wrong, which is a level below the vacuous assertions this
// campaign has been hunting. This is the check that says so out loud.
func TestUpdateBackupSetIsolationFixtureExercisesEveryField(t *testing.T) {
	configPath := writeRichTestConfigFile(t)
	bs := readBackupSetFromDisk(t, configPath, "production", "postgres-primary")

	rt := reflect.TypeOf(bs)
	rv := reflect.ValueOf(bs)
	checked := 0
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		if reason, exempt := exemptFromIsolationFixture[name]; exempt {
			if reason == "" {
				t.Errorf("%s is exempt with no reason recorded; the reason is the whole value of the exemption", name)
			}
			continue
		}
		checked++
		if rv.Field(i).IsZero() {
			t.Errorf("config.BackupSet.%s is at its zero value in the isolation fixture, so TestUpdateBackupSet_WritesOnlyTheFieldsTheRequestNames compares it equal on both sides no matter what the patch applier does to it.\n"+
				"Either set it in writeRichTestConfigFile, or add it to exemptFromIsolationFixture with the reason it cannot be set.", name)
		}
	}

	if checked == 0 {
		t.Fatal("every field was exempt, so this control checked nothing")
	}

	// The exemption list may not name a field that no longer exists,
	// which is how a ledger rots into a list of excuses for things that
	// are not there any more.
	for name := range exemptFromIsolationFixture {
		if _, ok := rt.FieldByName(name); !ok {
			t.Errorf("exemptFromIsolationFixture names %q, which config.BackupSet no longer has; remove the entry", name)
		}
	}
}
