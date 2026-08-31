package packaging

import (
	"os"
	"path/filepath"
	"strings"
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
			// <PostArgs> is Unraid's container command, so an empty one
			// is broken rather than safe. What must not appear is
			// anything that is not the canonical image's own binary.
			name: "an Unraid template whose command is not a canonical binary",
			mutate: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "template", "backup-manager.xml"),
					`<?xml version="1.0"?>`+"\n"+`<Container version="2">
  <Name>backup-manager</Name>
  <PostArgs>/usr/local/bin/seed.sh</PostArgs>
</Container>
`)
			},
			wantRule: RuleNonCanonicalCommand,
		},
		{
			name: "an Unraid template that chains a script onto the canonical command",
			mutate: func(t *testing.T, root string) {
				mustWrite(t, filepath.Join(root, "template", "backup-manager.xml"),
					`<?xml version="1.0"?>`+"\n"+`<Container version="2">
  <Name>backup-manager</Name>
  <PostArgs>/backup-manager-web serve &amp;&amp; /usr/local/bin/seed.sh</PostArgs>
</Container>
`)
			},
			wantRule: RuleInlineShell,
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

// TestScanLifecycleAcceptsACanonicalUnraidCommand is the other half of
// the two PostArgs controls above: the rule has to accept the one value
// that is correct, or it would just be a ban on Unraid templates.
func TestScanLifecycleAcceptsACanonicalUnraidCommand(t *testing.T) {
	root := cleanFixture(t)
	mustWrite(t, filepath.Join(root, "template", "backup-manager.xml"),
		`<?xml version="1.0"?>`+"\n"+`<Container version="2">
  <Name>backup-manager</Name>
  <PostArgs>/backup-manager-web serve</PostArgs>
</Container>
`)
	got, err := ScanLifecycle(root)
	if err != nil {
		t.Fatalf("ScanLifecycle: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a canonical <PostArgs> was reported as a violation:\n%s", format(got))
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
		// The fail-closed form. container/compose.yaml uses it for every
		// host path so an unset variable stops the deployment, and a
		// parser that does not recognise it leaves the whole literal in
		// place to be compared against a real path.
		{"${DISK:?set DISK}/backups", nil, "${DISK:?set DISK}/backups", 1},
		{"${DISK:?set DISK}/backups", map[string]string{"DISK": "/srv/d"}, "/srv/d/backups", 0},
		{"${DISK:?set DISK}/backups", map[string]string{"DISK": ""}, "${DISK:?set DISK}/backups", 1},
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

// TestVarRefsTellsTheThreeFormsApart is what the fail-closed storage rule
// stands on. All three forms expand to something; only one of them refuses
// to expand to a plausible wrong path when nobody set the variable.
func TestVarRefsTellsTheThreeFormsApart(t *testing.T) {
	refs := VarRefs("${A}:${B:-/default}:${C:?set C}")
	if len(refs) != 3 {
		t.Fatalf("got %d refs, want 3: %+v", len(refs), refs)
	}
	if refs[0].HasDefault || refs[0].FailClosed {
		t.Errorf("${A} read as %+v, want a bare reference", refs[0])
	}
	if !refs[1].HasDefault || refs[1].Default != "/default" || refs[1].FailClosed {
		t.Errorf("${B:-/default} read as %+v", refs[1])
	}
	if !refs[2].FailClosed || refs[2].HasDefault {
		t.Errorf("${C:?set C} read as %+v, want fail-closed", refs[2])
	}
}

// TestAStoragePathWithADefaultIsVisibleAsSuch is the positive control for
// TestEveryStoragePathFailsClosed. The rule is a negative claim about every
// mount in every profile, so it has to be shown failing against a mount
// that silently defaults and passing against the fail-closed form. It also
// pins the volume splitter: ${STATE_DIR:?set it} is one field with two
// colons in it, and splitting on every colon turns one volume into rubble.
func TestAStoragePathWithADefaultIsVisibleAsSuch(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "compose.yml")
	mustWrite(t, path, `services:
  engine:
    image: ghcr.io/spdrman/backup-manager:1.0.0
    volumes:
      - ${STATE_DIR:-/srv/fallback/state}:/data/state
      - ${BACKUP_DIR:?set BACKUP_DIR}:/data/backups
`)
	svcs, err := ReadCompose(path, map[string]string{"BACKUP_DIR": "/srv/backups"})
	if err != nil {
		t.Fatalf("ReadCompose: %v", err)
	}
	if len(svcs) != 1 || len(svcs[0].Mounts) != 2 {
		t.Fatalf("got %+v, want one service with two mounts", svcs)
	}

	state, backups := svcs[0].Mounts[0], svcs[0].Mounts[1]
	if state.ContainerPath != "/data/state" || backups.ContainerPath != "/data/backups" {
		t.Fatalf("volume split wrong: %q and %q", state.ContainerPath, backups.ContainerPath)
	}
	if backups.HostPath != "/srv/backups" {
		t.Errorf("fail-closed host path expanded to %q, want /srv/backups", backups.HostPath)
	}

	failClosed := func(m Mount) bool {
		for _, ref := range VarRefs(m.HostPathRaw) {
			if !ref.FailClosed {
				return false
			}
		}
		return true
	}
	if failClosed(state) {
		t.Error("a ${STATE_DIR:-/srv/fallback/state} mount was not reported as silently defaulting, so the rule cannot fail")
	}
	if !failClosed(backups) {
		t.Error("a ${BACKUP_DIR:?...} mount was reported as silently defaulting, so the rule fires on correct profiles")
	}
}

const cleanExtraParams = "--read-only --cap-drop=ALL --security-opt=no-new-privileges:true --tmpfs /tmp:size=64m --user 99:100"

func TestCheckExtraParamsHardeningAcceptsTheShippedTemplates(t *testing.T) {
	if v := CheckExtraParamsHardening("clean.xml", cleanExtraParams); len(v) > 0 {
		t.Errorf("a correct ExtraParams string was reported as unhardened:\n%s", format(v))
	}
}

// TestCheckExtraParamsHardeningCatchesFlagsThatUndoIt is the positive
// control for the Unraid half of the hardening rule. Every case here passed
// the previous substring version of the check, which asked only whether
// five strings appeared somewhere in the line.
func TestCheckExtraParamsHardeningCatchesFlagsThatUndoIt(t *testing.T) {
	tests := []struct {
		name        string
		extraParams string
		wantRule    string
	}{
		{"privileged", cleanExtraParams + " --privileged", RuleUnsafeRunFlag},
		{"capability added back", cleanExtraParams + " --cap-add=SYS_ADMIN", RuleUnsafeRunFlag},
		{"seccomp unconfined", cleanExtraParams + " --security-opt=seccomp=unconfined", RuleUnsafeRunFlag},
		{"host pid namespace", cleanExtraParams + " --pid=host", RuleUnsafeRunFlag},
		{"host network", cleanExtraParams + " --network=host", RuleUnsafeRunFlag},
		{"device passthrough", cleanExtraParams + " --device=/dev/sda", RuleUnsafeRunFlag},
		{"host userns", cleanExtraParams + " --userns=host", RuleUnsafeRunFlag},
		{"root user", "--read-only --cap-drop=ALL --security-opt=no-new-privileges:true --tmpfs /tmp:size=64m --user 0:0", RuleUnsafeRunFlag},
		// --userns=host contains the substring "--user", which is
		// exactly how the presence-only check was satisfied by a flag
		// that pins no user at all.
		{"userns is not a user", "--read-only --cap-drop=ALL --security-opt=no-new-privileges:true --tmpfs /tmp:size=64m --userns=host", RuleMissingHardening},
		{"no read-only rootfs", "--cap-drop=ALL --security-opt=no-new-privileges:true --tmpfs /tmp:size=64m --user 99:100", RuleMissingHardening},
		{"capabilities not dropped", "--read-only --security-opt=no-new-privileges:true --tmpfs /tmp:size=64m --user 99:100", RuleMissingHardening},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := CheckExtraParamsHardening("template.xml", tc.extraParams)
			if !hasRule(v, tc.wantRule) {
				t.Errorf("no %s reported for %q; got:\n%s", tc.wantRule, tc.extraParams, format(v))
			}
		})
	}
}

