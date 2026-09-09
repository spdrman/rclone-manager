package cliecho

import (
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/apicontract"
)

// TestEveryRouteAnswersWithACommandOrANamedGap is this package's own half
// of the parity claim. The other half, that the route TABLE matches the
// ROUTER, is in apps/common/webhost, which is the only place that can see
// the router.
func TestEveryRouteAnswersWithACommandOrANamedGap(t *testing.T) {
	for _, route := range Routes() {
		method, path, _ := strings.Cut(route, " ")
		e := routes[route]
		if e.build == nil && strings.TrimSpace(e.why) == "" {
			t.Errorf("%s has neither a command builder nor a reason there is none. A gap that says only that something is missing is not actionable; say what verb would have to exist.", route)
			continue
		}
		line := Echo(Action{Method: method, Route: path})
		if len(line.Command) == 0 && line.Gap == "" {
			t.Errorf("%s printed nothing at all. Silence is the thing this feature exists to abolish.", route)
		}
		if len(line.Command) > 0 && line.Gap != "" {
			t.Errorf("%s printed both a command and a gap", route)
		}
		if !strings.HasPrefix(line.Route, method+" /api/v1/") {
			t.Errorf("%s names itself %q; a gap has to name the endpoint that needs a verb, in the form an operator would find it in the API", route, line.Route)
		}
	}
}

// A command line is copy-pasteable as typed and carries nothing that is
// not the command, and a gap names its route in the form an operator would
// find it in the API. Both of those are format claims and both are
// load-bearing, so they are pinned rather than left to the renderer.
func TestTheLineFormat(t *testing.T) {
	gap := Echo(Action{Method: "POST", Route: "/backup-sets/test-connection"})
	if gap.Shell() != "" || len(gap.Command) != 0 {
		t.Errorf("a gap carries the command %q", gap.Shell())
	}
	if got, want := gap.Gap, "no "+Binary+" equivalent yet"; got != want {
		t.Errorf("a gap says %q, want %q; an operator and a grep only ever have one string to know", got, want)
	}
	if got, want := gap.Route, "POST /api/v1/backup-sets/test-connection"; got != want {
		t.Errorf("a gap names route %q, want %q", got, want)
	}
	if !strings.Contains(gap.GapDetail, "there is no verb") {
		t.Errorf("the gap carries no reason: %q", gap.GapDetail)
	}

	patch := Echo(Action{
		Method: "PATCH", Route: "/backup-sets/{source}/{set}",
		Params: map[string]string{"source": "api-server", "set": "var-backups"},
		Body:   []byte(`{"stale_after_seconds":172800}`),
	})
	if got, want := patch.Shell(), Binary+" backup-set patch api-server/var-backups --stale-after 48h"; got != want {
		t.Errorf("a patch prints\n  %s\nwant\n  %s", got, want)
	}

	// The prompt is the terminal's, not the command's. What goes on the
	// wire and into the journal is something a script can hand to a
	// shell; the dock draws "$ " in front of it the same way it draws
	// "# " in front of a gap and a timestamp in front of everything.
	if strings.HasPrefix(patch.Shell(), "$") {
		t.Errorf("the command carries a prompt: %q", patch.Shell())
	}

	// A value with a space in it is quoted, because the point of the line
	// is that it can be pasted. A known_hosts line is the case that
	// actually occurs.
	create := Echo(Action{
		Method: "POST", Route: "/backup-sets",
		Body: []byte(`{"source_name":"api-server","name":"var-backups","host":"10.0.0.14","user":"backups","remote_path":"/var/backups","local_path":"/data/backups","ssh_key_id":"key_1","known_hosts_line":"10.0.0.14 ssh-ed25519 AAAAC3Nz","completion_strategy":"rename"}`),
	})
	if !strings.Contains(create.Shell(), "--known-hosts-line '10.0.0.14 ssh-ed25519 AAAAC3Nz'") {
		t.Errorf("a value with a space in it is not quoted, so the command is not pasteable:\n%s", create.Shell())
	}
}

