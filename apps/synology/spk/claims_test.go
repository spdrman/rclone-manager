// The tests here read prose rather than code, which is unusual and
// deliberate: three of this package's claims live in a CI comment, in an
// acceptance procedure and in a README, and a claim about what has been
// proven is worth exactly as much as the thing it describes. A comment
// asserting a check that nothing runs is worse than an acknowledged gap,
// because a reader stops looking.
package spk

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func repoText(t *testing.T, rel string) string {
	t.Helper()
	body, err := os.ReadFile(repoFile(t, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(body)
}

// TestParityClaims_SayWhatTheyActuallyProve pins the correction to the
// two places that described the SPK suite as checking real release
// binaries against container/release-manifest.json.
//
// Nothing does. Every test in this package builds its package from
// binaries it synthesised and checks them against a manifest hashed from
// those same files, which proves the packer and the verifier agree and
// says nothing about any shipped artifact. Parity against a real one is
// blocked on #174, and #179's capability matrix already records it that
// way, so these two documents were the only places contradicting it.
func TestParityClaims_SayWhatTheyActuallyProve(t *testing.T) {
	for _, tc := range []struct {
		rel    string
		anchor string
		window int
	}{
		{"docs/acceptance/synology-dsm-package-lifecycle.md", "## What is already proven without hardware", 0},
		{".github/workflows/ci.yml", "SPK conformance suite", 1200},
	} {
		t.Run(tc.rel, func(t *testing.T) {
			body := repoText(t, tc.rel)
			start := strings.Index(body, tc.anchor)
			if start < 0 {
				t.Fatalf("%s no longer contains %q, so this test is checking nothing", tc.rel, tc.anchor)
			}
			section := body[start:]
			if tc.window == 0 {
				if end := strings.Index(section[len(tc.anchor):], "\n## "); end >= 0 {
					section = section[:len(tc.anchor)+end]
				}
			} else if len(section) > tc.window {
				section = section[:tc.window]
			}
			for _, want := range []string{"synthetic", "#174"} {
				if !strings.Contains(section, want) {
					t.Fatalf("%s claims binary-hash parity without mentioning %q:\n%s", tc.rel, want, section)
				}
			}
		})
	}
}

// TestLauncher_TargetsPlainHTTP covers the one line that decides whether
// a headline acceptance criterion is reproducible.
//
// INFO declares adminprotocol=http, serve-ui is started with no TLS
// anywhere in the package, and DSM 7's own hardened default is HTTPS on
// 5001. Mirroring window.location.protocol therefore sends the browser
// into a TLS handshake against a plain HTTP listener whenever the
// administrator reached DSM over HTTPS, and the desktop launcher appears
// broken while the package runs correctly.
func TestLauncher_TargetsPlainHTTP(t *testing.T) {
	body, err := assetFS.ReadFile("assets/ui/index.html")
	if err != nil {
		t.Fatalf("read the launcher page: %v", err)
	}
	page := string(body)

	target := fmt.Sprintf(`"http://" + window.location.hostname + ":%d/"`, UIPort)
	if !strings.Contains(page, target) {
		t.Fatalf("the launcher does not navigate to %s:\n%s", target, page)
	}
	// The control, and the actual regression: the scheme must not be
	// read off the DSM session.
	if strings.Contains(page, "window.location.protocol") {
		t.Fatal("the launcher mirrors the DSM session's scheme, which breaks every HTTPS session")
	}
}

// TestOwnershipAssumption_IsRecordedAndChecked covers the assumption the
// package makes and cannot verify: conf/privilege declares
// run-as: package, postinst creates three directories under var/ and
// nothing anywhere does a chown.
//
// Both outcomes are bad and neither is detectable from the repository -
// root-owned 0750 directories a package-user daemon cannot write, or a
// chmod that fails under set -e and aborts the install - and the failure
// surfaces as start-stop-status blaming an unconfigured sources list,
// which is the wrong cause. A chown here would convert a possible start
// failure into a certain install failure, so what is mandatory is that
// the assumption is written down and that the hardware run answers it.
func TestOwnershipAssumption_IsRecordedAndChecked(t *testing.T) {
	scripts, err := LifecycleScripts()
	if err != nil {
		t.Fatalf("LifecycleScripts: %v", err)
	}
	postinst := scripts["postinst"]
	for _, want := range []string{"run-as", "id -u"} {
		if !strings.Contains(postinst, want) {
			t.Fatalf("postinst does not record %q, so the install log cannot answer who owns var/:\n%s", want, postinst)
		}
	}
	if strings.Contains(postinst, "chown") && !strings.Contains(postinst, "no chown") {
		t.Fatal("postinst grew a chown; the acceptance run has to say which uid these directories need first")
	}

	procedure := repoText(t, "docs/acceptance/synology-dsm-package-lifecycle.md")
	for _, want := range []string{"run-as: package", "ls -ln"} {
		if !strings.Contains(procedure, want) {
			t.Fatalf("the acceptance procedure never records %q, so the ownership question stays unanswerable", want)
		}
	}
}
