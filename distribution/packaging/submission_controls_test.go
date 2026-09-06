package packaging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The positive controls for issue #90.
//
// Every rule this work package adds is a negative claim: no self-update,
// no floating tag, no privileged mode, no mandatory telemetry, no drift
// from the canonical contract, no missing submission material. A negative
// claim is satisfied by a check that can never fire, so each one is run
// here against input that SHOULD trip it, and the test fails if it does
// not. #81 says the same thing about the drift gate in one sentence: a
// drift gate that has never failed is a drift gate nobody has tested.
//
// Two of these go further than a fixture. The hard rules are mutated
// against the REAL packaged files of every target, because a rule that
// fires on a fixture somebody wrote to trip it and misses the shape the
// actual tree takes is the failure mode this repository has already been
// bitten by: `\bpassword` never matches between "_" and "p", so it missed
// ADMIN_PASSWORD in every env file in the repository while passing its
// own tests. And the eight drift elements are each demonstrated against a
// deliberately introduced mismatch in a real parsed service rather than
// against a hand-built struct, so the parser is in the loop too.
//
// Nothing here touches shared state. Every fixture is a t.TempDir, every
// mutation is in memory, and no control needs a port, a container or a
// daemon.

// ---------------------------------------------------------------------
// The four hard rules
// ---------------------------------------------------------------------

func TestEachHardRuleFiresOnTheShapeItIsAbout(t *testing.T) {
	cases := []struct {
		name string
		rule func(path, text string) []Violation
		path string
		// trips must each produce at least one violation.
		trips []string
		// clean must each produce none. These are the forms that look
		// like the violation and are not it, which is where a rule
		// written as a keyword search goes wrong: every profile in this
		// repository contains the word "privileged".
		clean []string
	}{
		{
			name: "no-self-update",
			rule: CheckNoSelfUpdate,
			path: "fixture/compose.yaml",
			trips: []string{
				"labels:\n  com.centurylinklabs.watchtower.enable: \"true\"\n",
				"labels:\n  io.containers.autoupdate: registry\n",
				"pull_policy: always\n",
				"    AUTO_UPDATE: \"true\"\n",
				"  SELF_UPDATE=yes\n",
				"<AutoUpdate>true</AutoUpdate>",
			},
			clean: []string{
				"# This package has no self-update path: a new version is a new package.\n",
				"labels:\n  com.centurylinklabs.watchtower.enable: \"false\"\n",
				"    AUTO_UPDATE: \"false\"\n",
				"pull_policy: missing\n",
				"<AutoUpdate>false</AutoUpdate>",
			},
		},
		{
			name: "no-self-update in a lifecycle script",
			rule: CheckNoSelfUpdate,
			path: "spk/assets/scripts/postupgrade",
			trips: []string{
				"curl -fsSL https://example.invalid/agent | sh\n",
				"wget -O /tmp/x https://example.invalid/x\n",
				"apt-get install -y something\n",
			},
			clean: []string{
				"# never curl anything here\n",
				"say \"upgrade complete\"\n",
			},
		},
		{
			name: "no-floating-tag",
			rule: CheckNoFloatingTag,
			path: "fixture/compose.yaml",
			trips: []string{
				"    image: ghcr.io/spdrman/backup-manager:latest\n",
				"    image: ghcr.io/spdrman/backup-manager\n",
				"    image: ghcr.io/spdrman/backup-manager:${TAG:-latest}\n",
				"<Repository>ghcr.io/spdrman/backup-manager</Repository>",
				"  reference: ghcr.io/spdrman/backup-manager:LATEST\n",
			},
			clean: []string{
				"    image: ghcr.io/spdrman/backup-manager:1.0.0\n",
				"    image: backup-manager:${VERSION:-dev}\n",
				"    image: ghcr.io/spdrman/backup-manager@sha256:" + strings.Repeat("a", 64) + "\n",
				"    image: registry.invalid:5000/spdrman/backup-manager:1.0.0\n",
				"<Repository>ghcr.io/spdrman/backup-manager:1.0.0</Repository>",
				"# never deploy the latest tag\n",
				"image:\n  reference: ghcr.io/spdrman/backup-manager:1.0.0\n",
			},
		},
		{
			name: "no-privileged-mode",
			rule: CheckNoPrivilegedMode,
			path: "fixture/compose.yaml",
			trips: []string{
				"    privileged: true\n",
				"    privileged: \"true\"\n",
				"<Privileged>true</Privileged>",
				"<ExtraParams>--read-only --privileged</ExtraParams>",
				"    network_mode: host\n",
				"    pid: host\n",
				"    cap_add:\n      - SYS_ADMIN\n",
				"    security_opt:\n      - seccomp=unconfined\n",
				"    security_opt:\n      - no-new-privileges:false\n",
			},
			clean: []string{
				"    privileged: false\n",
				"<Privileged>false</Privileged>",
				"    cap_drop:\n      - ALL\n",
				"    security_opt:\n      - no-new-privileges:true\n",
				"# This container never runs privileged and never adds a capability back.\n",
				"    network_mode: bridge\n",
			},
		},
		{
			name: "no-mandatory-telemetry",
			rule: CheckNoMandatoryTelemetry,
			path: "fixture/compose.yaml",
			trips: []string{
				"      TELEMETRY_ENABLED: \"true\"\n",
				"      BACKUP_MANAGER_ANALYTICS: on\n",
				"      SENTRY_DSN: https://key@sentry.invalid/1\n",
				"      REPORT_URL: https://metrics.example.invalid/v1/ingest\n",
				"      USAGE_STATS: 1\n",
				"      TELEMETRY_ENDPOINT: https://collector.example.invalid/ingest\n",
				"      CRASH_REPORT_HOST: crash.example.invalid\n",
				"      REPORT_URL: http://203.0.113.9/collect\n",
				"      REPORT_URL: http://[2001:db8::1]:4318/v1/traces\n",
			},
			clean: []string{
				"      TELEMETRY_ENABLED: \"false\"\n",
				"      ANALYTICS: off\n",
				"      TELEMETRY_ENDPOINT: \"\"\n",
				"      USAGE_STATS: none\n",
				"# There is no telemetry in this release, so there is nothing to disable.\n",
				"      PUBLIC_BASE_URL: http://localhost:8080\n",
				"      UPSTREAM_ADDR: http://backup-manager:8080\n",
				"      PUBLIC_BASE_URL: http://tower.local:8080\n",
				"  home: https://github.com/spdrman/rclone-manager\n",
				"  icon: https://raw.githubusercontent.com/spdrman/rclone-manager/main/docs/submission/icon.svg\n",
				"      ENGINE: http://192.168.1.20:8080\n",
				"      ENGINE: http://127.0.0.1:8080\n",
				"      ENGINE: http://10.7.0.4:8080\n",
				"      ENGINE: http://100.64.5.9:8080\n",
				"      ENGINE: http://[fd00::1]:8080\n",
				"      ENGINE: http://[::1]:8080\n",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, body := range tc.trips {
				if v := tc.rule(tc.path, body); len(v) == 0 {
					t.Errorf("rule accepted a package it must refuse:\n%s", body)
				}
			}
			for _, body := range tc.clean {
				if v := tc.rule(tc.path, body); len(v) > 0 {
					t.Errorf("rule refused a package it must accept: %s\n%s", oneLine(v), body)
				}
			}
		})
	}
}

