// The executable half of runtime-contract.json (issue #167, EPIC B #81
// Phase 6). Four claims: every field the contract names is declared by
// the canonical runtime definition and by every artifact derived from
// it, no definition anywhere needs a prohibited host privilege, nothing
// in the tree declares compose services without being registered, and
// the two facts the contract is most likely to be wrong about (which
// service holds which role, and where private state sits relative to
// backup data) are read out of the document rather than out of a name.
//
// Nearly every rule here is run twice, once against the real tree and
// once against the real tree deliberately broken. That is not symmetry
// for its own sake. All of these are negative claims, and a negative
// claim evaluated over a document that failed to parse, or over a
// contract with an empty field list, passes: a CheckField that returned
// nil would report a complete contract against a blank file, and a
// prohibition list nobody has watched refuse anything is a list of
// comments. Three preflight rules in this repository have already
// failed open. That is also why several tests below abort when their
// own mutation loop turns out to have run zero mutations, rather than
// reporting the pass that a zero-iteration loop produces.
//
// This is an external test package, so the checks reach the contract
// only through what it exports and cannot quietly depend on an
// unexported shortcut a refactor would take away.
// hostpath_internal_test.go is the one deliberate exception, and says
// why in its own words.
package compose_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/distribution/compose"
)

// env is what an operator's .env supplies. The canonical definition uses
// the ${VAR:?message} form for every host path precisely so an unset
// variable stops the deployment instead of landing somewhere plausible, so
// a contract check has to supply them the way `docker compose` would.
//
// Every variable any registered artifact uses in a host path belongs
// here, and the omission was not cosmetic: KEY_FILE and DISK were
// missing, apps/proxmox and apps/openmediavault name them, and the
// mount parser used to answer those entries with a corrupted host path
// instead of saying it could not resolve them. Now it refuses, and a
// missing key here fails the prohibition check by name rather than
// quietly checking nothing.
func env() map[string]string {
	return map[string]string{
		"STATE_DIR":        "/srv/backup-manager/state",
		"BACKUP_DIR":       "/srv/backup-manager/backups",
		"CONFIG_DIR":       "/srv/backup-manager/config",
		"SSH_KEY_FILE":     "/srv/backup-manager/secrets/id_ed25519",
		"KEY_FILE":         "/srv/backup-manager/secrets/id_ed25519",
		"KNOWN_HOSTS_FILE": "/srv/backup-manager/secrets/known_hosts",
		"DISK":             "/srv/dev-disk-by-uuid-11111111-2222-3333-4444-555555555555",
		"APPDATA":          "/volume1/docker/backup-manager",
		"BACKUP_ROOT":      "/volume1/backup-manager",
	}
}

// canonical reads the definition the contract names as authoritative,
// through the same path resolution and the same variable expansion an
// operator's `docker compose` would use. Hand-building a Document here
// would make every test below a test of the fixture: the interesting
// failures are ones where the checked-in YAML says something nobody
// meant, and a fixture cannot say that.
func canonical(t *testing.T) compose.Document {
	t.Helper()
	c := compose.MustLoadContract()
	doc, err := compose.Read(compose.Path(c.Canonical), env())
	if err != nil {
		t.Fatalf("read the canonical runtime definition: %v", err)
	}
	return doc
}

// ---------------------------------------------------------------------
// Every required field is declared
// ---------------------------------------------------------------------

// TestCanonicalDefinitionDeclaresEveryRequiredField runs one subtest per
// contract field rather than one over all of them, so a failure names
// the field instead of the count, and prints that field's own Why. The
// contract records a reason per field precisely so a red build tells the
// person reading it what the field was for, which is the difference
// between fixing the definition and deleting the field.
func TestCanonicalDefinitionDeclaresEveryRequiredField(t *testing.T) {
	t.Parallel()

	c := compose.MustLoadContract()
	doc := canonical(t)

	for _, field := range c.Fields {
		t.Run(field.ID, func(t *testing.T) {
			t.Parallel()
			if findings := doc.CheckField(field); len(findings) != 0 {
				for _, f := range findings {
					t.Errorf("%s: %s\n  why the contract requires it: %s", f.Rule, f.Detail, field.Why)
				}
			}
		})
	}
}

