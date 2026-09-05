package packaging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every value in this file is made up, and that is a rule rather than an
// accident. Issue #264's third criterion is that no host, port or key
// material from the production machines reaches this repository, and a
// test file is part of this repository: a guard written as "does the tree
// contain <the real port>" would put the real port in the tree, in a file
// with a name that invites reading. So the controls plant SHAPES.
//
// None of these resolves to anything anybody owns as far as this test is
// concerned, nothing is ever sent to them, and none of them is either of
// the two hosts this issue is about.
const (
	// A dotted name with a public-looking last label, which is what makes
	// it routable as far as the scan is concerned. Anything under
	// .internal, .local or an example.* domain is excluded by the rules
	// themselves, so a control built out of one would pass for the wrong
	// reason and prove nothing.
	madeUpRoutableHost = "a-host-this-test-invented.net"

	// A made-up address, chosen for its shape: outside every reserved
	// range the scan knows about, so the routable branch is the one that
	// runs.
	madeUpRoutableAddr = "11.22.33.44"

	// Not 22, because the whole shape this guard is about is an endpoint
	// on a port somebody chose not to publish.
	madeUpNonDefaultPort = "2222"

	// A key line of the right shape, and nobody's.
	madeUpHostKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI" + "BOGUSFIXTUREKEY"
)

// madeUpPEM is a key that is nobody's: the header, a body of the right
// shape, the footer. It authorises nothing and never did, and it is
// assembled here rather than written out so that this file does not
// itself contain the one shape the guard is looking for.
var madeUpPEM = "-----BEGIN OPENSSH PRIVATE KEY-----\n" +
	strings.Repeat("Qk9HVVNGSVhUVVJFTk9UQVJFQUxLRVlBVEFMTEZPUlRIRUdVQVJEVEVTVA", 2) + "\n" +
	"-----END OPENSSH PRIVATE KEY-----\n"

// TestNoProductionEndpointOrKeyMaterialIsInTheTree is issue #264's third
// acceptance criterion, made checkable.
//
// The criterion says the host, the port and the key material of two
// production machines never reach this repository. That is a rule a
// person can hold for exactly as long as they are thinking about it, and
// it is broken by one hostname in a doc example, one port hardcoded into
// a fixture so a test would go green, one key file copied in "just to
// try it". This runs on every gate instead.
//
// It sweeps what git tracks rather than what is on disk, because the
// criterion is about what reaches the repository.
func TestNoProductionEndpointOrKeyMaterialIsInTheTree(t *testing.T) {
	files, err := TrackedFiles(RepoRoot)
	if err != nil {
		t.Fatalf("listing tracked files: %v", err)
	}
	// A zero has two explanations, and "the scan found nothing" and "the
	// scan looked at nothing" are not the same result. This is the
	// difference between them.
	if len(files) < 500 {
		t.Fatalf("git ls-files reported %d tracked files, which is too few for this repository to be the one that was scanned. A clean sweep over an empty list is not a clean sweep.", len(files))
	}

	violations, err := ScanForLeakedEndpoints(RepoRoot, files)
	if err != nil {
		t.Fatalf("scanning the tracked tree: %v", err)
	}
	for _, v := range violations {
		// The finding, never the value. Printing what it found would put
		// the thing this test exists to keep out of the repository into
		// the gate's own output, which is captured, pasted and kept.
		t.Errorf("%s: %s (%s)\n\nIssue #264: the host, port and key material of the production sources are inputs, supplied at deployment time and never committed. Take it out of the tree and supply it the way the SSH key path is supplied. If this is a placeholder rather than a real endpoint, give it a name that says so: anything under .internal, .local, .test, .invalid or an example.* domain, or an address in a private or RFC 5737 documentation range, is excluded by these rules on purpose.", v.Path, v.Rule, v.Detail)
	}
}