// TestCheckForwardedHeaderTrustFailsInBothDirections is the positive
// control for the rule that did not exist at all: grepping this package for
// TRUST_FORWARDED_HEADERS used to return nothing, while docs/deployment.md
// makes it the one variable with an explicit never-set-it-here rule.
func TestCheckForwardedHeaderTrustFailsInBothDirections(t *testing.T) {
	engineTrusting := Service{Name: "backup-manager", Source: "compose.yml", Environment: map[string]string{"TRUST_FORWARDED_HEADERS": "true"}}
	engineSilent := Service{Name: "backup-manager", Source: "template.xml", Environment: map[string]string{}}
	uiTrusting := Service{Name: "backup-manager-ui", Source: "compose.yml", Environment: map[string]string{"TRUST_FORWARDED_HEADERS": "true"}}
	uiSilent := Service{Name: "backup-manager-ui", Source: "compose.yml", Environment: map[string]string{}}

	tests := []struct {
		name     string
		svc      Service
		edge     bool
		mayTrust bool
		wantFail bool
	}{
		{"engine trusts where the topology allows it", engineTrusting, false, true, false},
		{"engine stops trusting where the profile says it does", engineSilent, false, true, true},
		{"engine trusts on a shared host-wide network", engineTrusting, false, false, true},
		{"engine stays silent where it must", engineSilent, false, false, false},
		{"the edge trusts a forwarded header", uiTrusting, true, true, true},
		{"the edge stays silent", uiSilent, true, true, false},
		{"the edge trusts even where the engine may", uiTrusting, true, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := CheckForwardedHeaderTrust(tc.svc, tc.edge, tc.mayTrust)
			if got := len(v) > 0; got != tc.wantFail {
				t.Errorf("reported %d violations, want failure=%v:\n%s", len(v), tc.wantFail, format(v))
			}
		})
	}
}