// TestRequiredFieldCheckFailsWhenAFieldIsRemoved is the positive control
// for the suite above, one mutation per field. Without it a CheckField
// that returned nil unconditionally would report a complete contract
// against an empty file.
func TestRequiredFieldCheckFailsWhenAFieldIsRemoved(t *testing.T) {
	t.Parallel()

	c := compose.MustLoadContract()
	data, err := os.ReadFile(compose.Path(c.Canonical))
	if err != nil {
		t.Fatalf("read canonical: %v", err)
	}

	for _, field := range c.Fields {
		t.Run(field.ID, func(t *testing.T) {
			t.Parallel()

			doc, err := compose.Parse(data, c.Canonical, env())
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			mutated, what := doc.WithoutField(field)
			if what == "" {
				t.Fatalf("nothing to remove for %q, so this control proves nothing", field.ID)
			}
			findings := mutated.CheckField(field)
			if len(findings) == 0 {
				t.Fatalf("removing %s did not fail the %q check, so that check cannot detect a missing field", what, field.ID)
			}
			joined := findingText(findings)
			if !strings.Contains(joined, field.ID) {
				t.Errorf("the finding %q does not name the rule %q that produced it", joined, field.ID)
			}
		})
	}
}

// TestEveryMountFieldDeclaresExactlyOneWriteMode is the completeness rule
// this contract was missing, and it is the rule the PR that added the
// write-mode booleans applied to canonical.json and not to itself.
//
// Both directions fail. Setting both booleans makes checkServiceMount's
// verdict depend on which branch runs first. Setting NEITHER is worse and
// is what actually shipped: /data/state and /data/backups declared no
// write mode at all, so the check verified they were mounted and said
// nothing about `:ro`. A read-only backup destination in the canonical
// Compose file, the one file every adapter derives from, passed every
// gate in this repository. That is #196's shape with a worse blast
// radius: #196 broke three write paths, this breaks the backup itself.
func TestEveryMountFieldDeclaresExactlyOneWriteMode(t *testing.T) {
	t.Parallel()

	if findings := compose.CheckFieldPolicies(compose.MustLoadContract()); len(findings) != 0 {
		t.Errorf("the runtime contract does not hold to its own field policy:\n%s", findingText(findings))
	}
}

// TestTheFieldPolicyCheckFiresOnAFieldThatDeclaresNothing is its positive
// control. A completeness rule that has never been watched refuse anything
// is a comment, and this one is asserting the absence of a declaration,
// which is the easiest kind of rule to write so that it can only pass.
func TestTheFieldPolicyCheckFiresOnAFieldThatDeclaresNothing(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		field compose.Field
		want  string
	}{
		{
			name:  "a mount declaring neither write mode, which is what /data/backups did",
			field: compose.Field{ID: "control-mount", Scope: "service-mount", ContainerPath: "/data/backups", Derived: compose.DerivedChecked},
			want:  "declares neither readOnly nor writable",
		},
		{
			name:  "a mount declaring both",
			field: compose.Field{ID: "control-mount", Scope: "service-mount", ContainerPath: "/data/backups", ReadOnly: true, Writable: true, Derived: compose.DerivedChecked},
			want:  "declares both readOnly and writable",
		},
		{
			name:  "a field with no derived policy at all, which is where five of them sat",
			field: compose.Field{ID: "control-field", Scope: "service", Key: "restart"},
			want:  "declares derived policy",
		},
		{
			name:  "a field excused from the derived check without saying why",
			field: compose.Field{ID: "control-field", Scope: "service", Key: "restart", Derived: compose.DerivedCanonicalOnly},
			want:  "states no derivedWhy",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			findings := compose.CheckFieldPolicies(compose.Contract{Fields: []compose.Field{tc.field}})
			if len(findings) == 0 {
				t.Fatalf("the field policy check accepted %+v, so it cannot detect the thing it exists for", tc.field)
			}
			if joined := findingText(findings); !strings.Contains(joined, tc.want) {
				t.Errorf("the refusal is %q, which never says %q", joined, tc.want)
			}
		})
	}

	// The negative half: a fully declared field is accepted, so the four
	// refusals above are about the omission and not about a checker that
	// refuses everything.
	ok := compose.Field{ID: "control-mount", Scope: "service-mount", ContainerPath: "/data/backups", Writable: true, Derived: compose.DerivedChecked}
	if findings := compose.CheckFieldPolicies(compose.Contract{Fields: []compose.Field{ok}}); len(findings) != 0 {
		t.Errorf("a field that declares one write mode and a derived policy was refused: %s", findingText(findings))
	}
}

