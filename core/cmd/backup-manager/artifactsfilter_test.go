package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What `artifacts --backup-set` accepts, and what it refuses, on the one
// deployment shape where the answer matters (issue #569).
//
// Every fixture here has TWO sources whose backup sets share a name, which
// is not an exotic arrangement: it is what one naming convention applied
// across a fleet of hosts looks like, and it is the only shape that can
// tell a working filter from a broken one. A single-set fixture passes
// against a filter that ignores the source entirely, which is exactly the
// bug this file was written against: `--backup-set api-server/var-backups`
// was refused as "no configured backup set named api-server/var-backups"
// while that set was configured, listed by `sources` and reported on by
// `status`, and `--backup-set var-backups` was accepted and answered with
// both hosts' artifacts at once, with nothing in the output saying a
// second set had matched.
//
// The two artifacts under the shared name are deliberately called the same
// thing on both hosts, for the same reason: a convention applied across a
// fleet produces the same filenames as well as the same set names, so the
// only thing separating the rows an operator reads is the source half of
// the id. A test whose two hosts held differently named files would pass
// while printing the wrong host's row.

// writeTwoSourceTestConfig builds a deployment with two sources whose
// backup sets share the name `var-backups`, plus one set with a name only
// one source uses, so a test can drive both the ambiguous and the
// unambiguous bare name against one config.
//
// It is writeTestConfigIn's fixture widened rather than a new kind of
// fixture: real temp directories standing in as remotes, wired through the
// `local` transport backend, so a `run` here performs a real discover,
// transfer, verify and commit with no network and no Docker.
func writeTwoSourceTestConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	remoteFor := func(set, artifact string) string {
		remoteDir := filepath.Join(dir, set, "remote")
		if err := os.MkdirAll(remoteDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(filepath.Join(remoteDir, artifact), []byte("payload for "+set), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		return remoteDir
	}

	backupSet := func(id, remoteDir, localDir string) string {
		return "      - id: " + id + "\n" +
			"        remote:\n" +
			"          type: local\n" +
			"        remote_path: " + remoteDir + "\n" +
			"        local_path: " + localDir + "\n" +
			"        include:\n" +
			"          - \"*.tar\"\n" +
			"        completion:\n" +
			"          strategy: rename\n" +
			"        stale_after: 24h\n"
	}

	configPath := filepath.Join(dir, "config.yaml")
	content := "poll_interval: 15m\n" +
		"state:\n" +
		"  database: " + filepath.Join(dir, "state.db") + "\n" +
		"sources:\n" +
		"  - id: api-server\n" +
		"    backup_sets:\n" +
		backupSet("var-backups",
			remoteFor("api-var", "alternatives.tar"),
			filepath.Join(dir, "api-var", "local")) +
		backupSet("etc-backups",
			remoteFor("api-etc", "etc.tar"),
			filepath.Join(dir, "api-etc", "local")) +
		"  - id: cicd-pipeline\n" +
		"    backup_sets:\n" +
		backupSet("var-backups",
			remoteFor("cicd-var", "alternatives.tar"),
			filepath.Join(dir, "cicd-var", "local")) +
		"retention:\n" +
		"  timezone: UTC\n" +
		"  week_starts_on: monday\n"
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return configPath
}

// twoSourceDeployment writes the fixture above and runs one real cycle
// over it, so every assertion below is made against a journal that has
// three committed artifacts in it rather than against an empty one. An
// empty journal would let a filter that selects nothing look identical to
// a filter that selects the right thing.
func twoSourceDeployment(t *testing.T) string {
	t.Helper()
	configPath := writeTwoSourceTestConfig(t)
	if got := run([]string{"run", "--config", configPath}); got != 0 {
		t.Fatalf("run([\"run\", \"--config\", %q]) = %d, want 0 (these tests need three committed artifacts)", configPath, got)
	}
	return configPath
}

// The three artifact ids the fixture commits, each of which is the whole
// of what separates one printed row from another here: the two under the
// shared set name differ only in their source half.
const (
	apiVarArtifact  = "api-server/var-backups/alternatives.tar"
	cicdVarArtifact = "cicd-pipeline/var-backups/alternatives.tar"
	apiEtcArtifact  = "api-server/etc-backups/etc.tar"
)

// TestArtifacts_BackupSetTakesTheCompositeIdEveryOtherSurfacePrints is
// issue #569's first half.
//
// The id in this command's own first column, in `sources`, in `status` and
// in `retention`'s operand is `source/set`. Typing that at the one flag
// whose whole job is to select a backup set was a refusal, and the refusal
// named the set as unconfigured, which was untrue of it.
func TestArtifacts_BackupSetTakesTheCompositeIdEveryOtherSurfacePrints(t *testing.T) {
	configPath := twoSourceDeployment(t)

	var code int
	stdout := captureStdout(t, func() {
		code = run([]string{"artifacts", "--config", configPath, "--backup-set", "api-server/var-backups"})
	})

	if code != 0 {
		t.Fatalf("artifacts --backup-set api-server/var-backups = %d, want 0; it printed %q", code, stdout)
	}
	if !strings.Contains(stdout, apiVarArtifact) {
		t.Errorf("artifacts --backup-set api-server/var-backups printed %q, want the row for %s", stdout, apiVarArtifact)
	}
	if strings.Contains(stdout, cicdVarArtifact) {
		t.Errorf("artifacts --backup-set api-server/var-backups printed %q, which carries %s: the filter named one host and answered with another's backups", stdout, cicdVarArtifact)
	}
	if !strings.Contains(stdout, "1 artifact(s)") {
		t.Errorf("artifacts --backup-set api-server/var-backups printed %q, want a count of exactly 1", stdout)
	}
}

// TestArtifacts_BackupSetRefusesABareNameTwoSourcesShare is the sharper
// half of #569, and the reason this file's fixture has two sources.
//
// A bare name that two sources both configure identifies no backup set at
// all (FR-7: identity is source-plus-set, never set alone). Answering it
// with one set's rows, or with both sets' rows merged, gives an operator
// filtering for one host another host's backups and nothing to doubt. So
// it is refused, and the refusal names both candidates, because the whole
// remedy is retyping one of them.
func TestArtifacts_BackupSetRefusesABareNameTwoSourcesShare(t *testing.T) {
	configPath := twoSourceDeployment(t)

	var code int
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			code = run([]string{"artifacts", "--config", configPath, "--backup-set", "var-backups"})
		})
	})

	if code != 1 {
		t.Errorf("artifacts --backup-set var-backups = %d, want 1: two sources configure that name, so it names no backup set", code)
	}
	if strings.Contains(stdout, apiVarArtifact) || strings.Contains(stdout, cicdVarArtifact) {
		t.Errorf("artifacts --backup-set var-backups printed %q; an ambiguous filter must print no rows at all rather than one host's or both", stdout)
	}
	for _, want := range []string{"api-server/var-backups", "cicd-pipeline/var-backups"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal was %q, and it does not name %s; an operator cannot retype what they are not shown", stderr, want)
		}
	}
}

