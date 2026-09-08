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
		printed := strings.Join(line.Command, " ") + " " + line.Text()
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

// A gap line is greppable and names its route, and a command line is
// copy-pasteable as typed. Both of those are format claims and both are
// load-bearing, so they are pinned rather than left to the renderer.
func TestTheLineFormat(t *testing.T) {
	gap := Echo(Action{Method: "POST", Route: "/backup-sets/test-connection"})
	want := "# no backup-manager equivalent yet · POST /api/v1/backup-sets/test-connection"
	if got := strings.SplitN(gap.Text(), "\n", 2)[0]; got != want {
		t.Errorf("a gap prints\n  %s\nwant\n  %s", got, want)
	}
	if !strings.Contains(gap.Text(), "there is no verb") {
		t.Errorf("the gap carries no reason:\n%s", gap.Text())
	}

	patch := Echo(Action{
		Method: "PATCH", Route: "/backup-sets/{source}/{set}",
		Params: map[string]string{"source": "api-server", "set": "var-backups"},
		Body:   []byte(`{"stale_after_seconds":172800}`),
	})
	if got, want := patch.Text(), "$ backup-manager backup-set patch api-server/var-backups --stale-after 48h"; got != want {
		t.Errorf("a patch prints\n  %s\nwant\n  %s", got, want)
	}

	// A value with a space in it is quoted, because the point of the line
	// is that it can be pasted. A known_hosts line is the case that
	// actually occurs.
	create := Echo(Action{
		Method: "POST", Route: "/backup-sets",
		Body: []byte(`{"source_name":"api-server","name":"var-backups","host":"10.0.0.14","user":"backups","remote_path":"/var/backups","local_path":"/data/backups","ssh_key_id":"key_1","known_hosts_line":"10.0.0.14 ssh-ed25519 AAAAC3Nz","completion_strategy":"rename"}`),
	})
	if !strings.Contains(create.Text(), "--known-hosts-line '10.0.0.14 ssh-ed25519 AAAAC3Nz'") {
		t.Errorf("a value with a space in it is not quoted, so the command is not pasteable:\n%s", create.Text())
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
	if !strings.Contains(line.Text(), "POST /api/v1/operations") {
		t.Errorf("the gap does not name the route that needs a verb:\n%s", line.Text())
	}
	if !strings.Contains(line.Text(), "not in this engine") {
		t.Errorf("the gap does not say why `backup-manager run` is not the answer:\n%s", line.Text())
	}

	// The same route with a restore on it DOES have a verb, which is what
	// makes the refusal above about the request rather than about the
	// route.
	restore := Echo(Action{Method: "POST", Route: "/operations",
		Body: []byte(`{"action":"restore","config_revision":"r1","restore":{"artifact_id":"api-server/var-backups/dump.tar","medium":"offsite_s3","window_days":7,"acknowledged":true}}`)})
	if got, want := restore.Text(), "$ backup-manager restore api-server/var-backups/dump.tar --medium offsite_s3 --days 7 --acknowledge"; got != want {
		t.Errorf("a restore prints\n  %s\nwant\n  %s", got, want)
	}
}

// The environment the printed commands need, said once by the panel rather
// than repeated on every line, and never carrying the password.
func TestEnvironmentPreambleNeverCarriesThePassword(t *testing.T) {
	got := EnvironmentPreamble("http://nas.local:8080", "admin")
	for _, want := range []string{"BACKUP_MANAGER_API_URL=http://nas.local:8080", "BACKUP_MANAGER_API_USERNAME=admin", "BACKUP_MANAGER_API_PASSWORD=<your password>"} {
		if !strings.Contains(got, want) {
			t.Errorf("the preamble is %q and does not carry %q", got, want)
		}
	}
}