// TestEveryDerivedArtifactDeclaresEveryCheckedField is the other half of
// the same finding: five contract fields were checked on no adapter at
// all, because CheckField only ever ran against the canonical definition
// and derive.go's seven derived fields did not include them. Four of the
// five converted platforms shipped without TZ as a result, which is the
// field the contract's own reasoning says decides retention's calendar
// boundaries.
func TestEveryDerivedArtifactDeclaresEveryCheckedField(t *testing.T) {
	t.Parallel()

	c := compose.MustLoadContract()
	checked := c.DerivedCheckedFields()
	if len(checked) == 0 {
		t.Fatal("no field is checked on derived artifacts, so this suite would run zero assertions")
	}
	if len(c.Derived) == 0 {
		t.Fatal("the contract lists no derived artifact, so this suite would run zero assertions")
	}

	for _, rel := range c.Derived {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			doc, err := compose.Read(compose.Path(rel), env())
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			for _, field := range checked {
				if findings := doc.CheckField(field); len(findings) != 0 {
					for _, f := range findings {
						t.Errorf("%s: %s\n  why the contract requires it: %s", f.Rule, f.Detail, field.Why)
					}
				}
			}
		})
	}
}

// TestTheDerivedFieldCheckFailsWhenADerivedArtifactDropsAField is its
// positive control, one mutation per checked field, on a real derived
// artifact rather than on the canonical file. Without it, a suite that
// happens to hold could be a suite that checks nothing: that is precisely
// the state the derived artifacts were in before this test existed.
func TestTheDerivedFieldCheckFailsWhenADerivedArtifactDropsAField(t *testing.T) {
	t.Parallel()

	c := compose.MustLoadContract()
	rel := c.Derived[0]
	data, err := os.ReadFile(compose.Path(rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}

	for _, field := range c.DerivedCheckedFields() {
		t.Run(field.ID, func(t *testing.T) {
			t.Parallel()
			doc, err := compose.Parse(data, rel, env())
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			mutated, what := doc.WithoutField(field)
			if what == "" {
				t.Fatalf("nothing to remove for %q, so this control proves nothing", field.ID)
			}
			if findings := mutated.CheckField(field); len(findings) == 0 {
				t.Fatalf("removing %s from %s did not fail the %q check, so that field is not checked on derived artifacts", what, rel, field.ID)
			}
		})
	}
}

// TestWriteModeCheckFailsWhenAMountCarriesTheWrongOne is the positive
// control for the write-mode half of checkServiceMount, which the
// removal control above cannot reach: removing a mount proves the
// presence branch works and says nothing about whether :ro is inspected
// at all.
//
// It is what stops issue #196's defect returning. The configuration mount
// was `:ro` for the whole of Phase 4, and no check anywhere read that
// flag against a declared intent, so three merged write paths were inert
// in every packaged container while the suite stayed green.
func TestWriteModeCheckFailsWhenAMountCarriesTheWrongOne(t *testing.T) {
	t.Parallel()

	c := compose.MustLoadContract()
	data, err := os.ReadFile(compose.Path(c.Canonical))
	if err != nil {
		t.Fatalf("read canonical: %v", err)
	}

	controlled := 0
	for _, field := range c.Fields {
		if field.Scope != "service-mount" || (!field.ReadOnly && !field.Writable) {
			continue
		}
		controlled++
		t.Run(field.ID, func(t *testing.T) {
			t.Parallel()

			doc, err := compose.Parse(data, c.Canonical, env())
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if findings := doc.CheckField(field); len(findings) != 0 {
				t.Fatalf("the unmutated definition already fails %q (%s), so this control would pass for the wrong reason", field.ID, findingText(findings))
			}
			mutated, what, ok := doc.WithWrongWriteMode(field)
			if !ok {
				t.Fatalf("nothing to flip for %q, so this control proves nothing", field.ID)
			}
			findings := mutated.CheckField(field)
			if len(findings) == 0 {
				t.Fatalf("making %s did not fail the %q check, so the write mode of that mount is not checked at all", what, field.ID)
			}
			joined := findingText(findings)
			if !strings.Contains(joined, field.ID) {
				t.Errorf("the finding %q does not name the rule %q that produced it", joined, field.ID)
			}
		})
	}

	// A loop over an empty list is a passing test that checked nothing,
	// and the contract is a JSON file anyone can edit.
	if controlled == 0 {
		t.Fatal("no mount field declares a write mode, so this control ran zero mutations")
	}
}

// findingText keeps the rule ID attached to the detail, because the
// mutation controls do not only assert that a check fired: they assert
// it named itself. A finding that fires under the wrong rule ID sends
// the next reader to the wrong part of the contract.
func findingText(findings []compose.Finding) string {
	parts := make([]string, 0, len(findings))
	for _, f := range findings {
		parts = append(parts, f.Rule+": "+f.Detail)
	}
	return strings.Join(parts, " | ")
}

// ---------------------------------------------------------------------
// No prohibited host privilege, anywhere
// ---------------------------------------------------------------------

// TestNoDefinitionNeedsAProhibitedHostPrivilege runs the prohibition
// list over the canonical definition AND everything derived from it.
// The derived artifacts are the ones that matter: the canonical file is
// reviewed by everyone, while a platform adapter is reviewed by whoever
// added that platform, and `privileged: true` reads as a plausible fix
// for a permissions problem on a NAS.
func TestNoDefinitionNeedsAProhibitedHostPrivilege(t *testing.T) {
	t.Parallel()

	c := compose.MustLoadContract()
	paths := append([]string{c.Canonical}, c.Derived...)

	for _, rel := range paths {
		t.Run(rel, func(t *testing.T) {
			t.Parallel()
			doc, err := compose.Read(compose.Path(rel), env())
			if err != nil {
				t.Fatalf("read %s: %v", rel, err)
			}
			for _, f := range doc.CheckProhibited(c) {
				t.Errorf("%s\n  %s: %s\n  why it is prohibited: %s", rel, f.Rule, f.Detail, f.Why)
			}
		})
	}
}

// TestProhibitionCheckFiresOnEveryProhibitedSetting is the positive
// control: the canonical definition, mutated to require each prohibited
// setting in turn, must be rejected and must say which service and which
// setting. Three preflight rules in this repository have already failed
// open; a prohibition list nobody has watched fail is a list of comments.
func TestProhibitionCheckFiresOnEveryProhibitedSetting(t *testing.T) {
	t.Parallel()

	c := compose.MustLoadContract()
	data, err := os.ReadFile(compose.Path(c.Canonical))
	if err != nil {
		t.Fatalf("read canonical: %v", err)
	}

	for _, rule := range c.Prohibited {
		t.Run(rule.ID, func(t *testing.T) {
			t.Parallel()

			doc, err := compose.Parse(data, c.Canonical, env())
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			mutated, what := doc.WithProhibited(rule)
			if what == "" {
				t.Fatalf("no mutation is defined for %q, so this control proves nothing", rule.ID)
			}

			findings := mutated.CheckProhibited(c)
			if len(findings) == 0 {
				t.Fatalf("injecting %s did not trip the %q rule, so that rule fails open", what, rule.ID)
			}

			var matched *compose.Finding
			for i := range findings {
				if findings[i].Rule == rule.ID {
					matched = &findings[i]
					break
				}
			}
			if matched == nil {
				t.Fatalf("injecting %s tripped %s, but not %q itself: a rule that only fires through another rule is not the rule it claims to be", what, findingText(findings), rule.ID)
			}
			if matched.Service == "" {
				t.Errorf("the %q finding does not name the service it was found on: %q", rule.ID, matched.Detail)
			}
			if !strings.Contains(matched.Detail, what) {
				t.Errorf("the %q finding says %q, which never mentions the injected value %q", rule.ID, matched.Detail, what)
			}
		})
	}
}

// TestProhibitionScanSeesKeysTheParserHasNoFieldFor guards the specific
// way a checker like this fails open: parsing compose into a struct means
// a key with no matching field is invisible, so `privileged: true` on a
// service the struct never modelled would read as clean.
func TestProhibitionScanSeesKeysTheParserHasNoFieldFor(t *testing.T) {
	t.Parallel()

	const doc = `
services:
  something-nobody-modelled:
    image: backup-manager:dev
    command: ["/backup-manager-web", "serve"]
    privileged: true
    x-invented-key:
      nested:
        network_mode: host
`
	c := compose.MustLoadContract()
	parsed, err := compose.Parse([]byte(doc), "synthetic.yaml", env())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	findings := parsed.CheckProhibited(c)
	if len(findings) < 2 {
		t.Fatalf("the scan found %d finding(s) in a document that declares privileged mode AND host networking under an unmodelled key: %s", len(findings), findingText(findings))
	}
}

// TestTheStartGateRejectsTheBackupFreshnessCommand is the control that
// names the regression rather than a generic mutation: the exact
// healthcheck this file used to declare, put back, has to be rejected.
// web-ui waits on this check, so with `backup-manager status` in it a
// DEGRADED backup set or an unconfigured instance keeps the only
// LAN-facing listener from starting.
func TestTheStartGateRejectsTheBackupFreshnessCommand(t *testing.T) {
	t.Parallel()

	c := compose.MustLoadContract()
	var field compose.Field
	for _, f := range c.Fields {
		if f.ID == "start-gate-liveness" {
			field = f
			break
		}
	}
	if field.ID == "" {
		t.Fatal("the contract declares no start-gate-liveness field, so nothing checks what web-ui waits on")
	}

	doc := canonical(t)
	if findings := doc.CheckField(field); len(findings) != 0 {
		t.Fatalf("the canonical definition already fails its own start-gate rule: %s", findingText(findings))
	}

	mutated := doc.WithServiceHealthcheckTest(compose.RoleEngine, []any{"CMD", "/backup-manager", "status"})
	findings := mutated.CheckField(field)
	if len(findings) == 0 {
		t.Fatal("declaring `backup-manager status` as the engine's healthcheck passed the start-gate rule, so the rule cannot see the thing it exists to prevent")
	}
	if !strings.Contains(findingText(findings), "start-gate-liveness") {
		t.Errorf("the finding %q does not name the rule that produced it", findingText(findings))
	}
}

// TestProhibitionCheckFiresOnTheMountShapesTheShortFormHides is the
// control WithProhibited cannot be: it injects a literal host path in
// short syntax, so it only ever exercises the shape that already worked.
// These two are the shapes that used to be answered wrongly and in
// silence, and one of them was already reaching a real derived artifact.
func TestProhibitionCheckFiresOnTheMountShapesTheShortFormHides(t *testing.T) {
	t.Parallel()

	c := compose.MustLoadContract()

	cases := []struct {
		name  string
		entry any
	}{
		{
			name:  "compose long volume syntax, which the old parser rendered as one nonsense string",
			entry: map[string]any{"type": "bind", "source": "/var/run/docker.sock", "target": "/var/run/docker.sock"},
		},
		{
			name:  "a host path behind an unresolved variable, whose message carries the colon the parser split on",
			entry: "${SOCK:?set SOCK in .env to the docker socket}:/var/run/docker.sock",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			doc := canonical(t)
			if base := doc.CheckProhibited(c); len(base) != 0 {
				t.Fatalf("the unmutated canonical definition already reports %s, so a finding below would prove nothing", findingText(base))
			}

			mutated, what := doc.WithVolume(tc.entry)
			if what == "" {
				t.Fatal("nothing was injected, so this control proves nothing")
			}

			var matched *compose.Finding
			findings := mutated.CheckProhibited(c)
			for i := range findings {
				if findings[i].Rule == "docker-socket" {
					matched = &findings[i]
					break
				}
			}
			if matched == nil {
				t.Fatalf("injecting %s did not trip the %q rule (findings: %s), so the rule fails open on this shape", what, "docker-socket", findingText(findings))
			}
			if matched.Service == "" {
				t.Errorf("the finding does not name the service it was found on: %q", matched.Detail)
			}
		})
	}
}