// TestEveryLeakShapeIsCaught is the half that makes the test above worth
// having.
//
// A guard whose only evidence is its own green light is a guard nobody
// has checked. Each case here plants one shape in a directory of its own
// and asserts that the specific rule for it fires - specific, because a
// control that passes because some OTHER rule tripped proves nothing
// about the rule it claims to exercise.
func TestEveryLeakShapeIsCaught(t *testing.T) {
	cases := []struct {
		name string
		file string
		body string
		rule string
	}{
		{
			name: "a key file copied into the tree",
			file: "secrets/id_ed25519",
			body: madeUpPEM,
			rule: RuleCommittedPrivateKey,
		},
		{
			name: "a key file hidden under testdata, where the test exemption would otherwise reach",
			file: "core/testdata/deploy_key",
			body: madeUpPEM,
			rule: RuleCommittedPrivateKey,
		},
		{
			name: "a key pasted into something that is not a test",
			file: "core/internal/transport/rclone/defaults.go",
			body: "package rclone\n\nconst fallbackKey = `" + madeUpPEM + "`\n",
			rule: RuleCommittedPrivateKey,
		},
		{
			name: "a pinned host key for a routable endpoint, as a file",
			file: "container/known_hosts",
			body: "[" + madeUpRoutableHost + "]:" + madeUpNonDefaultPort + " " + madeUpHostKey + "\n",
			rule: RulePinnedEndpoint,
		},
		{
			name: "the same pin pasted into a Go string, which anchoring to a line start would miss",
			file: "core/service/pins.go",
			body: "package service\n\nvar pin = \"[" + madeUpRoutableHost + "]:" + madeUpNonDefaultPort + " " + madeUpHostKey + "\"\n",
			rule: RulePinnedEndpoint,
		},
		{
			name: "a pinned host key for a routable address on the default port",
			file: "docs/ssh-setup.md",
			body: "Pin it with:\n\n    " + madeUpRoutableAddr + " " + madeUpHostKey + "\n",
			rule: RulePinnedEndpoint,
		},
		{
			name: "a port hardcoded into a test fixture beside its host",
			file: "core/internal/config/fixture_test.go",
			body: "package config\n\n// sftp source\nvar fixture = Remote{\n\tHost: \"" + madeUpRoutableHost + "\",\n\tAddr: \"" + madeUpRoutableHost + ":" + madeUpNonDefaultPort + "\",\n}\n",
			rule: RuleRoutableSSHEndpoint,
		},
		{
			name: "a hostname and port in a doc example",
			file: "docs/install.md",
			body: "Point it at the source:\n\n    sftp://deploy@" + madeUpRoutableHost + ":" + madeUpNonDefaultPort + "/srv/backups\n",
			rule: RuleRoutableSSHEndpoint,
		},
		{
			name: "an endpoint in a YAML source block, where the ssh marker is lines away",
			file: "docs/reference/config.yaml",
			body: "sources:\n  - name: production\n    type: sftp\n    remote:\n      addr: " + madeUpRoutableAddr + ":" + madeUpNonDefaultPort + "\n",
			rule: RuleRoutableSSHEndpoint,
		},
		{
			name: "a keyscan command left in a script",
			file: "scripts/deploy/pin.sh",
			body: "#!/bin/sh\nssh-keyscan -p " + madeUpNonDefaultPort + " " + madeUpRoutableHost + ":" + madeUpNonDefaultPort + " >> known_hosts\n",
			rule: RuleRoutableSSHEndpoint,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, tc.file)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := ScanForLeakedEndpoints(root, []string{tc.file})
			if err != nil {
				t.Fatalf("scanning the planted fixture: %v", err)
			}
			if !firedRule(got, tc.rule) {
				t.Fatalf("planting this shape did not trip %s, so that rule cannot fail and is not a guard.\nplanted in %s:\n%s\nreported: %v", tc.rule, tc.file, tc.body, got)
			}
		})
	}
}

// TestWhatThisGuardMustNotReport is the other half of being able to fail.
//
// A rule that reports everything is switched off within a week, and then
// it reports nothing. Every case here is a shape that really is in this
// repository, or really belongs in it, and every one of them used to be
// a plausible false positive: the placeholder endpoints the tests
// already use, the documentation addresses the error-message tests are
// pinned to, and the `file.go:1514` citations that fill the transport
// package - which is also the package that says "ssh" most often, so the
// context window is no help there.
func TestWhatThisGuardMustNotReport(t *testing.T) {
	cases := []struct {
		name string
		file string
		body string
	}{
		{
			"the placeholder pin this repository's own tests use",
			"core/cmd/backup-manager/create_test.go",
			"package main\n\nconst aKnownHostsLine = \"[source.example.internal]:2222 " + madeUpHostKey + "\"\n",
		},
		{
			"an RFC 5737 documentation address in an SSH error message",
			"core/internal/lifecycle/stall_test.go",
			"package lifecycle\n\nvar e = `source \"prod\": NewFs: couldn't connect SSH: dial tcp 192.0.2.1:22: timeout`\n",
		},
		{
			"a Go source citation beside the word ssh",
			"core/internal/transport/rclone/ssh.go",
			"package rclone\n\n// The sftp backend sets HostKeyCallback at sftp.go:1514 and never\n// derives HostKeyAlgorithms from it, which is the whole bug.\n",
		},
		{
			"an image reference, which is a name and a version rather than a host and a port",
			"scripts/install/install_docker_host.py",
			"# ssh key path only\nIMAGE = \"ghcr.io/spdrman/backup-manager:0.3.0\"\n",
		},
		{
			"a generated throwaway keypair a test needs in order to parse one",
			"core/service/backupsets_test.go",
			"package service\n\nconst fixtureKey = `" + madeUpPEM + "`\n",
		},
		{
			"the module path of the sftp backend this product is built on",
			"core/internal/transport/rclone/adapter.go",
			"package rclone\n\nimport _ \"github.com/rclone/rclone/backend/sftp\"\n",
		},
		{
			"the egress probe's documented target, which is a public resolver and not a source",
			"scripts/install/install_docker_host.py",
			"# Nothing is sent to it. Not an ssh endpoint.\nPROBE = (\"1.1.1.1\", 443)\nPROBE_ADDR = \"1.1.1.1:443\"\n",
		},
		{
			"a private-range peer address in an auth test",
			"apps/common/auth/local/ratelimit_test.go",
			"package local\n\n// ssh has nothing to do with this, but the words are near each other\nvar peer = \"172.18.0.3:9000\"\n",
		},
		{
			"a container alias, which is a single label and resolves nowhere off its own network",
			"core/tests/machines/source.go",
			"package machines\n\n// the sftp source machine, reached by alias\nconst addr = \"source:2222\"\n",
		},
		{
			"a key PATH, which is not key material and is exactly what this product asks operators to supply",
			"scripts/install/install_docker_host.py",
			"# The SFTP client private key, never read and never printed.\nDEFAULT_KEY = \"/volume1/backup-manager/secrets/id_ed25519\"\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, tc.file)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := ScanForLeakedEndpoints(root, []string{tc.file})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 0 {
				t.Fatalf("reported %v for a shape that is not a leak. A rule that fires on this gets switched off, and then it fires on nothing.\nplanted in %s:\n%s", got, tc.file, tc.body)
			}
		})
	}
}

func firedRule(vs []Violation, rule string) bool {
	for _, v := range vs {
		if v.Rule == rule {
			return true
		}
	}
	return false
}
