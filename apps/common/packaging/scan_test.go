package packaging

import (
	"os"
	"path/filepath"
	"testing"
)

// cleanFixture writes a minimal, entirely legitimate Tier B provider
// package into a temp directory: a README, a compose file that reuses the
// canonical image with a canonical command, and a frontend bridge. Every
// positive control below starts from this and adds exactly one violation,
// so a control that fires proves the rule caught the thing it added, not
// something the baseline was already doing wrong.
func cleanFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	mustWrite(t, filepath.Join(root, "README.md"), "# Example\n\nRun `sh -c true` is fine in prose.\n")
	mustWrite(t, filepath.Join(root, "compose", "backup-manager.yml"), `services:
  engine:
    image: ghcr.io/spdrman/backup-manager:1.0.0
    command: ["/backup-manager-web", "serve"]
    read_only: true
    volumes:
      - /srv/state:/data/state
      - /srv/backups:/data/backups
`)
	mustWrite(t, filepath.Join(root, "frontend", "platform.ts"), "export const bridge = { id: \"example\" };\n")
	return root
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestScanLifecycleAcceptsACleanPackage is the negative control for the
// controls: if the baseline itself were dirty, every positive control
// below would pass for the wrong reason.
func TestScanLifecycleAcceptsACleanPackage(t *testing.T) {
	got, err := ScanLifecycle(cleanFixture(t))
	if err != nil {
		t.Fatalf("ScanLifecycle: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("clean fixture reported %d violation(s), want 0:\n%s", len(got), format(got))
	}
}

// TestScanLifecycleCatchesViolations is the positive control the WP4.3
// gate needs: proof that the "no provider-specific lifecycle
// implementation" check can actually fail. Each case adds one real
// violation to an otherwise clean package and names the rule that must
// fire.
func TestScanLifecycleCatchesViolations(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(t *testing.T, root string)
		wantRule string
	}{
		{
			name: "an install shell script",
			mutate: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "install.sh"), "#!/bin/sh\necho installing\n")
			},
			wantRule: RuleDisallowedFileType,
		},
		{
			name: "a Go lifecycle implementation",
			mutate: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "lifecycle.go"), "package truenas\n\nfunc Install() error { return nil }\n")
			},
			wantRule: RuleDisallowedFileType,
		},
		{
			name: "a Python helper with no recognised extension",
			mutate: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "hooks", "postinstall"), "#!/usr/bin/env python3\nprint('hi')\n")
			},
			wantRule: RuleShebang,
		},
		{
			name: "a TypeScript file smuggled outside frontend/",
			mutate: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "lifecycle.ts"), "export function install() {}\n")
			},
			wantRule: RuleDisallowedFileType,
		},
		{
			name: "a Go file smuggled inside frontend/",
			mutate: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "frontend", "adapter.go"), "package frontend\n")
			},
			wantRule: RuleDisallowedFileType,
		},
		{
			name: "an executable metadata file",
			mutate: func(t *testing.T, root string) {
				p := filepath.Join(root, "compose", "backup-manager.yml")
				if err := os.Chmod(p, 0o755); err != nil {
					t.Fatalf("chmod: %v", err)
				}
			},
			wantRule: RuleExecutableBit,
		},
		{
			name: "a compose service that builds its own image",
			mutate: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "compose", "backup-manager.yml"), `services:
  engine:
    build:
      context: ../..
    command: ["/backup-manager-web", "serve"]
`)
			},
			wantRule: RuleBuildsOwnImage,
		},
		{
			name: "a command wrapped in a shell",
			mutate: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "compose", "backup-manager.yml"), `services:
  engine:
    image: ghcr.io/spdrman/backup-manager:1.0.0
    command: ["/bin/sh", "-c", "/setup && /backup-manager-web serve"]
`)
			},
			wantRule: RuleNonCanonicalCommand,
		},
		{
			name: "an entrypoint override",
			mutate: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "compose", "backup-manager.yml"), `services:
  engine:
    image: ghcr.io/spdrman/backup-manager:1.0.0
    entrypoint: ["/init"]
    command: ["/backup-manager-web", "serve"]
`)
			},
			wantRule: RuleEntrypointOverride,
		},
		{
			name: "a catalog lifecycle hook",
			mutate: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "catalog", "app.yaml"), `name: backup-manager
post_install: /usr/local/bin/seed-state.sh
`)
			},
			wantRule: RuleLifecycleHook,
		},
		{
			name: "a privileged container",
			mutate: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "compose", "backup-manager.yml"), `services:
  engine:
    image: ghcr.io/spdrman/backup-manager:1.0.0
    command: ["/backup-manager-web", "serve"]
    privileged: true
`)
			},
			wantRule: RulePrivileged,
		},
		{
			name: "an Unraid template that appends its own arguments",
			mutate: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "template", "backup-manager.xml"),
					`<?xml version="1.0"?>`+"\n"+`<Container version="2">
  <Name>backup-manager</Name>
  <PostArgs>&amp;&amp; /usr/local/bin/seed.sh</PostArgs>
</Container>
`)
			},
			wantRule: RuleLifecycleHook,
		},
		{
			name: "a privileged Unraid template",
			mutate: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "template", "backup-manager.xml"),
					`<?xml version="1.0"?>`+"\n"+`<Container version="2">
  <Name>backup-manager</Name>
  <Privileged>true</Privileged>
</Container>
`)
			},
			wantRule: RulePrivileged,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := cleanFixture(t)
			tc.mutate(t, root)

			got, err := ScanLifecycle(root)
			if err != nil {
				t.Fatalf("ScanLifecycle: %v", err)
			}
			if !hasRule(got, tc.wantRule) {
				t.Fatalf("ScanLifecycle did not report %s; it reported:\n%s", tc.wantRule, format(got))
			}
		})
	}
}