// TestMountsRefusesWhatItCannotResolveInsteadOfAnsweringWrongly pins the
// parse itself, one layer below the rule. The reported corruption was
// exact: HostPath "${KEY_FILE", ContainerPath "?set KEY_FILE in .env to
// the SFTP private key", ReadOnly false. Nothing said so.
func TestMountsRefusesWhatItCannotResolveInsteadOfAnsweringWrongly(t *testing.T) {
	t.Parallel()

	const doc = `
services:
  engine:
    image: backup-manager:dev
    command: ["/backup-manager-web", "serve"]
    volumes:
      - ${KEY_FILE:?set KEY_FILE in .env to the SFTP private key}:/etc/backup-manager/id_ed25519:ro
      - /srv/backup-manager/state:/data/state
`
	parsed, err := compose.Parse([]byte(doc), "synthetic.yaml", map[string]string{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	svc := parsed.Service("engine")
	if svc == nil {
		t.Fatal("the synthetic document declares no engine service")
	}

	mounts := parsed.Mounts(svc)
	if len(mounts) != 1 {
		t.Fatalf("Mounts = %+v, want only the one entry that actually resolves: an unresolved host path must not be answered as a Mount", mounts)
	}
	if mounts[0].HostPath != "/srv/backup-manager/state" {
		t.Errorf("Mounts[0].HostPath = %q, want the resolved entry", mounts[0].HostPath)
	}

	refused := parsed.UnparseableMounts(svc)
	if len(refused) != 1 || !strings.Contains(refused[0], "KEY_FILE") {
		t.Fatalf("UnparseableMounts = %v, want the one entry naming KEY_FILE: a refusal nobody can read is the silence this replaces", refused)
	}
}

// ---------------------------------------------------------------------
// Completeness: nothing derives from the canonical definition unchecked
// ---------------------------------------------------------------------

// TestEveryComposeArtifactInTheTreeIsRegistered closes the gap every
// registry-driven check has: the rules above only run over what the
// contract lists, so an unregistered compose file is not a failing
// artifact, it is an invisible one. The walk finds compose artifacts by
// their content rather than their filename, because the thing being
// looked for is a file that declares services, and nothing makes an
// adapter author call it compose.yaml.
func TestEveryComposeArtifactInTheTreeIsRegistered(t *testing.T) {
	t.Parallel()

	c := compose.MustLoadContract()
	registered := map[string]bool{c.Canonical: true}
	for _, d := range c.Derived {
		registered[d] = true
	}

	var found []string
	roots := []string{"apps", "container", "distribution"}
	for _, root := range roots {
		err := filepath.WalkDir(compose.Path(root), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			ext := filepath.Ext(path)
			if ext != ".yaml" && ext != ".yml" {
				return nil
			}
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if !strings.Contains(string(body), "\nservices:") && !strings.HasPrefix(string(body), "services:") {
				return nil
			}
			rel, _ := filepath.Rel(compose.Path("."), path)
			found = append(found, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	if len(found) == 0 {
		t.Fatal("the walk found no compose artifact at all, so this completeness check is checking nothing")
	}
	for _, rel := range found {
		if !registered[rel] {
			t.Errorf("%s declares compose services but is not registered in runtime-contract.json, so no prohibition rule ever runs against it", rel)
		}
	}
}

// ---------------------------------------------------------------------
// Roles come from the command, never from the service name
// ---------------------------------------------------------------------

// TestServiceRolesAreDerivedFromTheCommand pins the one design decision
// the rest of this package rests on. Every per-role rule (which mounts,
// which ports, which health check) resolves a role first, and if that
// resolution keyed on the service name then renaming a service in an
// adapter would silently exempt it from all of them rather than fail.
// So the rename is the control: the same two services under different
// names have to come back with the same two roles.
func TestServiceRolesAreDerivedFromTheCommand(t *testing.T) {
	t.Parallel()

	doc := canonical(t)
	roles := map[compose.Role][]string{}
	for name, role := range doc.Roles() {
		roles[role] = append(roles[role], name)
	}

	for _, want := range []compose.Role{compose.RoleEngine, compose.RoleWebUI} {
		if len(roles[want]) != 1 {
			t.Errorf("the canonical definition has %d service(s) in the %q role, want exactly 1: %v", len(roles[want]), want, roles[want])
		}
	}

	// Positive control: renaming both services must not change the roles,
	// because a check keyed on the name would silently stop checking.
	renamed, err := compose.Parse([]byte(`
services:
  totally-different-name:
    image: backup-manager:dev
    command: ["/backup-manager-web", "serve", "--profile=generic"]
  another-name-entirely:
    image: backup-manager:dev
    command: ["/backup-manager-web", "serve-ui", "--profile=generic"]
`), "synthetic.yaml", env())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := renamed.Roles()
	if got["totally-different-name"] != compose.RoleEngine {
		t.Errorf("a renamed engine resolved to role %q, want %q", got["totally-different-name"], compose.RoleEngine)
	}
	if got["another-name-entirely"] != compose.RoleWebUI {
		t.Errorf("a renamed web UI resolved to role %q, want %q", got["another-name-entirely"], compose.RoleWebUI)
	}
}

// ---------------------------------------------------------------------
// Private state and backup data never contain one another
// ---------------------------------------------------------------------

// TestPrivateStateAndBackupDataAreSeparateMounts is issue #87 (B5.1)'s
// rule at its source. The backup destination is the one host directory
// an operator is expected to share with other people, so anything nested
// inside it is published: here that would be the lifecycle journal and
// the local-auth administrator record. Containment is checked both ways
// round because either nesting is a layout somebody could write, and the
// helper doing the checking gets its own control, since two assertions
// built on a Contains that always answered false would both pass.
func TestPrivateStateAndBackupDataAreSeparateMounts(t *testing.T) {
	t.Parallel()

	doc := canonical(t)
	state, ok := doc.MountFor(compose.RoleEngine, "/data/state")
	if !ok {
		t.Fatal("the engine declares no /data/state mount")
	}
	backups, ok := doc.MountFor(compose.RoleEngine, "/data/backups")
	if !ok {
		t.Fatal("the engine declares no /data/backups mount")
	}

	if state.HostPath == backups.HostPath {
		t.Fatalf("private state and backup data share the host path %q", state.HostPath)
	}
	if compose.Contains(backups.HostPath, state.HostPath) {
		t.Errorf("private application state %q lives inside the backup destination %q, so a backup share handed to a user carries the state database and the local-auth record", state.HostPath, backups.HostPath)
	}
	if compose.Contains(state.HostPath, backups.HostPath) {
		t.Errorf("the backup destination %q lives inside the private state directory %q", backups.HostPath, state.HostPath)
	}

	// Positive control: the containment helper has to actually detect
	// containment, or the two assertions above pass for any input.
	if !compose.Contains("/srv/backups", "/srv/backups/state") {
		t.Error("Contains fails to see a directory inside its own parent, so the separation assertions above are vacuous")
	}
	if compose.Contains("/srv/backups", "/srv/backups-elsewhere") {
		t.Error("Contains treats a sibling with a shared prefix as contained, which would make it fire on correct layouts")
	}
}
