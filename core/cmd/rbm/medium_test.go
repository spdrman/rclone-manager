package main

import (
	"flag"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Whether `medium preflight` can be found, and whether it refuses before it
// opens anything.
//
// Discoverability is checked because a verb that is dispatchable and unlisted
// has shipped invisibly here before. What is unusual is that the usage text
// itself is asserted for specific words: this command writes a probe object
// into an operator's bucket, and a usage line reading "checks your medium"
// would leave somebody expecting a reachability ping.
//
// The refusal cells run with no resolvable config at all. That is the
// strongest available form of "nothing was opened": if a refusal ever leaked
// past the argument checks, the command would fail for want of a config
// rather than for the reason under test, and the cell would notice.

// TestMediumPreflightIsDiscoverableFromTheCommandLine. A verb absent from
// the command table cannot be run, and a verb absent from the usage block
// cannot be found; the second is how `backup-set remove` shipped invisibly
// (#391). A preflight is worth less than nothing if the operator who needs
// it does not know it is there.
func TestMediumPreflightIsDiscoverableFromTheCommandLine(t *testing.T) {
	if _, ok := commands["medium"]; !ok {
		t.Fatal("there is no medium command, so nothing an operator types reaches the preflight this release built")
	}
	out := captureStderr(t, usage)
	if !strings.Contains(out, "medium preflight <medium-id>") {
		t.Error("usage() does not list the medium preflight verb, so an operator cannot discover it")
	}
	// The usage line has to say what it actually does, because "checks
	// your medium" would leave somebody assuming a reachability ping and
	// not expecting a probe object in their bucket.
	for _, says := range []string{"probe object", "reads it back", "deletes the probe"} {
		if !strings.Contains(out, says) {
			t.Errorf("usage() does not mention %q, so an operator cannot tell this writes to their bucket", says)
		}
	}
}

// TestMediumPreflightIsRefusedBeforeAnythingIsOpened. Every case here is
// run with no resolvable config path at all, so a refusal that leaked past
// the argument checks would fail trying to open a state database rather
// than returning 2. Exit code 2 is this CLI's usage-error code and 1 is
// its failure code, so a 2 proves the request was rejected on its own
// terms, before a service, a journal or an endpoint was involved.
func TestMediumPreflightIsRefusedBeforeAnythingIsOpened(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		says string
	}{
		{
			// The arity refusal names the verbs that exist rather than
			// the one that existed when this was written, so the
			// expectation is built from the same table the message is.
			// That keeps this cell about arity: it asks whether the
			// refusal happened and said what shape it wanted, and does
			// not go red the day a second verb is added.
			//
			// It reads mediumOperandShapes rather than mediumVerbNames
			// because G2.2 (#594) gave this command verbs that take no
			// operand at all, so one trailing "<medium-id>" would have
			// asked four of the seven for an id they are right not to
			// want.
			name: "no verb and no medium",
			args: nil,
			says: "expected " + mediumOperandShapes(),
		},
		{
			name: "a verb but no medium",
			args: []string{"preflight"},
			says: "expected " + mediumOperandShapes(),
		},
		{
			// The other direction, which only became possible when verbs
			// stopped sharing an arity: an operand given to a verb that
			// takes none. Telling somebody "expected list | show
			// <medium-id> | ..." here would send them looking for an id
			// they were right not to have.
			name: "an operand a verb does not take",
			args: []string{"list", "offsite_s3"},
			says: "list takes no argument",
		},
		{
			name: "a medium but no verb this command has",
			args: []string{"prefligt", "offsite_s3"},
			says: `unknown subcommand "prefligt"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var code int
			out := captureStderr(t, func() {
				code = cmdMedium(append(tc.args, "--config", "/nonexistent/no-such-config.yaml"))
			})
			if code != 2 {
				t.Fatalf("exit code = %d, want 2 (a usage refusal, before anything is opened); stderr=%s", code, out)
			}
			if !strings.Contains(out, tc.says) {
				t.Errorf("stderr = %q, want it to contain %q", out, tc.says)
			}
		})
	}
}

// -------------------------------------------- the G2.2 verbs (#594) ------

// TestMediumSurfaceHasNoFlagThatTakesASecret is the security property
// this whole command surface is shaped around, asserted structurally
// rather than trusted.
//
// EPIC G requires every action taken in the browser to print its `rbm`
// equivalent into the global terminal, and that terminal is
// copy-to-clipboard and exportable, so anything printed there ends up
// pasted into a chat window eventually. Redaction is not the answer:
// redaction is a policy somebody has to remember to apply at every print
// site, and a site that forgets is indistinguishable from one that had
// nothing to redact until the day it leaks.
//
// So there is no flag here that takes a secret at all, and this walks the
// real flag set to prove it. The material only ever arrives on stdin,
// which is not in the process table and not in shell history.
func TestMediumSurfaceHasNoFlagThatTakesASecret(t *testing.T) {
	fs := flag.NewFlagSet("medium", flag.ContinueOnError)
	declareMediumFlags(fs)

	var names []string
	fs.VisitAll(func(f *flag.Flag) { names = append(names, f.Name) })
	if len(names) == 0 {
		t.Fatal("declareMediumFlags declared nothing, so this test would pass by finding no flags at all")
	}
	// The positive control: the scan really does see the flags that are
	// there, so an empty result below means "no such flag exists" rather
	// than "the walk found nothing".
	if !slices.Contains(names, "credentials-id") {
		t.Fatalf("the flag walk does not see --credentials-id, so it proves nothing about what it did not find: %v", names)
	}

	forbidden := regexp.MustCompile(`(?i)secret|access-key|accesskey|password|passphrase|token`)
	for _, name := range names {
		if forbidden.MatchString(name) {
			t.Errorf("--%s takes something secret-shaped on a command line, which is in `ps` output for every user on this host and in shell history. Read it from stdin instead, the way import-credentials does", name)
		}
	}
}

// TestMediumImportCredentials_RefusesWithoutStdin. Reading a terminal's
// stdin by default would hang with no explanation for somebody exploring,
// and the flag is also what makes the shape of this command obvious in an
// echoed command line: there is nowhere else the material could come from.
func TestMediumImportCredentials_RefusesWithoutStdin(t *testing.T) {
	var code int
	out := captureStderr(t, func() {
		code = cmdMedium([]string{"import-credentials", "--config", "/nonexistent/no-such-config.yaml"})
	})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (a usage refusal, before anything is opened); stderr=%s", code, out)
	}
	if !strings.Contains(out, "--stdin is required") {
		t.Errorf("stderr = %q, want it to say --stdin is required", out)
	}
	if !strings.Contains(out, "shell history") {
		t.Errorf("the refusal does not say why the secret is not a flag: %q", out)
	}
}

// TestParseSharedCredentialsText_ReportsShapeAndNeverTheBytes is the
// parse this command does to what it read off stdin. It runs in a terminal
// whose transcript an operator exports, so a refusal that quoted its input
// would be the one place this binary prints a secret.
func TestParseSharedCredentialsText_ReportsShapeAndNeverTheBytes(t *testing.T) {
	const canary = "CANARY-594-cli-1f7be4a09c53-DO-NOT-PRINT"

	id, secret, token, err := parseSharedCredentialsText([]byte(
		"[default]\naws_access_key_id = EXAMPLE-NOT-A-REAL-KEY\naws_secret_access_key = " + canary + "\n"))
	if err != nil {
		t.Fatalf("parseSharedCredentialsText: %v", err)
	}
	if id != "EXAMPLE-NOT-A-REAL-KEY" || secret != canary || token != "" {
		t.Fatalf("parsed as id=%q secret-len=%d token=%q", id, len(secret), token)
	}

	// The refusal path, with the canary in play so the assertion is about
	// a code path rather than about an input that carried nothing.
	_, _, _, err = parseSharedCredentialsText([]byte("[default]\naws_secret_access_key = " + canary + "\n"))
	if err == nil {
		t.Fatal("credentials text with no access key id was accepted")
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("the refusal echoes what it read: %v", err)
	}
}

// TestMediumFlagsSpec_LeavesUnwrittenFlagsAlone is the property an edit
// depends on. A flag that was not passed and a flag that was passed empty
// are different requests, and only the first one means "leave it alone".
// It matters most for the credential: an unnamed one keeps the credential
// already configured, and an empty one would describe a destination that
// reaches nowhere.
func TestMediumFlagsSpec_LeavesUnwrittenFlagsAlone(t *testing.T) {
	fs := flag.NewFlagSet("medium", flag.ContinueOnError)
	flags := declareMediumFlags(fs)
	if err := fs.Parse([]string{"--region", "eu-west-1"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	spec := flags.spec("offsite_s3")
	if spec.Region != "eu-west-1" {
		t.Errorf("Region = %q, want eu-west-1", spec.Region)
	}
	// --type has a non-empty DEFAULT ("s3"), which is exactly the case a
	// value-reading implementation would get wrong: it would send "s3" on
	// an edit that never mentioned the type, and a whole-record replace
	// would then write it.
	if spec.Type != "" {
		t.Errorf("Type = %q for a flag that was never passed; an edit would write it over whatever the destination says", spec.Type)
	}
	if spec.Bucket != "" || spec.Credentials.ID != "" || spec.Credentials.File != "" {
		t.Errorf("unwritten flags reached the spec: %+v", spec)
	}
}

// TestMediumReadsAreNotRefusedBesideARunningEngine is #538's rule applied
// to this command's two read verbs, and it is here because getting it
// wrong is invisible on a developer machine and total on a real one.
//
// openConfigWriteRoute refuses when something is serving this deployment
// and no route was named. That is right for a write, because a change left
// in the file is one the serving process would never read. It is wrong for
// a read: the ordinary install has an engine running and no
// BACKUP_MANAGER_API_URL set, so a routed `medium list` would refuse to
// show an operator their own destinations on every deployment that has
// one.
//
// This asks the question structurally rather than by standing up an
// engine: the read verbs must not reach openConfigWriteRoute at all. The
// positive control is the write verbs, which must.
func TestMediumReadsAreNotRefusedBesideARunningEngine(t *testing.T) {
	source, err := os.ReadFile("medium.go")
	if err != nil {
		t.Fatalf("reading medium.go: %v", err)
	}
	text := string(source)

	// The positive control first. Without it, a file that had stopped
	// mentioning either helper would pass every assertion below by
	// finding nothing at all.
	if !strings.Contains(text, "withMediumRoute(ctx, cfgPath") {
		t.Fatal("no verb reaches withMediumRoute any more, so this test proves nothing about which ones do")
	}

	for _, verb := range []string{"mediumList", "mediumShow"} {
		body := verbBody(t, text, verb)
		if strings.Contains(body, "withMediumRoute") {
			t.Errorf("%s goes through withMediumRoute, so it is refused beside a running engine with no route configured, which is every ordinary install. A read answers from this host's configuration file (withMediumRead), the way `settings` already does", verb)
		}
		if !strings.Contains(body, "withMediumRead") {
			t.Errorf("%s does not go through withMediumRead, so nothing here can say which door it opens", verb)
		}
	}
	// mediumPreflightVerb joined this list with #636, and it is the one
	// entry here that was on the other side of it. A check that PASSES
	// clears that destination's unverified mark, which is a configuration
	// write, so it stopped being a read: a mark cleared in the file beside
	// a running engine is a change that process never re-reads and would
	// put back from its own stale copy on the next write. That costs the
	// verb something the four beside it never had, so it is pinned here
	// rather than left to a comment.
	for _, verb := range []string{"mediumWrite", "mediumRemove", "mediumImportCredentials", "mediumPreflightVerb"} {
		if body := verbBody(t, text, verb); !strings.Contains(body, "withMediumRoute") {
			t.Errorf("%s does not go through withMediumRoute: a configuration write left in the file beside a running engine is a change that process would never read (#538, #543, #636)", verb)
		}
	}
}

// verbBody is the source of one function in medium.go, from its signature
// to the next top-level `func`. Crude on purpose: what it is used for is
// "does this function mention that helper", and a real parse would be more
// machinery than the question deserves.
func verbBody(t *testing.T, text, name string) string {
	t.Helper()
	start := strings.Index(text, "\nfunc "+name+"(")
	if start < 0 {
		t.Fatalf("medium.go has no func %s, so this test read nothing for it", name)
	}
	rest := text[start+1:]
	if end := strings.Index(rest[1:], "\nfunc "); end >= 0 {
		return rest[:end+1]
	}
	return rest
}