// TestArtifacts_BackupSetStillTakesAnUnambiguousBareName keeps the form
// operators already have in their shell history working.
//
// This is the positive control for the test above: a refusal that fired on
// every bare name would satisfy it, and would break every script that
// filters on a set name only one source uses.
func TestArtifacts_BackupSetStillTakesAnUnambiguousBareName(t *testing.T) {
	configPath := twoSourceDeployment(t)

	var code int
	stdout := captureStdout(t, func() {
		code = run([]string{"artifacts", "--config", configPath, "--backup-set", "etc-backups"})
	})

	if code != 0 {
		t.Fatalf("artifacts --backup-set etc-backups = %d, want 0: only one source configures that name", code)
	}
	if !strings.Contains(stdout, apiEtcArtifact) {
		t.Errorf("artifacts --backup-set etc-backups printed %q, want the row for %s", stdout, apiEtcArtifact)
	}
	if !strings.Contains(stdout, "1 artifact(s)") {
		t.Errorf("artifacts --backup-set etc-backups printed %q, want a count of exactly 1", stdout)
	}
}

// TestArtifacts_BackupSetRefusesAnIdNamingNothing keeps #187's rule
// intact through the new id form: a filter that names nothing is refused
// by name, never answered with an empty listing, because "there is no such
// backup set" and "this backup set has no backups yet" call for opposite
// responses from an operator.
//
// Each case asserts the whole refusal rather than that the typed string
// appears somewhere in it, because the half that is missing is the useful
// part of the answer. A composite id whose SOURCE is the typo sends an
// operator looking at the wrong host's configuration if it is reported as
// a missing backup set.
func TestArtifacts_BackupSetRefusesAnIdNamingNothing(t *testing.T) {
	configPath := twoSourceDeployment(t)

	for _, tc := range []struct {
		name string
		flag string
		want string
	}{
		{
			name: "a set name no source configures",
			flag: "no-such-set",
			want: "no configured backup set named no-such-set",
		},
		{
			name: "a composite id whose set half is not there",
			flag: "api-server/no-such-set",
			want: "no configured backup set named api-server/no-such-set",
		},
		{
			name: "a composite id whose source half is not there",
			flag: "no-such-host/var-backups",
			want: "no configured source named no-such-host",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			var stdout string
			stderr := captureStderr(t, func() {
				stdout = captureStdout(t, func() {
					code = run([]string{"artifacts", "--config", configPath, "--backup-set", tc.flag})
				})
			})

			if code != 1 {
				t.Errorf("artifacts --backup-set %s = %d, want 1", tc.flag, code)
			}
			if strings.TrimSpace(stdout) != "" {
				t.Errorf("artifacts --backup-set %s printed %q on stdout, want nothing: a refusal is not a listing", tc.flag, stdout)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("artifacts --backup-set %s was refused with %q, which does not name %q", tc.flag, stderr, tc.want)
			}
		})
	}
}

