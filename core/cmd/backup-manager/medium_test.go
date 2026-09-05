package main

import (
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
			name: "no verb and no medium",
			args: nil,
			says: "expected preflight <medium-id>",
		},
		{
			name: "a verb but no medium",
			args: []string{"preflight"},
			says: "expected preflight <medium-id>",
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
