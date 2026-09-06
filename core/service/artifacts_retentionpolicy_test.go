// Issue #523: the backups list has to say which of its rows nothing will
// ever delete.
//
// Removing a backup set is config-only by design (#391), so its backups
// stay on storage and stay listed. What no surface outside a terminal
// could see is that they left every retention chain on the way out:
// retention walks the configuration, the configuration no longer names
// the set, so nothing selects those files, nothing expires them, and they
// hold their space until somebody removes them by hand. On a NAS with a
// ceiling that is a slow, silent problem, which is the worst kind.
//
// Every test here checks BOTH directions on the same run, and that is
// deliberate rather than thorough. A projection that always answered
// "configured" and a projection that always answered "none" are both
// perfectly consistent with a one-sided test, and both are broken: the
// first hides the rows this field exists to surface, and the second cries
// wolf on every healthy backup until an operator learns to ignore it.
package service

import (
	"context"
	"testing"
)

// policyByArtifact reads the whole unfiltered backups list into a lookup
// of artifact id to the policy the read model reports for it.
//
// Unfiltered, because that is the read the Backups page and `artifacts`
// both make, and it is the only one that carries a removed set's rows at
// all (ListArtifacts' own doc says why the quarantine read does not).
func policyByArtifact(t *testing.T, svc *BackupService) map[string]string {
	t.Helper()
	all, err := svc.ListArtifacts(context.Background(), ArtifactFilter{})
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	out := make(map[string]string, len(all))
	for _, a := range all {
		out[a.ID] = a.RetentionPolicy
	}
	return out
}

// setsWithPolicy lists the backup set ids whose artifacts report policy,
// in whatever order the list came back.
func setsWithPolicy(t *testing.T, svc *BackupService, policy string) []string {
	t.Helper()
	all, err := svc.ListArtifacts(context.Background(), ArtifactFilter{})
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	seen := map[string]bool{}
	var out []string
	for _, a := range all {
		if a.RetentionPolicy == policy && !seen[a.BackupSetID] {
			seen[a.BackupSetID] = true
			out = append(out, a.BackupSetID)
		}
	}
	return out
}

// TestListArtifacts_SaysWhichBackupsNoPolicyGovernsAndWhichAreStillGoverned
// is issue #523's acceptance at the service boundary, with the two
// backup sets of the removal fixture playing the two answers off each
// other.
//
// The fixture's second set is what makes this a test rather than an
// assertion about a constant. Remove alpha and beta stays configured, so
// the same call has to give two different answers on the same run: a
// projection stuck on either value fails here, and it fails on the half
// that names the mistake.
func TestListArtifacts_SaysWhichBackupsNoPolicyGovernsAndWhichAreStillGoverned(t *testing.T) {
	svc, _, _, _ := openRemovalFixtureService(t)
	ctx := context.Background()

	if got := cycleReportFrom(svc.state.Load().inner, svc.holds); len(got.Sets) == 0 {
		t.Fatal("the seeding cycle covered no sets, so there would be no backups to report a policy for")
	}

	// The control, before anything is removed. Without it, every "none"
	// below would be equally consistent with a read model that has never
	// said "configured" in its life.
	before := policyByArtifact(t, svc)
	if len(before) == 0 {
		t.Fatal("the seeding cycle journaled nothing, so this test could not tell a governed backup from an abandoned one")
	}
	for id, policy := range before {
		if policy != RetentionPolicyConfigured {
			t.Fatalf("%s reports RetentionPolicy %q before any removal, want %q: every set in this fixture is configured and every one of them is retained under a chain",
				id, policy, RetentionPolicyConfigured)
		}
	}

	alphaBefore := artifactIDsUnderSet(t, svc, "production/alpha")
	betaBefore := artifactIDsUnderSet(t, svc, "production/beta")
	if len(alphaBefore) == 0 || len(betaBefore) == 0 {
		t.Fatalf("the fixture journaled %d alpha and %d beta artifact(s); this test needs both sides to have rows or it can only prove one direction",
			len(alphaBefore), len(betaBefore))
	}

	if err := svc.RemoveBackupSet(ctx, "production/alpha"); err != nil {
		t.Fatalf("RemoveBackupSet: %v", err)
	}

	after := policyByArtifact(t, svc)
	for _, id := range alphaBefore {
		got, listed := after[id]
		if !listed {
			t.Fatalf("%s vanished from the backups list after its set was removed; the removal confirmation promises it stays listed", id)
		}
		if got != RetentionPolicyNone {
			t.Errorf("%s reports RetentionPolicy %q, want %q: its backup set's configuration is gone, so no chain will ever select it and nothing here will ever delete it",
				id, got, RetentionPolicyNone)
		}
	}
	for _, id := range betaBefore {
		if got := after[id]; got != RetentionPolicyConfigured {
			t.Errorf("%s reports RetentionPolicy %q, want %q: beta is still configured and its retention chain still ages these out. Flagging a governed backup as ungoverned teaches an operator to ignore the flag",
				id, got, RetentionPolicyConfigured)
		}
	}

	// And the two answers actually landed on different sets, rather than
	// the loops above agreeing because one of them iterated nothing.
	if ungoverned := setsWithPolicy(t, svc, RetentionPolicyNone); len(ungoverned) != 1 || ungoverned[0] != "production/alpha" {
		t.Errorf("the sets reporting %q are %v, want exactly [production/alpha]", RetentionPolicyNone, ungoverned)
	}
	if governed := setsWithPolicy(t, svc, RetentionPolicyConfigured); len(governed) != 1 || governed[0] != "production/beta" {
		t.Errorf("the sets reporting %q are %v, want exactly [production/beta]", RetentionPolicyConfigured, governed)
	}
}