// TestArtifacts_BackupSetRefusesASourceItsOwnIdContradicts is the one
// mistake the new id form makes possible: a composite id names a source,
// so `--source` naming a different one is two flags asking for two
// different things.
//
// It exits 2 rather than 1, and that is the same call every other wrong
// command line here gets: nothing about it depends on what is configured,
// the same two flags are a contradiction on every deployment there is, and
// the fix is to retype the command rather than to go and look at
// config.yaml. It is decided before anything is opened, for the same
// reason.
func TestArtifacts_BackupSetRefusesASourceItsOwnIdContradicts(t *testing.T) {
	configPath := twoSourceDeployment(t)

	var code int
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			code = run([]string{"artifacts", "--config", configPath, "--source", "cicd-pipeline", "--backup-set", "api-server/var-backups"})
		})
	})

	if code != 2 {
		t.Errorf("artifacts --source cicd-pipeline --backup-set api-server/var-backups = %d, want 2: the command line contradicts itself", code)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("that invocation printed %q on stdout, want nothing at all", stdout)
	}
	if !strings.Contains(stderr, "cicd-pipeline") || !strings.Contains(stderr, "api-server/var-backups") {
		t.Errorf("the refusal was %q, want it to name both halves of the contradiction", stderr)
	}
}

// TestArtifacts_SourceAndCompositeBackupSetAgreeingIsAccepted is the
// positive control for the test above: a check that refused whenever both
// flags were given would satisfy it, and `--source api-server
// --backup-set api-server/var-backups` says one thing twice rather than
// two things.
func TestArtifacts_SourceAndCompositeBackupSetAgreeingIsAccepted(t *testing.T) {
	configPath := twoSourceDeployment(t)

	var code int
	stdout := captureStdout(t, func() {
		code = run([]string{"artifacts", "--config", configPath, "--source", "api-server", "--backup-set", "api-server/var-backups"})
	})

	if code != 0 {
		t.Fatalf("artifacts --source api-server --backup-set api-server/var-backups = %d, want 0", code)
	}
	if !strings.Contains(stdout, apiVarArtifact) || strings.Contains(stdout, cicdVarArtifact) {
		t.Errorf("that invocation printed %q, want exactly the row for %s", stdout, apiVarArtifact)
	}
}

