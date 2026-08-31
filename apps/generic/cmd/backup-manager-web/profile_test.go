package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCmdServe_RejectsAnUnknownProfile: profile selection is a startup-time
// decision, and an unrecognised one has to stop the process rather than
// quietly fall back to generic. A silent fallback is how a UGOS deployment
// ends up running the generic auth story.
func TestCmdServe_RejectsAnUnknownProfile(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "serve", args: []string{"serve", "--profile", "not-a-profile", "--config", "/does/not/exist/config.yaml"}},
		{name: "serve-ui", args: []string{"serve-ui", "--profile", "not-a-profile"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(tc.args); got != 2 {
				t.Errorf("run(%v) = %d, want 2", tc.args, got)
			}
		})
	}
}

// TestCmdServe_RejectsAGatewayProfileWithNoTrustedPeer is the fail-closed
// half. `--profile=ugos` says "trust an identity header set by the
// platform gateway"; without a declared trusted peer there is no gateway,
// only a header anyone on the LAN can set.
func TestCmdServe_RejectsAGatewayProfileWithNoTrustedPeer(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(config, []byte("poll_interval: 1h\nsources: []\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if got := run([]string{"serve", "--profile", "ugos", "--config", config}); got == 0 {
		t.Error("serve --profile=ugos with no --trusted-gateway started; a gateway profile with no trusted peer must refuse")
	}
}

// TestCmdServeUI_RejectsAnUnusableUIDir: the whole point of a disk-backed
// bundle is that the operator can see it was used. Falling back to the
// embedded bundle when --ui-dir is wrong would hide the mistake behind a
// working-looking UI serving the wrong bridge.
func TestCmdServeUI_RejectsAnUnusableUIDir(t *testing.T) {
	if got := run([]string{"serve-ui", "--ui-dir", filepath.Join(t.TempDir(), "missing")}); got != 1 && got != 2 {
		t.Errorf("serve-ui --ui-dir <missing> = %d, want a non-zero exit", got)
	}
}

// TestUsageDocumentsEveryModeTheBinaryCarries keeps the help text honest:
// a mode nobody can find is a mode nobody uses, and this binary is the one
// executable the canonical runtime contract selects a profile on.
func TestUsageDocumentsEveryModeTheBinaryCarries(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	stderr := os.Stderr
	os.Stderr = w
	usage()
	os.Stderr = stderr
	w.Close()

	buf := make([]byte, 1<<16)
	n, _ := r.Read(buf)
	text := string(buf[:n])

	for _, want := range []string{"--profile", "--ui-dir", "--ui-root", "--trusted-gateway", "serve", "serve-ui", "healthcheck"} {
		if !strings.Contains(text, want) {
			t.Errorf("usage never mentions %q", want)
		}
	}
}

// TestCmdServeUI_RejectsAGatewayProfileWithNoTrustedPeer is issue #87's
// half of the fail-closed rule. `serve` has refused this since #167, and
// refusing it there alone was the gap: the engine's only possible peer in
// the shipped two-container topology IS the UI host, so the engine's
// trusted range has to contain the UI host for the deployment to
// authenticate at all, and every identity header the UI host forwards
// unexamined then arrives from a peer the engine trusts.
//
// The boundary therefore has to be declared on the hop that faces the
// LAN. Left undeclared, serve.NewUI strips every identity header, which
// is safe but produces a console nobody can sign in to and no reason why,
// so this refuses at startup instead.
func TestCmdServeUI_RejectsAGatewayProfileWithNoTrustedPeer(t *testing.T) {
	if got := run([]string{"serve-ui", "--profile", "ugos"}); got != 2 {
		t.Errorf("serve-ui --profile=ugos with no --trusted-gateway = %d, want 2", got)
	}

	// Positive control: the same command with a range declared gets past
	// this check, so the refusal is about the missing boundary and not
	// about the ugos profile being unusable in serve-ui at all. It still
	// fails, on the NEXT check (an unusable --ui-dir), which is what
	// proves it got past this one.
	if got := run([]string{"serve-ui", "--profile", "ugos", "--trusted-gateway", "10.0.0.0/8", "--ui-dir", "/does/not/exist"}); got != 1 {
		t.Errorf("serve-ui with a declared gateway and an unusable --ui-dir = %d, want 1 (a 2 means it never got past the gateway check)", got)
	}
}

// TestCmdServeUI_RejectsATrustedGatewayOnAProfileWithoutOne. Naming a
// trusted range for a profile that has no gateway is somebody expecting a
// native identity path that does not exist, exactly as it is on `serve`.
func TestCmdServeUI_RejectsATrustedGatewayOnAProfileWithoutOne(t *testing.T) {
	if got := run([]string{"serve-ui", "--profile", "generic", "--trusted-gateway", "10.0.0.0/8"}); got != 2 {
		t.Errorf("serve-ui --profile=generic --trusted-gateway = %d, want 2", got)
	}
}

// TestCmdServeUI_RejectsAMalformedTrustedGateway. A range that does not
// parse is not a weaker boundary, it is none.
func TestCmdServeUI_RejectsAMalformedTrustedGateway(t *testing.T) {
	if got := run([]string{"serve-ui", "--profile", "ugos", "--trusted-gateway", "not-a-cidr"}); got != 2 {
		t.Errorf("serve-ui with an unparsable --trusted-gateway = %d, want 2", got)
	}
}