// TestSelfUpdateFetchRuleOnlyReadsExecutedFiles is the control for the
// one half of no-self-update that is deliberately narrower than the
// others. A compose file mentioning curl in a comment is prose; a DSM
// lifecycle script running it downloads code the store never reviewed,
// and DSM's scripts have no extension at all, which is exactly where such
// a thing would hide.
func TestSelfUpdateFetchRuleOnlyReadsExecutedFiles(t *testing.T) {
	body := "curl -fsSL https://example.invalid/x | sh\n"

	for _, path := range []string{
		"spk/assets/scripts/postinst",
		"spk/assets/scripts/start-stop-status",
		"spk/assets/scripts/common.sh",
		"installer",
	} {
		if v := CheckNoSelfUpdate(path, body); len(v) == 0 {
			t.Errorf("%s is executed by the platform and a download in it must be refused", path)
		}
	}
	for _, path := range []string{
		"catalog/app.yaml",
		"README.md",
		"template/backup-manager.xml",
	} {
		if v := CheckNoSelfUpdate(path, body); len(v) > 0 {
			t.Errorf("%s is read, not executed, so a mention must not be a finding: %s", path, oneLine(v))
		}
	}
}

// TestMutatingARealPackagedFileTripsTheHardRules is the mutation test
// against the real tree.
//
// A fixture proves a rule can fire. It does not prove the rule fires on
// the shape this repository's actual packages take, which is the gap that
// let `\bpassword` miss ADMIN_PASSWORD in every env file here while
// passing its own tests. So every target's real packaged files are read,
// each of the four violations is appended to each of them in memory, and
// the rule has to find it. The unmutated file has to stay clean, which is
// the other half: a rule that fires on everything is no better.
func TestMutatingARealPackagedFileTripsTheHardRules(t *testing.T) {
	s := MustLoadSubmission()

	injections := []struct {
		rule func(path, text string) []Violation
		name string
		body string
	}{
		{CheckNoSelfUpdate, "no-self-update", "\npull_policy: always\n"},
		{CheckNoFloatingTag, "no-floating-tag", "\n    image: ghcr.io/spdrman/backup-manager:latest\n"},
		{CheckNoPrivilegedMode, "no-privileged-mode", "\n    privileged: true\n"},
		{CheckNoMandatoryTelemetry, "no-mandatory-telemetry", "\n      TELEMETRY_ENDPOINT: https://collector.example.invalid/ingest\n"},
	}

	for id, sp := range s.Providers {
		if len(sp.ArtifactFiles) == 0 {
			continue
		}
		t.Run(id, func(t *testing.T) {
			files := realPackagedFiles(t, sp.ArtifactFiles)
			if len(files) == 0 {
				t.Fatalf("%s declares %v and none of it holds a file to mutate", id, sp.ArtifactFiles)
			}
			for _, inj := range injections {
				mutated := 0
				for path, body := range files {
					if v := inj.rule(path, body); len(v) > 0 {
						t.Errorf("%s already fires on the unmutated %s: %s", inj.name, path, oneLine(v))
						continue
					}
					if v := inj.rule(path, body+inj.body); len(v) == 0 {
						t.Errorf("%s did not fire after injecting into the real %s", inj.name, path)
						continue
					}
					mutated++
				}
				if mutated == 0 {
					t.Errorf("%s was never actually exercised against %s's real files", inj.name, id)
				}
			}
		})
	}
}