// A create refused for a missing field echoes the command that was asked
// for, with the missing field missing, rather than --user followed by an
// empty string.
//
// The two are different commands: an empty user is a value, not an
// absence, and a line carrying one is not something an operator can fill
// in and run. It would be refused again for a different reason.
func TestCreateSkipsTheFieldsTheRequestDidNotCarry(t *testing.T) {
	partial := Echo(Action{
		Method: "POST", Route: "/backup-sets",
		Body: []byte(`{"source_name":"api-server","name":"var-backups","host":"10.0.0.14"}`),
	})
	if got, want := partial.Shell(), Binary+" backup-set create api-server/var-backups --host 10.0.0.14"; got != want {
		t.Errorf("a create missing every field but the host prints\n  %s\nwant\n  %s", got, want)
	}
	if strings.Contains(partial.Shell(), "''") {
		t.Errorf("the command carries an empty value: %s", partial.Shell())
	}

	// The control: the same fields, when the request DOES carry them, are
	// still echoed, so the case above is about absence and not about the
	// builder having stopped emitting the flags.
	full := Echo(Action{
		Method: "POST", Route: "/backup-sets",
		Body: []byte(`{"source_name":"api-server","name":"var-backups","host":"10.0.0.14","user":"backups","remote_path":"/var/backups","local_path":"/data/backups","ssh_key_id":"key_1","known_hosts_line":"10.0.0.14 ssh-ed25519 AAAAC3Nz","completion_strategy":"rename"}`),
	})
	for _, flag := range []string{"--host 10.0.0.14", "--user backups", "--remote-path /var/backups", "--local-path /data/backups", "--ssh-key-id key_1", "--known-hosts-line '10.0.0.14 ssh-ed25519 AAAAC3Nz'", "--completion-strategy rename"} {
		if !strings.Contains(full.Shell(), flag) {
			t.Errorf("a full create no longer carries %s:\n  %s", flag, full.Shell())
		}
	}
}

// TestAStorageMediumWriteEchoesTheSkipOnlyWhenItWasAskedFor is issue
// #636's opt-out on this surface.
//
// A request that skipped the check and a line that did not would be a
// printed command doing something other than the action it claims to be
// the equivalent of, which is the one failure this whole surface exists to
// prevent. And a line that named --no-verify for a save that DID check
// would teach an operator to skip by default.
//
// The candidate preflight is the control, and it is the reason the flag is
// not folded into mediumSpecFlags: that route IS the check, so `medium
// test-connection --candidate --no-verify` is not an instruction anybody
// can mean.
func TestAStorageMediumWriteEchoesTheSkipOnlyWhenItWasAskedFor(t *testing.T) {
	const spec = `"id":"offsite_s3","type":"s3","region":"eu-central-1","bucket":"acme-backups","credentials":{"credentials_id":"cred_01HX"}`

	skipping := Echo(Action{Method: "POST", Route: "/storage-mediums",
		Body: []byte(`{` + spec + `,"skip_connection_check":true}`)})
	if !strings.Contains(skipping.Shell(), "--no-verify") {
		t.Errorf("a create that skipped the check echoes a line that runs it:\n  %s", skipping.Shell())
	}

	checking := Echo(Action{Method: "POST", Route: "/storage-mediums", Body: []byte(`{` + spec + `}`)})
	if strings.Contains(checking.Shell(), "--no-verify") {
		t.Errorf("a create that asked for the check echoes a line that skips it:\n  %s", checking.Shell())
	}

	edit := Echo(Action{Method: "PUT", Route: "/storage-mediums/{id}",
		Params: map[string]string{"id": "offsite_s3"},
		Body:   []byte(`{"region":"eu-west-1","skip_connection_check":true}`)})
	if !strings.Contains(edit.Shell(), "--no-verify") {
		t.Errorf("an edit that skipped the check echoes a line that runs it:\n  %s", edit.Shell())
	}

	candidate := Echo(Action{Method: "POST", Route: "/storage-mediums/preflight",
		Body: []byte(`{` + spec + `,"skip_connection_check":true}`)})
	if strings.Contains(candidate.Shell(), "--no-verify") {
		t.Errorf("a candidate preflight echoes a flag that would tell the check not to run:\n  %s", candidate.Shell())
	}
}

// The gap the issue singles out, because it is the one a lazier
// implementation gets wrong. `backup-manager run` exists, and printing it
// here would print a command that does something different to a different
// process.
func TestRunAllDueSetsPrintsTheGapAndNotBackupManagerRun(t *testing.T) {
	line := Echo(Action{Method: "POST", Route: "/operations", Body: []byte(`{"action":"` + apicontract.ActionRunCycle + `","config_revision":"r1"}`)})
	if len(line.Command) != 0 {
		t.Fatalf("run_cycle printed the command %v; `"+Binary+" run` opens the service in the operator's own process and runs a cycle THERE, so it is a different act against a different process",
			line.Command)
	}
	if line.Route != "POST /api/v1/operations" {
		t.Errorf("the gap does not name the route that needs a verb: %q", line.Route)
	}
	if !strings.Contains(line.GapDetail, "not in this engine") {
		t.Errorf("the gap does not say why `"+Binary+" run` is not the answer: %q", line.GapDetail)
	}

	// The same route with a restore on it DOES have a verb, which is what
	// makes the refusal above about the request rather than about the
	// route.
	restore := Echo(Action{Method: "POST", Route: "/operations",
		Body: []byte(`{"action":"` + apicontract.ActionRestorePlacement + `","config_revision":"r1","restore":{"artifact_id":"api-server/var-backups/dump.tar","medium":"offsite_s3","window_days":7,"acknowledged":true}}`)})
	if got, want := restore.Shell(), Binary+" restore api-server/var-backups/dump.tar --medium offsite_s3 --days 7 --acknowledge"; got != want {
		t.Errorf("a restore prints\n  %s\nwant\n  %s", got, want)
	}
}