// cleanProcedure is an acceptance procedure that does everything the two
// rules ask for. Each control below removes exactly one thing from it, so a
// control that fires proves the rule caught what it removed.
const cleanProcedure = `
# Example procedure

	mkdir -p /mnt/user/backups/backup-manager
	chown -R 99:100 /mnt/user/appdata/backup-manager
	chown 99:100 /mnt/user/backups/backup-manager

	head -c 8M /dev/urandom > /mnt/user/backups/backup-manager/canary.bin
	sha256sum /mnt/user/backups/backup-manager/canary.bin | tee /root/evidence/canary.sha256
	find /mnt/user/backups/backup-manager -type f | sort > /root/evidence/before

	sha256sum -c /root/evidence/canary.sha256
	diff /root/evidence/before /root/evidence/after

- [ ] the backup root is untouched, byte for byte
`

func TestCheckAcceptanceProcedureAcceptsASafeVerifiableProcedure(t *testing.T) {
	if v := CheckAcceptanceProcedure(cleanProcedure, "/mnt/user/backups/backup-manager", nil); len(v) > 0 {
		t.Errorf("a safe, baseline-recording procedure was reported as unsafe:\n%s", format(v))
	}
}

func TestCheckAcceptanceProcedureCatchesDestructiveAndUnverifiableSteps(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(string) string
		subs     map[string]string
		wantRule string
	}{
		{
			name:     "recursive chown across the share holding the backup root",
			mutate:   func(s string) string { return s + "\n\tchown -R 99:100 /mnt/user/backups\n" },
			wantRule: RuleRecursiveChown,
		},
		{
			name:     "recursive chown on the backup root itself",
			mutate:   func(s string) string { return s + "\n\tchown -R 99:100 /mnt/user/backups/backup-manager\n" },
			wantRule: RuleRecursiveChown,
		},
		{
			name:     "recursive chown hidden behind a placeholder",
			mutate:   func(s string) string { return s + "\n\tchown -R 1000:100 \"$SHARE\"\n" },
			subs:     map[string]string{"$SHARE": "/mnt/user/backups"},
			wantRule: RuleRecursiveChown,
		},
		{
			name:     "no canary written",
			mutate:   func(s string) string { return strings.ReplaceAll(s, "/dev/urandom", "/dev/zero") },
			wantRule: RuleUnverifiableClaim,
		},
		{
			name:     "hash recorded but never verified",
			mutate:   func(s string) string { return strings.ReplaceAll(s, "sha256sum -c", "ls -la") },
			wantRule: RuleUnverifiableClaim,
		},
		{
			name:     "listing recorded but never compared",
			mutate:   func(s string) string { return strings.ReplaceAll(s, "diff ", "cat ") },
			wantRule: RuleUnverifiableClaim,
		},
		{
			name: "baseline recorded inside the tree it vouches for",
			mutate: func(s string) string {
				return strings.ReplaceAll(s, "/root/evidence/canary.sha256", "/mnt/user/backups/backup-manager/canary.sha256")
			},
			wantRule: RuleBaselineInsideBackupRoot,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := CheckAcceptanceProcedure(tc.mutate(cleanProcedure), "/mnt/user/backups/backup-manager", tc.subs)
			if !hasRule(v, tc.wantRule) {
				t.Errorf("no %s reported; got:\n%s", tc.wantRule, format(v))
			}
		})
	}
}

