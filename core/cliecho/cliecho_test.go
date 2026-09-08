package cliecho

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

// TestNoBuilderEverPrintsACredential is the mechanical form of "a command
// never carries a key, a password or a token".
//
// The panel these lines appear in is exportable and copy-to-clipboard by
// design, so a credential reaching one is a credential in whatever issue
// tracker the export lands in. Getting that right at each call site is not
// a rule, it is a habit, so this drives EVERY builder with a request body
// carrying every secret-shaped field this contract has and fails if any
// value reaches the argv. A builder added later that reads a new secret
// field fails here without anybody remembering to check.
func TestNoBuilderEverPrintsACredential(t *testing.T) {
	// Distinctive values, so a substring match cannot fire on a
	// coincidence and cannot miss a value that was mangled on the way.
	secrets := map[string]string{
		"private_key_pem": "-----BEGIN OPENSSH PRIVATE KEY----- cliechoSECRETpem",
		"passphrase":      "cliechoSECRETpassphrase",
		"password":        "cliechoSECRETpassword",
		"token":           "cliechoSECRETtoken",
		"secret":          "cliechoSECRETsecret",
		"secret_key":      "cliechoSECRETsecretkey",
		"access_key":      "cliechoSECRETaccesskey",
		"credentials":     "cliechoSECRETcredentials",
	}
	body, err := json.Marshal(secrets)
	if err != nil {
		t.Fatalf("marshalling the secret body: %v", err)
	}

	params := map[string]string{"source": "api-server", "set": "var-backups", "name": "dump.tar", "id": "offsite_s3"}
	query := url.Values{}
	for k, v := range secrets {
		query.Set(k, v)
	}

	checked := 0
	for _, route := range Routes() {
		method, path, _ := strings.Cut(route, " ")
		line := Echo(Action{Method: method, Route: path, Params: params, Query: query, Body: body})
		checked++
		printed := strings.Join(line.Command, " ") + " " + line.Shell()
		for field, value := range secrets {
			if strings.Contains(printed, value) {
				t.Errorf("%s prints the request's %s field:\n  %s\nThis panel is exportable and copy-to-clipboard, so a value that reaches it reaches whatever issue tracker the export lands in. Name the flag with a placeholder instead.",
					route, field, printed)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no routes were checked, so this test proves nothing")
	}
}

// TestNoBuilderEverPrintsACredential_WouldCatchOne is that test's control.
// It is a negative assertion over a table, which is the shape that most
// easily degrades into checking nothing at all.
func TestNoBuilderEverPrintsACredential_WouldCatchOne(t *testing.T) {
	leak := newCmd("backup-set", "create", "a/b").flag("ssh-key-file", "cliechoSECRETpem")
	if !strings.Contains(strings.Join(leak.argv, " "), "cliechoSECRETpem") {
		t.Fatal("a command built with a secret as a flag value does not contain it, so the substring search above could never fire")
	}
	safe := newCmd("backup-set", "create", "a/b").placeholderFlag("ssh-key-file", "the private key file you chose")
	if strings.Contains(strings.Join(safe.argv, " "), "cliechoSECRETpem") {
		t.Fatal("a placeholder still carries the value")
	}
	if !safe.placeholder {
		t.Fatal("a command carrying a placeholder does not say it is not runnable as printed, so an operator would paste it and get a file called <the private key file you chose>")
	}
}

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
	if got, want := gap.Gap, "no backup-manager equivalent yet"; got != want {
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
	if got, want := patch.Shell(), "backup-manager backup-set patch api-server/var-backups --stale-after 48h"; got != want {
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
	if got, want := partial.Shell(), "backup-manager backup-set create api-server/var-backups --host 10.0.0.14"; got != want {
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

// The gap the issue singles out, because it is the one a lazier
// implementation gets wrong. `backup-manager run` exists, and printing it
// here would print a command that does something different to a different
// process.
func TestRunAllDueSetsPrintsTheGapAndNotBackupManagerRun(t *testing.T) {
	line := Echo(Action{Method: "POST", Route: "/operations", Body: []byte(`{"action":"run_cycle","config_revision":"r1"}`)})
	if len(line.Command) != 0 {
		t.Fatalf("run_cycle printed the command %v; `backup-manager run` opens the service in the operator's own process and runs a cycle THERE, so it is a different act against a different process",
			line.Command)
	}
	if line.Route != "POST /api/v1/operations" {
		t.Errorf("the gap does not name the route that needs a verb: %q", line.Route)
	}
	if !strings.Contains(line.GapDetail, "not in this engine") {
		t.Errorf("the gap does not say why `backup-manager run` is not the answer: %q", line.GapDetail)
	}

	// The same route with a restore on it DOES have a verb, which is what
	// makes the refusal above about the request rather than about the
	// route.
	restore := Echo(Action{Method: "POST", Route: "/operations",
		Body: []byte(`{"action":"restore","config_revision":"r1","restore":{"artifact_id":"api-server/var-backups/dump.tar","medium":"offsite_s3","window_days":7,"acknowledged":true}}`)})
	if got, want := restore.Shell(), "backup-manager restore api-server/var-backups/dump.tar --medium offsite_s3 --days 7 --acknowledge"; got != want {
		t.Errorf("a restore prints\n  %s\nwant\n  %s", got, want)
	}
}