// TestGetArtifact_SaysWhichPolicyGovernsOneBackup is the same claim on the
// single-artifact read, because GET /api/v1/backups/{id} is where an
// operator lands after clicking the row the list flagged, and a detail
// page that silently disagreed with the list it came from would be worse
// than either answer alone.
func TestGetArtifact_SaysWhichPolicyGovernsOneBackup(t *testing.T) {
	svc, _, _, _ := openRemovalFixtureService(t)
	ctx := context.Background()

	if got := cycleReportFrom(svc.state.Load().inner, svc.holds); len(got.Sets) == 0 {
		t.Fatal("the seeding cycle covered no sets")
	}
	alpha := artifactIDsUnderSet(t, svc, "production/alpha")
	beta := artifactIDsUnderSet(t, svc, "production/beta")
	if len(alpha) == 0 || len(beta) == 0 {
		t.Fatalf("the fixture journaled %d alpha and %d beta artifact(s); both sides are needed", len(alpha), len(beta))
	}

	for _, id := range append(append([]string{}, alpha...), beta...) {
		got, err := svc.GetArtifact(ctx, id)
		if err != nil {
			t.Fatalf("GetArtifact(%s): %v", id, err)
		}
		if got.RetentionPolicy != RetentionPolicyConfigured {
			t.Fatalf("GetArtifact(%s).RetentionPolicy = %q before any removal, want %q", id, got.RetentionPolicy, RetentionPolicyConfigured)
		}
	}

	if err := svc.RemoveBackupSet(ctx, "production/alpha"); err != nil {
		t.Fatalf("RemoveBackupSet: %v", err)
	}

	for _, id := range alpha {
		got, err := svc.GetArtifact(ctx, id)
		if err != nil {
			t.Fatalf("GetArtifact(%s) after removal: %v", id, err)
		}
		if got.RetentionPolicy != RetentionPolicyNone {
			t.Errorf("GetArtifact(%s).RetentionPolicy = %q, want %q", id, got.RetentionPolicy, RetentionPolicyNone)
		}
	}
	for _, id := range beta {
		got, err := svc.GetArtifact(ctx, id)
		if err != nil {
			t.Fatalf("GetArtifact(%s) after removal: %v", id, err)
		}
		if got.RetentionPolicy != RetentionPolicyConfigured {
			t.Errorf("GetArtifact(%s).RetentionPolicy = %q, want %q: removing alpha must not change what governs beta", id, got.RetentionPolicy, RetentionPolicyConfigured)
		}
	}
}

// TestListArtifacts_NeverReportsAPolicyItWasNotAskedFor. Every row the
// boundary serves carries one of the two values and never the empty
// string, because an empty one downstream is a third, unnamed state that
// a mapper has to guess at, and the guess this product has already got
// wrong once is the one that reads a missing answer as a healthy one.
func TestListArtifacts_NeverReportsAPolicyItWasNotAskedFor(t *testing.T) {
	svc, _, _, _ := openRemovalFixtureService(t)
	ctx := context.Background()

	if got := cycleReportFrom(svc.state.Load().inner, svc.holds); len(got.Sets) == 0 {
		t.Fatal("the seeding cycle covered no sets")
	}
	if err := svc.RemoveBackupSet(ctx, "production/alpha"); err != nil {
		t.Fatalf("RemoveBackupSet: %v", err)
	}

	all, err := svc.ListArtifacts(ctx, ArtifactFilter{})
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no artifacts listed, so this checked nothing")
	}
	for _, a := range all {
		switch a.RetentionPolicy {
		case RetentionPolicyConfigured, RetentionPolicyNone:
		default:
			t.Errorf("%s reports RetentionPolicy %q, which is neither %q nor %q; a caller cannot render a value nobody defined and will fall back to whichever answer its author found convenient",
				a.ID, a.RetentionPolicy, RetentionPolicyConfigured, RetentionPolicyNone)
		}
	}
}

// TestConfiguredSetIndex_ReadsAnUnknownSetAsUngoverned pins the direction
// the lookup falls in, on its own, away from any fixture.
//
// It is the smallest statement of the rule the rest of this file exercises
// end to end: an id the running configuration does not hold is an id
// nothing walks, and the honest answer about it is "no policy", never the
// reassuring one.
func TestConfiguredSetIndex_ReadsAnUnknownSetAsUngoverned(t *testing.T) {
	index := configuredSetIndex{"production/alpha": true}

	if got := index.retentionPolicyFor("production/alpha"); got != RetentionPolicyConfigured {
		t.Errorf("retentionPolicyFor(a configured set) = %q, want %q", got, RetentionPolicyConfigured)
	}
	if got := index.retentionPolicyFor("production/gone"); got != RetentionPolicyNone {
		t.Errorf("retentionPolicyFor(a set the configuration does not name) = %q, want %q", got, RetentionPolicyNone)
	}
	// The degenerate case a fixture cannot reach: nothing configured at
	// all. It must not read as "everything is fine" either.
	if got := (configuredSetIndex{}).retentionPolicyFor("production/alpha"); got != RetentionPolicyNone {
		t.Errorf("retentionPolicyFor against an empty index = %q, want %q", got, RetentionPolicyNone)
	}
}