// TestRenderedCatalogCarriesTheDefaultsItWasRenderedWith is the positive
// control for the TrueNAS catalog coverage. The catalog entry is the
// app-store deliverable, and until it was rendered here nothing compared
// its image, host paths, ports or hardening to anything. Pointing
// ix_values.yaml somewhere else has to change what the rules see, otherwise
// the new coverage is decorative.
func TestRenderedCatalogCarriesTheDefaultsItWasRenderedWith(t *testing.T) {
	c := MustLoad()
	catalog := filepath.Join(PlatformDir("truenas"), "catalog")
	template := filepath.Join(catalog, "templates", "docker-compose.yaml")

	real, err := RenderTrueNASCatalogTemplate(template, filepath.Join(catalog, "ix_values.yaml"))
	if err != nil {
		t.Fatalf("render with the shipped defaults: %v", err)
	}
	svcs, err := ParseCompose([]byte(real), "rendered", nil)
	if err != nil {
		t.Fatalf("parse rendered catalog: %v", err)
	}
	if len(svcs) == 0 {
		t.Fatal("rendered catalog has no services")
	}
	for _, svc := range svcs {
		if svc.Image != c.Image.Reference {
			t.Fatalf("baseline render already disagrees with canonical: %q", svc.Image)
		}
	}

	// Now the mutation: the same template, rendered against defaults that
	// point somewhere else.
	mutated := filepath.Join(t.TempDir(), "ix_values.yaml")
	mustWrite(t, mutated, `image:
  reference: "docker.io/somebody/else:latest"
storage:
  state:
    hostPath: "/mnt/tank/backup-manager/state"
  backups:
    hostPath: "/mnt/tank/backup-manager/secrets"
  config:
    hostPath: "/mnt/tank/backup-manager/config/config.yaml"
  sshKey:
    hostPath: "/mnt/tank/backup-manager/secrets/id_ed25519"
  knownHosts:
    hostPath: "/mnt/tank/backup-manager/secrets/known_hosts"
network:
  webPort: 9999
runtime:
  puid: 568
  pgid: 568
`)
	bad, err := RenderTrueNASCatalogTemplate(template, mutated)
	if err != nil {
		t.Fatalf("render with mutated defaults: %v", err)
	}
	badSvcs, err := ParseCompose([]byte(bad), "rendered", nil)
	if err != nil {
		t.Fatalf("parse mutated render: %v", err)
	}
	sawImage, sawPath := false, false
	for _, svc := range badSvcs {
		if svc.Image != c.Image.Reference {
			sawImage = true
		}
		for _, m := range svc.Mounts {
			if want, _ := c.Platforms["truenas"].HostPaths.ByRole(m.Role); m.Role != "" && m.HostPath != want {
				sawPath = true
			}
		}
	}
	if !sawImage {
		t.Error("a catalog rendered against a foreign image reference still looked canonical, so the image rule cannot fail on the catalog")
	}
	if !sawPath {
		t.Error("a catalog rendered with the backup root pointed at the secrets directory still looked canonical, so the storage rule cannot fail on the catalog")
	}

	// A question with no default must be an error, not a template
	// expression smuggled through into the rendered YAML.
	empty := filepath.Join(t.TempDir(), "ix_values.yaml")
	mustWrite(t, empty, "image:\n  reference: \"x\"\n")
	if _, err := RenderTrueNASCatalogTemplate(template, empty); err == nil {
		t.Error("rendering with missing defaults returned no error; the renderer would fail open")
	}
}