func TestScanSecretsAcceptsACleanPackage(t *testing.T) {
	got, err := ScanSecrets(cleanFixture(t))
	if err != nil {
		t.Fatalf("ScanSecrets: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("clean fixture reported %d secret finding(s), want 0:\n%s", len(got), format(got))
	}
}

// TestScanSecretsCatchesBundledCredentials is the positive control for the
// Phase 4 gate's "no bundled secrets" check.
func TestScanSecretsCatchesBundledCredentials(t *testing.T) {
	tests := []struct {
		name   string
		file   string
		body   string
		expect bool
	}{
		{
			name:   "a PEM private key",
			file:   "secrets/id_ed25519",
			body:   "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk=\n-----END OPENSSH PRIVATE KEY-----\n",
			expect: true,
		},
		{
			name:   "an SSH public key baked into a template",
			file:   "compose/authorized.yml",
			body:   "authorized: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleKeyMaterialHere operator@nas\n",
			expect: true,
		},
		{
			name:   "a literal password in an env file",
			file:   "compose/backup-manager.env",
			body:   "PUID=1000\nADMIN_PASSWORD=s3cretValue99\n",
			expect: true,
		},
		{
			name:   "a literal API token in a catalog file",
			file:   "catalog/values.yaml",
			body:   "api_key: abcdef0123456789\n",
			expect: true,
		},
		{
			name:   "a placeholder is not a secret",
			file:   "compose/backup-manager.env",
			body:   "ADMIN_PASSWORD=CHANGEME_before_first_start\n",
			expect: false,
		},
		{
			name:   "an unexpanded variable is not a secret",
			file:   "compose/backup-manager.env",
			body:   "ADMIN_PASSWORD=${ADMIN_PASSWORD}\n",
			expect: false,
		},
		{
			name:   "prose about passwords is not a secret",
			file:   "README.md",
			body:   "Enrol with a password: choose a long one and keep it out of this repository.\n",
			expect: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := cleanFixture(t)
			mustWrite(t, filepath.Join(root, tc.file), tc.body)

			got, err := ScanSecrets(root)
			if err != nil {
				t.Fatalf("ScanSecrets: %v", err)
			}
			found := hasRule(got, RuleBundledSecret)
			if found != tc.expect {
				t.Fatalf("ScanSecrets found=%v, want %v; findings:\n%s", found, tc.expect, format(got))
			}
		})
	}
}