func realPackagedFiles(t *testing.T, roots []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, root := range roots {
		err := filepath.WalkDir(Path(root), func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			data, readErr := os.ReadFile(p)
			if readErr != nil {
				return readErr
			}
			rel, _ := filepath.Rel(Path("."), p)
			out[rel] = string(data)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func TestImageTagUnderstandsEveryFloatingForm(t *testing.T) {
	cases := []struct {
		ref  string
		kind tagKind
	}{
		{"ghcr.io/spdrman/backup-manager:1.0.0", tagPinned},
		{"ghcr.io/spdrman/backup-manager@sha256:" + strings.Repeat("b", 64), tagPinned},
		{"registry.invalid:5000/spdrman/backup-manager:1.0.0", tagPinned},
		{"backup-manager:${VERSION:-dev}", tagVariable},
		{"ghcr.io/spdrman/backup-manager", tagAbsent},
		{"registry.invalid:5000/spdrman/backup-manager", tagAbsent},
		{"ghcr.io/spdrman/backup-manager:latest", tagLatest},
		{"ghcr.io/spdrman/backup-manager:LATEST", tagLatest},
		{"backup-manager:${VERSION:-latest}", tagFloatingDefault},
	}
	for _, tc := range cases {
		if _, got := ImageTag(tc.ref); got != tc.kind {
			t.Errorf("ImageTag(%q) = %v, want %v", tc.ref, got, tc.kind)
		}
	}
}

// ---------------------------------------------------------------------
// The eight drift elements
// ---------------------------------------------------------------------

// canonicalCompose is a two-service profile that agrees with
// canonical.json on every one of the eight elements. Every drift control
// below is this text with exactly one thing changed, so a control that
// fires proves the element noticed that change and not something else
// about the fixture.
const canonicalCompose = `
services:
  backup-manager:
    image: ghcr.io/spdrman/backup-manager:0.3.0
    command: ["/backup-manager-web", "serve"]
    user: "568:568"
    read_only: true
    privileged: false
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    tmpfs: ["/tmp:size=64m"]
    environment:
      LISTEN_ADDR: ":8080"
    volumes:
      - "/host/state:/data/state"
      - "/host/backups:/data/backups"
      - "/host/config:/etc/backup-manager/config"
      - "/host/id_ed25519:/etc/backup-manager/id_ed25519:ro"
      - "/host/known_hosts:/etc/backup-manager/known_hosts:ro"
  backup-manager-ui:
    image: ghcr.io/spdrman/backup-manager:0.3.0
    command: ["/backup-manager-web", "serve-ui"]
    user: "568:568"
    read_only: true
    privileged: false
    cap_drop: [ALL]
    security_opt: ["no-new-privileges:true"]
    tmpfs: ["/tmp:size=16m"]
    environment:
      LISTEN_ADDR: ":8080"
      UPSTREAM_ADDR: "http://backup-manager:8080"
    healthcheck:
      test: ["CMD", "/backup-manager-web", "healthcheck"]
    ports:
      - "8080:8080"
`

// driftTarget builds a target whose packaging metadata is a throwaway
// compose file, keeping the real declaration for id so the elements that
// consume the cross-provider matrix's verdict resolve exactly as they do
// in a real run.
func driftTarget(t *testing.T, id, compose string) targetUnderTest {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "compose.yaml"), compose)

	conf := MustLoadConformance()
	sub := MustLoadSubmission()
	spec := conf.Providers[id]
	spec.Metadata = Metadata{Kind: "compose", Root: relToRepo(t, dir), Compose: "compose.yaml"}

	return targetUnderTest{
		providerUnderTest: providerUnderTest{id: id, spec: spec, canonical: MustLoad()},
		sub:               sub.Providers[id],
		bundle:            sub.Bundle,
		conf:              conf,
	}
}

func TestEveryDriftElementFailsOnADeliberateMismatch(t *testing.T) {
	cases := []struct {
		capability string
		// provider whose real declaration makes the unmutated fixture a
		// PASS for this element.
		provider string
		mutate   func(string) string
		wants    string
	}{
		{
			capability: "drift-image-reference",
			provider:   "truenas",
			mutate:     func(s string) string { return strings.ReplaceAll(s, "backup-manager:0.3.0", "backup-manager:9.9.9") },
			wants:      "9.9.9",
		},
		{
			capability: "drift-required-mounts",
			provider:   "truenas",
			mutate:     func(s string) string { return strings.Replace(s, "known_hosts:ro", "known_hosts", 1) },
			wants:      "known_hosts",
		},
		{
			capability: "drift-expected-ports",
			provider:   "truenas",
			mutate: func(s string) string {
				return strings.Replace(s, `      LISTEN_ADDR: ":8080"
    volumes:`, `      LISTEN_ADDR: ":8080"
    ports:
      - "8081:8080"
    volumes:`, 1)
			},
			wants: "publish a port",
		},
		{
			capability: "drift-health-check",
			provider:   "truenas",
			mutate: func(s string) string {
				return strings.Replace(s, `["CMD", "/backup-manager-web", "healthcheck"]`, `["CMD", "true"]`, 1)
			},
			wants: "healthcheck",
		},
		{
			capability: "drift-runtime-profile",
			provider:   "truenas",
			mutate:     func(s string) string { return strings.Replace(s, "read_only: true", "read_only: false", 1) },
			wants:      "read_only",
		},
		{
			capability: "drift-forbidden-privileges",
			provider:   "truenas",
			mutate:     func(s string) string { return strings.Replace(s, "privileged: false", "privileged: true", 1) },
			wants:      "privileged",
		},
		{
			capability: "drift-api-compatibility",
			provider:   "truenas",
			mutate:     func(s string) string { return strings.Replace(s, `LISTEN_ADDR: ":8080"`, `LISTEN_ADDR: ":9090"`, 1) },
			wants:      "9090",
		},
	}

	for _, tc := range cases {
		t.Run(tc.capability, func(t *testing.T) {
			e, ok := DriftElementFor(tc.capability)
			if !ok {
				t.Fatalf("%s is not a drift element", tc.capability)
			}
			rule := driftRule(e)

			// Positive control: the unmutated profile satisfies this
			// element, so the refusal below is about the mutation rather
			// than about a fixture that never agreed with anything.
			if ok, detail := rule(driftTarget(t, tc.provider, canonicalCompose)); !ok {
				t.Fatalf("the canonical profile must satisfy %s: %s", tc.capability, detail)
			}

			mutated := tc.mutate(canonicalCompose)
			if mutated == canonicalCompose {
				t.Fatal("the mutation changed nothing, so this control proves nothing")
			}
			ok, detail := rule(driftTarget(t, tc.provider, mutated))
			if ok {
				t.Fatalf("%s accepted a profile that drifted from the canonical contract", tc.capability)
			}
			if !strings.Contains(detail, tc.wants) {
				t.Errorf("%s refused for a reason that does not name the drift (%q): %s", tc.capability, tc.wants, detail)
			}
		})
	}

	// The eighth element has nothing to do with a compose file: a
	// provider's architecture claim is a statement its own package makes,
	// and Synology is the one target that makes one.
	t.Run("drift-architecture-support", func(t *testing.T) {
		e, _ := DriftElementFor("drift-architecture-support")
		rule := driftRule(e)

		conf := MustLoadConformance()
		sub := MustLoadSubmission()
		good := targetUnderTest{
			providerUnderTest: providerUnderTest{id: "synology", spec: conf.Providers["synology"], canonical: MustLoad()},
			sub:               sub.Providers["synology"],
			bundle:            sub.Bundle,
			conf:              conf,
		}
		if ok, detail := rule(good); !ok {
			t.Fatalf("Synology's real architecture claim must satisfy the element: %s", detail)
		}

		bad := good
		bad.spec.Metadata.ArchitectureClaim.Architectures = []string{"riscv64"}
		if ok, _ := rule(bad); ok {
			t.Error("the element accepted a package claiming an architecture the build does not produce")
		}
	})
}

// TestDriftElementsRefuseATargetWithNothingToCompare is the vacuity
// control. Five of the eight elements read a parsed service, and a target
// with none of those would satisfy "no violations found" trivially, which
// is how Synology's container-shaped cells would go green off a package
// that is not a container at all.
func TestDriftElementsRefuseATargetWithNothingToCompare(t *testing.T) {
	conf := MustLoadConformance()
	sub := MustLoadSubmission()
	empty := targetUnderTest{
		providerUnderTest: providerUnderTest{id: "synology", spec: conf.Providers["synology"], canonical: MustLoad()},
		sub:               sub.Providers["synology"],
		bundle:            sub.Bundle,
		conf:              conf,
	}

	for _, e := range DriftElements {
		if e.MatrixCapability != "" {
			continue
		}
		ok, detail := driftRule(e)(empty)
		if ok {
			t.Errorf("%s passed a target that declares no container service at all", e.Capability)
		}
		if !strings.Contains(detail, "no container services") {
			t.Errorf("%s refused for the wrong reason: %s", e.Capability, detail)
		}
	}
}

// TestDelegatedDriftElementsDoNotLaunderANonDecision is the control for
// the one judgement call in the drift gate: an element that consumes the
// cross-provider matrix's verdict counts a PASS and nothing else. A
// NOT_APPLICABLE means the matrix declined to decide, and treating that
// as agreement with the canonical contract is the exact move a drift gate
// exists to stop.
func TestDelegatedDriftElementsDoNotLaunderANonDecision(t *testing.T) {
	e, _ := DriftElementFor("drift-architecture-support")
	rule := driftRule(e)

	conf := MustLoadConformance()
	sub := MustLoadSubmission()

	// TrueNAS makes no architecture claim of its own, so the matrix
	// records NOT_APPLICABLE. The element must not read that as a pass.
	tn := targetUnderTest{
		providerUnderTest: providerUnderTest{id: "truenas", spec: conf.Providers["truenas"], canonical: MustLoad()},
		sub:               sub.Providers["truenas"],
		bundle:            sub.Bundle,
		conf:              conf,
	}
	ok, detail := rule(tn)
	if ok {
		t.Error("a delegated element treated a NOT_APPLICABLE matrix cell as agreement with the canonical contract")
	}
	if !strings.Contains(detail, string(OutcomeNotApplicable)) {
		t.Errorf("the refusal should name the matrix outcome it consumed, got: %s", detail)
	}
	// And it must say which matrix cell it consumed, so nobody has to
	// guess where the answer came from.
	if !strings.Contains(detail, e.MatrixCapability) {
		t.Errorf("the detail should name %s, got: %s", e.MatrixCapability, detail)
	}
}

// ---------------------------------------------------------------------
// The submission materials
// ---------------------------------------------------------------------

func TestMaterialRulesRefuseADraft(t *testing.T) {
	const good = `Backup Manager pulls backup artifacts off a remote SFTP source on a schedule,
verifies each one against the hash the source published, and retains them under a
retention policy the administrator confirms before anything is deleted. It runs on the
NAS, stores everything on the NAS, and talks to nothing except the sources an
administrator configured.`

	if v := CheckMaterial("f.md", good, 200, []string{"sftp", "retention"}); len(v) > 0 {
		t.Fatalf("a real listing must be accepted: %s", oneLine(v))
	}
	cases := []struct {
		name string
		text string
		min  int
		want []string
	}{
		{"too short to be a listing", "Backups. SFTP. Retention.", 200, []string{"sftp", "retention"}},
		{"a draft marker", good + "\n\nTODO: rewrite this.", 200, []string{"sftp", "retention"}},
		{"never covers a required subject", good, 200, []string{"sftp", "retention", "encryption at rest"}},
	}
	for _, tc := range cases {
		if v := CheckMaterial("f.md", tc.text, tc.min, tc.want); len(v) == 0 {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}

func TestStoreIconRuleRefusesAnInAppMark(t *testing.T) {
	const good = `<svg xmlns="http://www.w3.org/2000/svg" width="256" height="256"><title>Backup Manager</title><rect width="256" height="256" fill="#1f3a5f"/></svg>`
	if v := CheckStoreIcon("icon.svg", good, 256); len(v) > 0 {
		t.Fatalf("a listing icon must be accepted: %s", oneLine(v))
	}

	cases := map[string]string{
		"paints with currentColor, so it renders black or invisible on a store's own page": `<svg xmlns="http://www.w3.org/2000/svg" width="256" height="256"><title>x</title><rect width="256" height="256" fill="currentColor"/></svg>`,
		"declares no intrinsic width":                    `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 48 48"><title>x</title></svg>`,
		"is drawn far smaller than a listing renders it": `<svg xmlns="http://www.w3.org/2000/svg" width="48" height="48"><title>x</title></svg>`,
		"carries no title, so it has no accessible name": `<svg xmlns="http://www.w3.org/2000/svg" width="256" height="256"></svg>`,
		"is empty": ``,
	}
	for name, body := range cases {
		if v := CheckStoreIcon("icon.svg", body, 256); len(v) == 0 {
			t.Errorf("accepted an icon that %s", name)
		}
	}

	// The shipped in-app mark is the case that motivated this rule, and
	// it is still the right file for the shell: this asserts the two are
	// genuinely different files rather than one file nobody checked.
	inApp, err := os.ReadFile(Path("ui/shared/public/icon.svg"))
	if err != nil {
		t.Fatal(err)
	}
	if v := CheckStoreIcon("ui/shared/public/icon.svg", string(inApp), 256); len(v) == 0 {
		t.Error("the in-app mark passes the store-icon rule, which means the rule no longer distinguishes the two")
	}
}

func TestChecklistRuleHoldsARowToTheTree(t *testing.T) {
	required := []string{"install", "recovery"}
	const good = `| Item | Where it is | State |
|---|---|---|
| install | ` + "`docs/deployment.md`" + ` | ready |
| recovery | ` + "`docs/recovery-without-a-terminal.md`" + ` | ready |
`
	if v := CheckChecklist("c.md", good, required); len(v) > 0 {
		t.Fatalf("a complete checklist must be accepted: %s", oneLine(v))
	}

	cases := map[string]string{
		"a missing row": `| Item | Where it is | State |
|---|---|---|
| install | ` + "`docs/deployment.md`" + ` | ready |
`,
		"a ready row naming a path that is not in the tree": strings.Replace(good, "docs/deployment.md", "docs/nowhere.md", 1),
		"a state outside the fixed four":                    strings.Replace(good, "| ready |", "| probably |", 1),
		"a blocked row that names no issue":                 strings.Replace(good, "| ready |", "| blocked |", 1),
		"an operator row naming no procedure":               strings.Replace(good, "| `docs/deployment.md` | ready |", "|  | operator |", 1),
		"a row nothing measures":                            good + "| something-else | `docs/deployment.md` | ready |\n",
		"no table at all":                                   "Everything is fine, honestly.\n",
	}
	for name, body := range cases {
		if v := CheckChecklist("c.md", body, required); len(v) == 0 {
			t.Errorf("accepted a checklist with %s", name)
		}
	}

	// A blocked row that names its issue is accepted, because the whole
	// point of the state is that another work package owns the material.
	blocked := strings.Replace(good, "| `docs/recovery-without-a-terminal.md` | ready |", "| owned elsewhere | blocked #88 |", 1)
	if v := CheckChecklist("c.md", blocked, required); len(v) > 0 {
		t.Errorf("a blocked row naming its issue must be accepted: %s", oneLine(v))
	}
}

// ---------------------------------------------------------------------
// The readiness verdict
// ---------------------------------------------------------------------

// TestReadinessVerdictDistinguishesItsFiveStates is the control for the
// format #178 consumes. Every state has to be reachable and they have to
// be reachable for different reasons: a verdict type where two of the
// five can never occur is a boolean with extra words.
func TestReadinessVerdictDistinguishesItsFiveStates(t *testing.T) {
	s := MustLoadSubmission()
	conf := s.AsConformance(MustLoadConformance())

	build := func(id string, outcomes map[string]Outcome) *Matrix {
		m := NewMatrix(conf)
		for _, cap := range s.Capabilities {
			o, ok := outcomes[cap.ID]
			if !ok {
				o = OutcomePass
			}
			m.Record(Result{Provider: id, Capability: cap.ID, Outcome: o, Detail: "fixture"})
		}
		return m
	}

	// BLOCKED is the one state that needs a declaration as well as an
	// outcome, because the row's blocker list is read off the
	// declaration. The declaration is planted here rather than borrowed
	// from submission.json: a control that reaches into the live data
	// for the one cell that happens to be blocked today stops proving
	// anything the day that blocker is resolved, which is exactly how
	// the two staleness findings on this PR were reached.
	sBlocked := MustLoadSubmission()
	blockedCell := map[string]Cell{}
	for id, cell := range sBlocked.Providers["truenas"].Cells {
		blockedCell[id] = cell
	}
	blockedCell["artifact-provenance"] = Cell{
		Declared: DeclBlocked, Blocker: "#9999", ExpectedDetail: "fixture",
		Reason: "a planted blocker, so this control owns the state it measures",
	}
	blockedTruenas := sBlocked.Providers["truenas"]
	blockedTruenas.Cells = blockedCell
	sBlocked.Providers["truenas"] = blockedTruenas

	cases := []struct {
		name     string
		id       string
		sub      Submission
		outcomes map[string]Outcome
		want     Readiness
	}{
		{"everything held", "truenas", s, nil, ReadySubmit},
		{"a hardware step outstanding", "truenas", s, map[string]Outcome{"materials-screenshots": OutcomePendingOperator}, ReadyPendingOperator},
		{"a rule nobody can decide", "truenas", sBlocked, map[string]Outcome{"artifact-provenance": OutcomeBlocked}, ReadyBlocked},
		{"a rule that failed", "truenas", s, map[string]Outcome{"no-privileged-mode": OutcomeFail}, ReadyNot},
		{"no artifact to preflight", "ugos", s, nil, ReadyNotYetApplicable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ReadinessFor(build(tc.id, tc.outcomes), tc.sub, tc.id)
			if got.Readiness != tc.want {
				t.Errorf("got %s, want %s (%s)", got.Readiness, tc.want, got.Why)
			}
			if strings.TrimSpace(got.Why) == "" {
				t.Error("a recorded verdict with no reason is not a recorded verdict")
			}
		})
	}

	// The positive control for the planted declaration: with the same
	// blocked outcome and no declared blocker, the row must not read
	// BLOCKED, so the state above is reached by the declaration under
	// test rather than by the outcome alone.
	if got := ReadinessFor(build("truenas", map[string]Outcome{"artifact-provenance": OutcomeBlocked}), s, "truenas"); got.Readiness == ReadyBlocked {
		t.Error("a blocked outcome with no declared blocker read as BLOCKED, so the planted declaration proves nothing")
	}

	// Precedence, asserted rather than assumed. A failure outranks a
	// blocker outranks a hardware step, because a verdict that reported
	// "blocked" while a rule was failing would send somebody to chase the
	// wrong issue.
	m := build("truenas", map[string]Outcome{
		"no-privileged-mode":    OutcomeFail,
		"artifact-provenance":   OutcomeBlocked,
		"materials-screenshots": OutcomePendingOperator,
	})
	if got := ReadinessFor(m, sBlocked, "truenas"); got.Readiness != ReadyNot {
		t.Errorf("a failure alongside a blocker read as %s, want %s", got.Readiness, ReadyNot)
	}

	// A cell the run never recorded is a failure, not an absence. An
	// unrun rule reads as a passing one otherwise.
	partial := NewMatrix(conf)
	partial.Record(Result{Provider: "truenas", Capability: "no-self-update", Outcome: OutcomePass})
	if got := ReadinessFor(partial, s, "truenas"); got.Readiness != ReadyNot {
		t.Errorf("a target most of whose rules never ran read as %s, want %s", got.Readiness, ReadyNot)
	}
}

// TestHasArtifactIsDerivedRatherThanDeclared is the control for the one
// thing keeping UGREEN out of this gate. If it could be set, "not ready
// yet" would be the single most useful thing for a declaration to lie
// about: it turns every rule off at once and reads as a schedule.
func TestHasArtifactIsDerivedRatherThanDeclared(t *testing.T) {
	c := MustLoadConformance()

	if HasArtifact(c.Providers["ugos"]) {
		t.Error("UGOS has an artifact today, which would mean #83 landed and this row should be decided rather than deferred")
	}
	for _, id := range c.ProviderIDsFor(PhaseFourEpic) {
		if !HasArtifact(c.Providers[id]) {
			t.Errorf("%s is shipped by EPIC B and reports no artifact, so its whole column would defer", id)
		}
	}

	// The day EPIC D's package lands, the row starts being decided with
	// no edit to this package: a metadata kind is enough, and so is a
	// single store artifact.
	ugos := c.Providers["ugos"]
	ugos.Metadata.Kind = "compose"
	if !HasArtifact(ugos) {
		t.Error("declaring a packaging format is not enough to make a target real, so #83 landing would not switch its column on")
	}
	ugos = c.Providers["ugos"]
	ugos.Metadata.StoreArtifacts = []string{"upk/INFO"}
	if !HasArtifact(ugos) {
		t.Error("shipping a store artifact is not enough to make a target real")
	}
}

// ---------------------------------------------------------------------
// Registering a target this issue never ran against
// ---------------------------------------------------------------------

// TestATargetThisIssueNeverRanAgainstGetsAVerdict is #90's own acceptance
// criterion, and #81's "a new NAS should cost metadata, not an
// implementation" made measurable rather than rhetorical.
//
// A target nothing in this repository has ever shipped is registered
// entirely by declaration: a conformance column, a submission row, a
// compose file and a workflow document, all built in a temp directory.
// Not one rule, check or element is added or changed for it, which is the
// claim. If registering a target required touching this package, the loop
// below could not be written.
//
// The name is one no product will ever have, and that is load-bearing.
// This fixture used to be Portainer, chosen because #170 had named it and
// none of it existed yet; #170 then shipped it, the store-submission
// procedure grew a real "Portainer CE" section, and the one cell this
// control turns on - a blocked proactive-alert-delivery, blocked because
// nobody had written that section - started passing. A fixture a real
// change can turn valid is a control with an expiry date on it, and this
// is the second one in this repository to hit that (see
// scripts/architecture/selftest.sh, where the planted violation was
// apps/casaos until CasaOS became real).
func TestATargetThisIssueNeverRanAgainstGetsAVerdict(t *testing.T) {
	dir := t.TempDir()
	rel := relToRepo(t, dir)
	write(t, filepath.Join(dir, "stack.yml"), canonicalCompose)

	sub := MustLoadSubmission()
	workflow := filepath.Join(dir, "selftest-unshipped-target.md")
	write(t, workflow, `# Selftest Unshipped Target distribution workflow

https://example.invalid/selftest-unshipped-target

| Item | Where it is | State |
|---|---|---|
| install | `+"`docs/deployment.md`"+` | ready |
| update | `+"`docs/deployment.md`"+` | ready |
| remove | `+"`docs/deployment.md`"+` | ready |
| recovery | `+"`"+sub.Bundle.Recovery+"`"+` | ready |
| support | `+"`docs/submission/support-source-license.md`"+` | ready |
`)

	// The conformance column. Everything structural about a target lives
	// here, which is the point: the submission row below adds only what
	// submission needs.
	conf := MustLoadConformance()
	conf.Providers["selftest-unshipped-target"] = Provider{
		DisplayName: "Selftest Unshipped Target",
		Tier:        "C",
		Epic:        SubmissionEpic,
		WorkPackage: "6.6",
		Metadata: Metadata{
			Kind:           "compose",
			Root:           rel,
			Compose:        "stack.yml",
			StoreArtifacts: []string{"stack.yml"},
		},
		ScanRoots: []string{rel},
		Cells: map[string]Cell{
			"canonical-image-parity": {Declared: DeclSupported},
			"api-path-isolation":     {Declared: DeclSupported},
			"architecture-parity":    {Declared: DeclNotApplicable, Reason: "makes no architecture claim of its own"},
		},
	}

	cells := map[string]Cell{}
	for _, cap := range sub.Capabilities {
		cells[cap.ID] = Cell{Declared: DeclSupported}
	}
	for _, id := range MaterialsCapabilityIDs {
		cells[id] = Cell{Declared: DeclNotApplicable, Reason: "no store to submit to; a documented workflow instead"}
	}
	cells["drift-architecture-support"] = Cell{Declared: DeclNotApplicable, Reason: "makes no architecture claim of its own"}
	cells["proactive-alert-delivery"] = Cell{
		Declared: DeclBlocked, Blocker: "#90", ExpectedDetail: "has no section for",
		Reason: "the state this control exists to reproduce: a target registered by declaration before anyone has written its hardware section, so the one rule that needs that section cannot be decided. #90 owns the preflight that decides it once the section exists.",
	}
	cells["artifact-provenance"] = Cell{Declared: DeclSupported}
	sub.Providers["selftest-unshipped-target"] = SubmissionProvider{
		Store: Store{
			Kind:      "none",
			Checklist: relToRepo(t, workflow),
			Reference: "https://example.invalid/selftest-unshipped-target",
		},
		ArtifactFiles: []string{filepath.Join(rel, "stack.yml")},
		Cells:         cells,
	}

	// The completeness guard applies to it like any other column: a
	// target registered with a gap in its declarations is refused rather
	// than quietly half-checked.
	c := sub.AsConformance(conf)
	if findings := auditPreflightDeclarations(c.Providers["selftest-unshipped-target"], c.CapabilityIDs()); len(findings) > 0 {
		t.Fatalf("the registration is itself incomplete: %v", findings)
	}

	run := runPreflight(t, sub, conf)

	row := run.ReadinessOf("selftest-unshipped-target")
	if row.Readiness != ReadyBlocked {
		t.Errorf("the unshipped target is recorded %s (%s), want %s: nothing about it failed, and one rule cannot be decided", row.Readiness, row.Why, ReadyBlocked)
	}
	if strings.Join(row.Blockers, ",") != "#90" {
		t.Errorf("the unshipped target's blockers are %v, want #90", row.Blockers)
	}
	if len(row.Failed) > 0 {
		t.Errorf("a target registered by declaration alone failed %v, so registration is not enough after all", row.Failed)
	}
	if len(run.Matrix.Results["selftest-unshipped-target"]) != len(sub.Capabilities) {
		t.Errorf("the unshipped target produced %d results for %d rules", len(run.Matrix.Results["selftest-unshipped-target"]), len(sub.Capabilities))
	}

	// And it is inside the gate, not merely reported beside it. A target
	// that gets a verdict nobody counts is not registered, it is
	// decorated.
	v := run.Matrix.Verdict(SubmissionEpic)
	found := false
	for _, id := range v.Providers {
		if id == "selftest-unshipped-target" {
			found = true
		}
	}
	if !found {
		t.Errorf("the unshipped target is not in the Phase 5 verdict, which was computed over %v", v.Providers)
	}
}

// ---------------------------------------------------------------------
// The two guards this design rests on
// ---------------------------------------------------------------------

func TestThePreflightCompletenessGuardStillFires(t *testing.T) {
	c := MustLoadSubmission().AsConformance(MustLoadConformance())
	caps := c.CapabilityIDs()

	// Positive control: the real declarations are complete, so a finding
	// below is about the mutation.
	real := c.Providers["truenas"]
	if findings := auditPreflightDeclarations(real, caps); len(findings) > 0 {
		t.Fatalf("the shipped declarations must be complete: %v", findings)
	}

	mutate := func(f func(map[string]Cell)) Provider {
		cells := map[string]Cell{}
		for k, v := range real.Cells {
			cells[k] = v
		}
		f(cells)
		p := real
		p.Cells = cells
		return p
	}

	cases := map[string]Provider{
		"a rule left undeclared": mutate(func(m map[string]Cell) { delete(m, "no-privileged-mode") }),
		"an unexplained exemption": mutate(func(m map[string]Cell) {
			m["no-privileged-mode"] = Cell{Declared: DeclNotApplicable}
		}),
		"a blocker that is not an issue": mutate(func(m map[string]Cell) {
			m["artifact-provenance"] = Cell{Declared: DeclBlocked, Reason: "later", Blocker: "soon", ExpectedDetail: "x"}
		}),
		"a blocker with nothing tying it to the failure it excuses": mutate(func(m map[string]Cell) {
			m["artifact-provenance"] = Cell{Declared: DeclBlocked, Reason: "later", Blocker: "#174"}
		}),
		"a declaration outside the four": mutate(func(m map[string]Cell) {
			m["no-privileged-mode"] = Cell{Declared: "probably-fine"}
		}),
		"an outcome for something that is not a rule": mutate(func(m map[string]Cell) {
			m["no-cosmic-rays"] = Cell{Declared: DeclSupported}
		}),
	}
	for name, p := range cases {
		if findings := auditPreflightDeclarations(p, caps); len(findings) == 0 {
			t.Errorf("the completeness guard accepted %s", name)
		}
	}
}

// TestThePreflightStalenessGuardStillFires is the other half, and the one
// that stops submission.json becoming a list of reasons not to look. A
// declaration the repository has outgrown is worse than no declaration:
// it is a documented reason to stop checking.
func TestThePreflightStalenessGuardStillFires(t *testing.T) {
	cap := Capability{ID: "no-privileged-mode", Mode: ModeRepo}

	for _, declared := range []Declaration{DeclUnsupported, DeclNotApplicable} {
		cell := Cell{Declared: declared, Reason: "recorded as not applying here"}
		r := resolveWith(SubmissionSource, "truenas", cap, cell, true, "the check now passes")
		if r.Outcome != OutcomeFail {
			t.Errorf("a %s cell whose check passes resolved %s, want %s", declared, r.Outcome, OutcomeFail)
		}
		// The message has to name the exact edit, not merely the side.
		// Both blocking findings on this PR were repaired by editing one
		// field, and the message that reported them pointed at the wrong
		// file and left the reader to work out that the regeneration
		// command it offered was not the fix.
		if !strings.Contains(r.Detail, "Re-derive submission.json -> providers.truenas.cells.no-privileged-mode.declared rather than the check") {
			t.Errorf("the staleness failure should name the file and field to edit, got: %s", r.Detail)
		}
	}

	// A blocked cell excuses the failure it named and nothing else, which
	// is the half a blocker number alone would silence.
	blocked := Cell{Declared: DeclBlocked, Blocker: "#174", ExpectedDetail: "is not an ancestor of HEAD", Reason: "the release manifest"}
	if r := resolveWith(SubmissionSource, "truenas", cap, blocked, false, "release manifest pins commit c51a07f, which is not an ancestor of HEAD"); r.Outcome != OutcomeBlocked {
		t.Errorf("the failure the blocker names resolved %s, want %s", r.Outcome, OutcomeBlocked)
	}
	if r := resolveWith(SubmissionSource, "truenas", cap, blocked, false, "the package now requests privileged mode"); r.Outcome != OutcomeFail {
		t.Errorf("an unrelated failure was swallowed by the blocker: %s", r.Outcome)
	}

	// And the blocked cell still fails when the blocker has been fixed
	// underneath it, so a stale excuse cannot outlive its reason.
	if r := resolveWith(SubmissionSource, "truenas", cap, blocked, true, "manifest commit is reachable"); r.Outcome != OutcomeFail {
		t.Errorf("a blocked cell whose check now passes resolved %s, want %s", r.Outcome, OutcomeFail)
	}
}

// TestTheAPIContractAnchorsCannotSilentlyStopChecking is the control for
// the one extractor in this package that reads Go and TypeScript with a
// regular expression. Crude is fine; crude and silent is not, so an
// anchor that matches nothing has to be an error naming the file rather
// than an empty result read as agreement.
func TestTheAPIContractAnchorsCannotSilentlyStopChecking(t *testing.T) {
	for _, anchor := range apiContractAnchors {
		data, err := os.ReadFile(Path(anchor.path))
		if err != nil {
			t.Fatalf("%s: %v", anchor.path, err)
		}
		m := anchor.re.FindSubmatch(data)
		if m == nil {
			t.Errorf("%s no longer states the API base path in a form the extractor can read", anchor.path)
			continue
		}
		if got := string(m[1]); got != APIBasePath {
			t.Errorf("%s serves %s, want %s", anchor.path, got, APIBasePath)
		}
		// The control: the same pattern must not match a file that moved
		// the base path, and must not match one that dropped it.
		moved := strings.ReplaceAll(string(data), APIBasePath, "/api/v2")
		if hit := anchor.re.FindSubmatch([]byte(moved)); hit == nil {
			t.Errorf("%s's pattern stopped matching when the version moved, so a bump would read as a missing anchor rather than as a disagreement", anchor.path)
		} else if string(hit[1]) == APIBasePath {
			t.Errorf("%s's pattern reported the old base path from a file that no longer contains it", anchor.path)
		}
	}
}

// TestTelemetryRuleReadsAddressLiteralsAsHosts is the control for the
// hole the consolidated review of #191 found in isPublicHost.
//
// The rule's stated premise is that an outbound host in a shipped package
// is a telemetry endpoint until it is shown otherwise, which is a
// refusal. The earlier classifier skipped any host whose first
// dot-separated label parsed as an integer, which excluded every IPv4 and
// IPv6 literal, public or private, so the same URL was reported when it
// named a host and ignored when it gave an address. That is the fail-open
// half of a fail-closed rule, and only the unnamed path is affected: a
// public address behind a recognised telemetry variable name was always
// caught by the name-based half, which is why nothing here went red.
//
// Both directions are asserted from one table of URL shapes, because
// either half alone is satisfied by something broken: a classifier that
// calls everything public passes the first loop, and one that calls
// nothing public passes the second.
func TestTelemetryRuleReadsAddressLiteralsAsHosts(t *testing.T) {
	// REPORT_URL deliberately: it is not on the telemetry variable list,
	// so the only thing that can report these is the URL scan, which is
	// the path isPublicHost exists to cover.
	const varName = "      REPORT_URL: "

	reportable := []string{
		"https://collector.example-vendor.invalid/ingest",
		"http://203.0.113.9/collect",
		"http://198.51.100.7:9411/api/v2/spans",
		"http://[2001:db8::1]:4318/v1/traces",
		"https://[2606:4700:4700::1111]/ingest",
		"http://8.8.8.8/collect",
	}
	for _, u := range reportable {
		if v := CheckNoMandatoryTelemetry("fixture/compose.yaml", varName+u+"\n"); len(v) != 1 {
			t.Errorf("%s produced %d violation(s), want 1: an outbound host is an endpoint whether it is written as a name or as an address", u, len(v))
		}
	}

	// The positive control. Without it a classifier that answered "public"
	// to everything would satisfy every assertion above, and the rule
	// would fire on the loopback and LAN addresses this product's own
	// packages are full of.
	local := []string{
		"http://localhost:8080",
		"http://backup-manager:8080",
		"http://tower.local:8080",
		"http://127.0.0.1:8080",
		"http://[::1]:8080",
		"http://192.168.1.20:8080",
		"http://10.7.0.4:8080",
		"http://172.16.9.9:8080",
		"http://100.64.5.9:8080",
		"http://[fd00::1]:8080",
		"http://0.0.0.0:8080",
	}
	for _, u := range local {
		if v := CheckNoMandatoryTelemetry("fixture/compose.yaml", varName+u+"\n"); len(v) > 0 {
			t.Errorf("%s produced %s: an address an operator's own network can hold is not a collector", u, oneLine(v))
		}
	}

	// And the classifier on its own, since the parity between a name and
	// an address is the actual claim and the URL scan is only where it
	// shows up.
	for host, want := range map[string]bool{
		"collector.example-vendor.invalid": true,
		"203.0.113.9":                      true,
		"8.8.8.8":                          true,
		"2001:db8::1":                      true,
		"[2001:db8::1]":                    true,
		"::ffff:203.0.113.9":               true,
		"localhost":                        false,
		"backup-manager":                   false,
		"tower.local":                      false,
		"127.0.0.1":                        false,
		"::1":                              false,
		"192.168.1.20":                     false,
		"100.64.5.9":                       false,
		"fd00::1":                          false,
		"fe80::1":                          false,
		"224.0.0.1":                        false,
		"::ffff:127.0.0.1":                 false,
	} {
		if got := isPublicHost(host); got != want {
			t.Errorf("isPublicHost(%q) = %v, want %v", host, got, want)
		}
	}
}