// --- the same flag, on the other command that has one ---
//
// `fetch` is the only other command whose backup set arrives as a flag
// rather than as an operand, and it is the only other one that refused the
// id every surface prints. It does not share the dangerous half of #569:
// it has always required a source, so the pair it looks a set up by is
// exact and a name two sources share can never resolve to one of them
// behind an operator's back. What it shared is the annoying half, and one
// flag on one binary should not mean two different things.

// TestFetch_BackupSetTakesTheCompositeIdEveryOtherSurfacePrints drives the
// preview rather than a real cycle, because what is under test is which
// backup set the id resolved to and --dry-run answers exactly that
// question while touching neither a journal nor a remote.
//
// The etc-backups case is the one that proves the resolution rather than
// merely surviving it: it is a different set under the same source, with a
// remote of its own, so a fetch that landed on the wrong one previews the
// wrong file.
func TestFetch_BackupSetTakesTheCompositeIdEveryOtherSurfacePrints(t *testing.T) {
	configPath := writeTwoSourceTestConfig(t)

	for _, tc := range []struct {
		id      string
		want    string
		notWant string
	}{
		{id: "api-server/etc-backups", want: "etc.tar", notWant: "alternatives.tar"},
		{id: "cicd-pipeline/var-backups", want: "alternatives.tar", notWant: "etc.tar"},
	} {
		t.Run(tc.id, func(t *testing.T) {
			var code int
			stdout := captureStdout(t, func() {
				code = run([]string{"fetch", "--config", configPath, "--backup-set", tc.id, "--dry-run"})
			})

			if code != 0 {
				t.Fatalf("fetch --backup-set %s --dry-run = %d, want 0", tc.id, code)
			}
			if !strings.Contains(stdout, tc.want) || strings.Contains(stdout, tc.notWant) {
				t.Errorf("fetch --backup-set %s --dry-run previewed %q, want %s and not %s", tc.id, stdout, tc.want, tc.notWant)
			}
		})
	}
}

// TestFetch_StillRequiresASourceForAPlainSetName is the positive control
// for the test above, and it is the one that keeps this command's own
// contract: the source is not optional, it is allowed to arrive inside the
// id. A plain set name still needs it, with the wording that refusal has
// always had, and two flags naming two different sources is the same
// contradiction `artifacts` refuses.
func TestFetch_StillRequiresASourceForAPlainSetName(t *testing.T) {
	configPath := writeTwoSourceTestConfig(t)

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "a plain set name with no source",
			args: []string{"fetch", "--config", configPath, "--backup-set", "var-backups", "--dry-run"},
			want: "fetch requires --source and --backup-set",
		},
		{
			name: "neither flag at all",
			args: []string{"fetch", "--config", configPath, "--dry-run"},
			want: "fetch requires --source and --backup-set",
		},
		{
			name: "a source the id contradicts",
			args: []string{"fetch", "--config", configPath, "--source", "cicd-pipeline", "--backup-set", "api-server/var-backups", "--dry-run"},
			want: "contradicts",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			var stdout string
			stderr := captureStderr(t, func() {
				stdout = captureStdout(t, func() { code = run(tc.args) })
			})

			if code != 2 {
				t.Errorf("run(%v) = %d, want 2", tc.args, code)
			}
			if strings.TrimSpace(stdout) != "" {
				t.Errorf("run(%v) printed %q on stdout, want nothing", tc.args, stdout)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("run(%v) was refused with %q, want it to say %q", tc.args, stderr, tc.want)
			}
		})
	}
}