func TestContains(t *testing.T) {
	tests := []struct {
		parent, child string
		want          bool
	}{
		{"/mnt/user/backups", "/mnt/user/backups", true},
		{"/mnt/user/backups", "/mnt/user/backups/set-a/artifact.tar", true},
		{"/mnt/user/backups", "/mnt/user/appdata/backup-manager/state", false},
		// The prefix trap: "/mnt/user/backups-old" starts with
		// "/mnt/user/backups" as a string but is a sibling, not a child.
		{"/mnt/user/backups", "/mnt/user/backups-old/x", false},
		{"/mnt/user/backups/", "/mnt/user/backups/x", true},
		{"/srv/dev-disk-by-uuid/backups", "/srv/dev-disk-by-uuid/appdata/x", false},
	}
	for _, tc := range tests {
		if got := Contains(tc.parent, tc.child); got != tc.want {
			t.Errorf("Contains(%q, %q) = %v, want %v", tc.parent, tc.child, got, tc.want)
		}
	}
}

func TestExpandCompose(t *testing.T) {
	tests := []struct {
		in             string
		env            map[string]string
		want           string
		wantUnresolved int
	}{
		{"ghcr.io/x:1.0.0", nil, "ghcr.io/x:1.0.0", 0},
		{"${IMAGE:-ghcr.io/x:1.0.0}", nil, "ghcr.io/x:1.0.0", 0},
		{"${IMAGE:-ghcr.io/x:1.0.0}", map[string]string{"IMAGE": "local:dev"}, "local:dev", 0},
		{"${IMAGE}", nil, "${IMAGE}", 1},
		{"${DISK:-/srv/d}/backups", nil, "/srv/d/backups", 0},
	}
	for _, tc := range tests {
		got, unresolved := ExpandCompose(tc.in, tc.env)
		if got != tc.want || len(unresolved) != tc.wantUnresolved {
			t.Errorf("ExpandCompose(%q) = %q, %v; want %q with %d unresolved",
				tc.in, got, unresolved, tc.want, tc.wantUnresolved)
		}
	}
}

// TestBridgeStorageMountExtractorCanFail proves the TypeScript extractor
// is not silently matching nothing. A regex-based reader that stops
// matching degrades into a test that always passes, so it has to be able
// to report both a value and an absence.
func TestBridgeStorageMountExtractorCanFail(t *testing.T) {
	root := t.TempDir()

	good := filepath.Join(root, "good.ts")
	mustWrite(t, good, "export const b = { deployment: { storageMount: \"/mnt/tank/x\" } };\n")
	got, err := BridgeStorageMount(good)
	if err != nil {
		t.Fatalf("BridgeStorageMount: %v", err)
	}
	if got != "/mnt/tank/x" {
		t.Fatalf("got %q, want /mnt/tank/x", got)
	}

	bad := filepath.Join(root, "bad.ts")
	mustWrite(t, bad, "export const b = { deployment: { label: \"none\" } };\n")
	if _, err := BridgeStorageMount(bad); err == nil {
		t.Fatal("BridgeStorageMount returned no error for a file with no storageMount; the extractor would fail open")
	}
}

func hasRule(violations []Violation, rule string) bool {
	for _, v := range violations {
		if v.Rule == rule {
			return true
		}
	}
	return false
}

func format(violations []Violation) string {
	if len(violations) == 0 {
		return "  (none)"
	}
	out := ""
	for _, v := range violations {
		out += "  " + v.String() + "\n"
	}
	return out
}

// TestScanForBespokeAuthCatchesAnOwnAuthMechanism is the positive control
// for the gate's "auth mode" check. Without it, the assertion that no
// platform wires its own authentication is just an assertion that a
// regular expression matched nothing, which is also what a broken regular
// expression looks like.
func TestScanForBespokeAuthCatchesAnOwnAuthMechanism(t *testing.T) {
	tests := []struct {
		name   string
		file   string
		body   string
		expect bool
	}{
		{"an OIDC block in a compose file", "compose/backup-manager.yml",
			"services:\n  engine:\n    environment:\n      OIDC_ISSUER: \"https://idp.example\"\n", true},
		{"an LDAP bind in a catalog file", "catalog/app.yaml", "auth: ldap\n", true},
		{"an htpasswd file", "compose/users.yml", "htpasswd: /etc/nginx/.htpasswd\n", true},
		{"an --auth-mode override", "compose/backup-manager.yml",
			"services:\n  engine:\n    command: [\"/backup-manager-web\", \"serve\", \"--auth-mode=ugos\"]\n", true},
		{"a README explaining there is no SSO", "README.md",
			"There is no SSO and no LDAP here: this platform uses the generic host's local auth.\n", false},
		{"the clean baseline", "compose/extra.yml", "services: {}\n", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := cleanFixture(t)
			mustWrite(t, filepath.Join(root, tc.file), tc.body)

			got, err := ScanForBespokeAuth(root)
			if err != nil {
				t.Fatalf("ScanForBespokeAuth: %v", err)
			}
			if found := hasRule(got, RuleBespokeAuth); found != tc.expect {
				t.Fatalf("found=%v, want %v; findings:\n%s", found, tc.expect, format(got))
			}
		})
	}
}

