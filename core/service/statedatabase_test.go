package service

import "testing"

// Issue #571's second half: the guarantee that a `backup-set create` typed
// on a host finds the engine serving it rests on the CLI and the web host
// naming the SAME journal. These two hold the definition itself; the check
// that both surfaces actually take their default from it reads a file in
// each module, so it lives in apps/generic/cmd/backup-manager-web, which is
// allowed to look down into core. It cannot live here: the dependency rule
// is proved by deleting apps/ and running core's tests, so a core test that
// reads apps/ fails the whole gate rather than the thing it is checking.

func TestStateDatabaseDefault_IsThePackagedPathWhenNothingSaysOtherwise(t *testing.T) {
	t.Setenv(StateDatabaseEnv, "")
	if got := StateDatabaseDefault(); got != DefaultStateDatabase {
		t.Fatalf("StateDatabaseDefault() = %q with %s unset, want %q", got, StateDatabaseEnv, DefaultStateDatabase)
	}
}

func TestStateDatabaseDefault_IsWhatTheEnvironmentSays(t *testing.T) {
	t.Setenv(StateDatabaseEnv, "/somewhere/else/state.db")
	if got := StateDatabaseDefault(); got != "/somewhere/else/state.db" {
		t.Fatalf("StateDatabaseDefault() = %q with %s set, want the value the environment named; an override that reaches only one of the two surfaces points them at different deployments",
			got, StateDatabaseEnv)
	}
}