// TestScanForOMVPluginCatchesPluginMaterial is the positive control for
// §4A's deferral of a native OpenMediaVault Workbench plugin. A deferral
// that nothing enforces is a deferral that quietly stops holding.
func TestScanForOMVPluginCatchesPluginMaterial(t *testing.T) {
	tests := []struct {
		name   string
		file   string
		body   string
		expect bool
	}{
		{"a Debian packaging directory", "debian/control.txt", "Package: openmediavault-backupmanager\n", true},
		{"a salt state tree", "salt/deploy.yml", "backup-manager: {}\n", true},
		{"a Workbench navigation file", "workbench/navigation.yaml", "route: /services/backup\n", true},
		{"an RPC service", "rpc/backupmanager.json", "{\"service\": \"BackupManager\"}\n", true},
		{"an omv-mkconf hook referenced from metadata", "compose/backup-manager.yml",
			"services:\n  engine:\n    labels:\n      hook: omv-mkconf backupmanager\n", true},
		{"a README saying the plugin is deferred", "README.md",
			"There is no native OMV plugin here: no Workbench page, no RPC service, no debian package.\n", false},
		{"the clean baseline", "compose/extra.yml", "services: {}\n", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := cleanFixture(t)
			mustWrite(t, filepath.Join(root, tc.file), tc.body)

			got, err := ScanForOMVPlugin(root)
			if err != nil {
				t.Fatalf("ScanForOMVPlugin: %v", err)
			}
			if found := hasRule(got, RuleOMVPlugin); found != tc.expect {
				t.Fatalf("found=%v, want %v; findings:\n%s", found, tc.expect, format(got))
			}
		})
	}
}

// TestTrueNASVariableExtractorsCanFail keeps the catalog-coherence check
// honest: both extractors have to return something for a real file and
// nothing for a file that genuinely has nothing, otherwise the coherence
// test passes by comparing two empty sets.
func TestTrueNASVariableExtractorsCanFail(t *testing.T) {
	root := t.TempDir()

	questions := filepath.Join(root, "questions.yaml")
	mustWrite(t, questions, `questions:
  - variable: storage
    schema:
      type: dict
      attrs:
        - variable: state
          schema:
            type: dict
            attrs:
              - variable: hostPath
                schema:
                  type: hostpath
        - variable: backups
          schema:
            type: dict
            attrs:
              - variable: hostPath
                schema:
                  type: hostpath
`)
	got, err := TrueNASQuestionVariables(questions)
	if err != nil {
		t.Fatalf("TrueNASQuestionVariables: %v", err)
	}
	want := []string{"storage.state.hostPath", "storage.backups.hostPath"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, w := range want {
		if !hasString(got, w) {
			t.Errorf("missing %q in %v", w, got)
		}
	}

	empty := filepath.Join(root, "empty.yaml")
	mustWrite(t, empty, "questions: []\n")
	if got, err := TrueNASQuestionVariables(empty); err != nil || len(got) != 0 {
		t.Fatalf("empty questions.yaml gave %v, %v; want no variables and no error", got, err)
	}

	refs := TrueNASTemplateVariables(`image: {{ .Values.image.reference }}
volumes:
  - {{ .Values.storage.state.hostPath }}:/data/state
`)
	if len(refs) != 2 || !hasString(refs, "image.reference") || !hasString(refs, "storage.state.hostPath") {
		t.Fatalf("TrueNASTemplateVariables gave %v", refs)
	}
	if refs := TrueNASTemplateVariables("no values here at all\n"); len(refs) != 0 {
		t.Fatalf("TrueNASTemplateVariables on a template with no values gave %v", refs)
	}
}

func hasString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
