package packaging

import (
	"strings"
	"testing"
)

// This file is issue #169's RED evidence and its gate in one place: every
// converted adapter derives its runtime fields from the authoritative
// contract, and a deliberately introduced mismatch fails and names the
// field that drifted.
//
// The mismatch half is not decoration. #169 asks for the derivation check
// to be demonstrated "against a deliberately introduced mismatch, not
// just against a missing adapter", because a check that only ever runs
// against a correct tree is indistinguishable from a check that returns
// nil. Three preflight rules in this repository have already failed open.

func canonicalForTest(t *testing.T) Canonical {
	t.Helper()
	return MustLoad()
}

// adapterRuntimes reduces every deployable definition a platform ships to
// the two canonical roles. Per artifact, not per platform: TrueNAS ships
// a paste-in compose file AND a catalog entry, and flattening them would
// make one adapter look like it declares two engines.
func adapterRuntimes(t *testing.T, p platformFixture, c Canonical) []struct {
	name string
	rt   AdapterRuntime
} {
	t.Helper()
	if p.runtimeArtifacts == nil {
		t.Fatalf("%s declares no runtimeArtifacts, so the derivation gate would silently skip it", p.name)
	}
	arts := p.runtimeArtifacts(t)
	if len(arts) == 0 {
		t.Fatalf("%s produced no runtime artifact, so this platform would pass by checking nothing", p.name)
	}
	var out []struct {
		name string
		rt   AdapterRuntime
	}
	for _, art := range arts {
		a, drift := ReduceToRoles(p.name, art.svcs, c)
		if len(drift) > 0 {
			t.Fatalf("%s (%s): reducing to roles already found drift:\n%s", p.name, art.name, FormatDrift(drift))
		}
		out = append(out, struct {
			name string
			rt   AdapterRuntime
		}{art.name, a})
	}
	return out
}

// TestEveryAdapterDerivesItsRuntimeFieldsFromTheCanonicalContract is
// #169's central claim, stated so it can fail: five platforms, one
// runtime definition, no independently authored copies.
func TestEveryAdapterDerivesItsRuntimeFieldsFromTheCanonicalContract(t *testing.T) {
	c := canonicalForTest(t)

	for _, p := range allPlatforms() {
		for _, art := range adapterRuntimes(t, p, c) {
			t.Run(p.name+"/"+art.name, func(t *testing.T) {
				if d := CheckDerivation(art.rt, c); len(d) > 0 {
					t.Errorf("%s does not derive its runtime fields from the canonical contract:\n%s", art.name, FormatDrift(d))
				}
			})
		}
	}
}

// TestEveryAdapterMakesNoArchitectureClaimOfItsOwn is the same rule for
// the one derived field that lives in the conformance declaration rather
// than in a compose file.
func TestEveryAdapterMakesNoArchitectureClaimOfItsOwn(t *testing.T) {
	c := canonicalForTest(t)
	conf := MustLoadConformance()

	for _, p := range allPlatforms() {
		t.Run(p.name, func(t *testing.T) {
			claim := conf.Providers[p.name].Metadata.ArchitectureClaim
			if d := CheckArchitectureDerivation(p.name, claim.Architectures, c); len(d) > 0 {
				t.Errorf("%s", FormatDrift(d))
			}
		})
	}

	// Positive control: a claim that disagrees with the release has to
	// fail, or the loop above passes for every provider simply because
	// none of them claims anything.
	if d := CheckArchitectureDerivation("control", []string{"amd64", "riscv64"}, c); len(d) == 0 {
		t.Fatalf("an adapter claiming %v passed against a release that builds %v", []string{"amd64", "riscv64"}, c.Architectures)
	}
}

// ---------------------------------------------------------------------
// The positive controls, one mutation per derived field
// ---------------------------------------------------------------------

// derivationMutation deliberately breaks one derived field of a real
// adapter and names what it broke. It mutates the reduced runtime and the
// canonical source together, because some fields (the contract version)
// live on one side and some (the image reference) on the other.
type derivationMutation struct {
	field string
	apply func(a *AdapterRuntime, c *Canonical) string
}

func derivationMutations() []derivationMutation {
	return []derivationMutation{
		{FieldContractVersion, func(a *AdapterRuntime, c *Canonical) string {
			p := c.Platforms[a.Platform]
			p.DerivesFrom.Contract = "0.0.1-stale"
			c.Platforms[a.Platform] = p
			return "pinned the platform's derivesFrom to a stale contract version"
		}},
		{FieldImageReference, func(a *AdapterRuntime, _ *Canonical) string {
			a.Engine.Image = "docker.io/somebody/else:latest"
			return "pointed the engine at somebody else's image"
		}},
		{FieldRuntimeProfile, func(a *AdapterRuntime, _ *Canonical) string {
			a.Engine.Command = withoutProfileArg(a.Engine.Command)
			return "dropped --profile= from the engine command"
		}},
		{FieldStorageMounts, func(a *AdapterRuntime, _ *Canonical) string {
			a.Engine.Mounts[0].ContainerPath = "/somewhere/else"
			return "moved one engine mount to a container path the runtime does not know"
		}},
		{FieldPublishedPort, func(a *AdapterRuntime, _ *Canonical) string {
			a.WebUI.Ports = []string{"8080:9999"}
			return "published the Web UI on a container port the runtime does not listen on"
		}},
		{FieldHealthCheck, func(a *AdapterRuntime, _ *Canonical) string {
			a.WebUI.HealthcheckTest = nil
			a.WebUI.HealthcheckDisabled = false
			return "left the Web UI inheriting the image's engine health check"
		}},
		{FieldHealthCheck, func(a *AdapterRuntime, _ *Canonical) string {
			// Issue #206's shape, which needs BOTH halves to exist
			// before it is a defect: an engine that declares nothing,
			// so it inherits the image's backup-freshness verdict, and
			// something that will not start until that verdict passes.
			// The wait is added here rather than assumed, so this
			// control is the same control on Unraid, whose template
			// format has no dependency gate of its own.
			a.Engine.HealthcheckTest = nil
			a.Engine.HealthcheckDisabled = false
			a.WebUI.WaitsForHealthy = append(a.WebUI.WaitsForHealthy, a.Engine.Name)
			return "left the engine inheriting the image's backup-freshness verdict while the Web UI waits on its health"
		}},
	}
}

// TestTheBackupFreshnessVerdictIsRefusedAsAnEngineStartGate names the
// regression rather than mutating something adjacent to it: the exact
// health check every adapter shipped with, put back, has to be refused.
//
// A generic mutation cannot say this. `backup-manager status` is a real
// command the image really ships and really answers, so nothing about it
// looks wrong from the outside; what is wrong is that a container start
// waits on it, and a fresh install has backed nothing up.
func TestTheBackupFreshnessVerdictIsRefusedAsAnEngineStartGate(t *testing.T) {
	c := canonicalForTest(t)

	for _, p := range allPlatforms() {
		t.Run(p.name, func(t *testing.T) {
			a := adapterRuntimes(t, p, c)[0].rt
			if d := CheckDerivation(a, c); len(d) > 0 {
				t.Fatalf("the unmutated adapter already drifts, so this control would pass for the wrong reason:\n%s", FormatDrift(d))
			}

			a.Engine.HealthcheckTest = []string{"CMD", "/backup-manager", "status"}
			a.Engine.HealthcheckDisabled = false
			d := CheckDerivation(a, c)
			if !namesField(d, FieldHealthCheck) {
				t.Fatalf("declaring `backup-manager status` as the engine's health check produced %s, want a refusal naming %q: it is FR-24's freshness verdict, non-zero on a fresh install, and the Web UI waits on it", FormatDrift(d), FieldHealthCheck)
			}
		})
	}
}

// TestTheEngineMayInheritTheImageCheckOnlyWhereNothingWaitsOnIt is the
// negative assertion's positive control, on one fixture, in both
// directions.
//
// Without the second half the rule reads as "inheriting is fine", which
// is what shipped. Without the first it reads as "inheriting is never
// fine", which would fail Unraid for a seam its template schema does not
// have, and an exemption typed into a rule is how the rule stops being
// one.
func TestTheEngineMayInheritTheImageCheckOnlyWhereNothingWaitsOnIt(t *testing.T) {
	c := canonicalForTest(t)

	var unraid *platformFixture
	for _, p := range allPlatforms() {
		if p.name == "unraid" {
			fixture := p
			unraid = &fixture
		}
	}
	if unraid == nil {
		t.Fatal("no unraid fixture, so the one adapter with no health-check seam is not covered")
	}

	a := adapterRuntimes(t, *unraid, c)[0].rt
	if got := SeamOf(a.Engine); got != SeamImageInherited {
		t.Fatalf("the Unraid engine expresses its health check as %q, want %q; this control is about the adapter that CANNOT declare one, so any other seam means it proves nothing", got, SeamImageInherited)
	}
	if len(a.WebUI.WaitsForHealthy) != 0 {
		t.Fatalf("the Unraid Web UI waits on %v, and an Unraid template has no dependency gate at all; the parse is reading something that is not there", a.WebUI.WaitsForHealthy)
	}
	if d := CheckDerivation(a, c); len(d) > 0 {
		t.Fatalf("inheriting the image's health check drifted on an adapter where nothing waits on it, so the rule is refusing a seam rather than a defect:\n%s", FormatDrift(d))
	}

	a.WebUI.WaitsForHealthy = []string{a.Engine.Name}
	d := CheckDerivation(a, c)
	if !namesField(d, FieldHealthCheck) {
		t.Fatalf("the same inherited check with the Web UI waiting on it produced %s, want a refusal naming %q", FormatDrift(d), FieldHealthCheck)
	}
	if !strings.Contains(FormatDrift(d), a.WebUI.Name) {
		t.Errorf("the refusal %q does not name the container being held back, which is the whole symptom", FormatDrift(d))
	}
}

func withoutProfileArg(argv []string) []string {
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		if strings.HasPrefix(a, "--profile=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// TestTheDerivationCheckFailsOnADeliberateMismatch runs every mutation
// against every adapter. Against every adapter and not just one, because
// the five are read out of four different metadata formats, and a rule
// that fires on a compose file and not on an Unraid template is a rule
// Unraid does not have.
func TestTheDerivationCheckFailsOnADeliberateMismatch(t *testing.T) {
	base := canonicalForTest(t)

	for _, p := range allPlatforms() {
		for _, m := range derivationMutations() {
			t.Run(p.name+"/"+m.field, func(t *testing.T) {
				// A fresh Canonical and a freshly parsed adapter per
				// subtest. One embedded canonical.json is shared mutable
				// state, and a mutation that leaked into the next subtest
				// would make these failures depend on execution order,
				// which is not a control.
				c := cloneCanonical(base)
				arts := adapterRuntimes(t, p, c)
				a := arts[0].rt

				if d := CheckDerivation(a, c); len(d) > 0 {
					t.Fatalf("the unmutated adapter already drifts, so this control would pass for the wrong reason:\n%s", FormatDrift(d))
				}

				what := m.apply(&a, &c)
				d := CheckDerivation(a, c)
				if len(d) == 0 {
					t.Fatalf("%s and the derivation check still passed, so %q is not actually derived", what, m.field)
				}
				if !namesField(d, m.field) {
					t.Errorf("%s produced drift that never names %q, so the failure does not say which field drifted:\n%s", what, m.field, FormatDrift(d))
				}
			})
		}
	}
}

// TestEveryDerivedFieldHasAMutation pins the field list to the controls
// in both directions. One direction stops a field being declared with no
// control behind it; the other stops a control existing for a field the
// gate does not have.
func TestEveryDerivedFieldHasAMutation(t *testing.T) {
	controlled := map[string]bool{}
	for _, m := range derivationMutations() {
		controlled[m.field] = true
	}
	// Architectures has its own control in
	// TestEveryAdapterMakesNoArchitectureClaimOfItsOwn, because it is
	// checked against the conformance declaration rather than against a
	// reduced runtime.
	controlled[FieldArchitectures] = true

	declared := map[string]bool{}
	for _, f := range DerivedFields {
		declared[f.ID] = true
		if !controlled[f.ID] {
			t.Errorf("derived field %q has no positive control, so nobody has watched it fail", f.ID)
		}
		if f.Why == "" {
			t.Errorf("derived field %q says nothing about why it is derived rather than restated", f.ID)
		}
	}
	for id := range controlled {
		if !declared[id] {
			t.Errorf("there is a control for %q, which DerivedFields does not declare", id)
		}
	}
	if len(DerivedFields) == 0 {
		t.Fatal("no derived fields are declared at all, so the gate checks nothing")
	}
}

// TestAThirdContainerIsRefused is the shape a fork actually arrives in:
// not a changed field, an added service. #169's whole point is that a
// platform integration is metadata, and a platform that needs a process
// of its own has stopped being an adapter.
func TestAThirdContainerIsRefused(t *testing.T) {
	c := canonicalForTest(t)
	p := allPlatforms()[0]
	a := adapterRuntimes(t, p, c)[0].rt

	a.Others = append(a.Others, Service{Name: "backup-manager-sidecar", Command: []string{"/usr/bin/some-agent"}})
	d := CheckDerivation(a, c)
	if !namesField(d, FieldRuntimeProfile) {
		t.Errorf("a third container produced %s, want a refusal naming %q", FormatDrift(d), FieldRuntimeProfile)
	}
}

// cloneCanonical is a deep-enough copy for the mutations above: the maps
// and slices a mutation reaches into.
//
// Shared mutable state is not a control. These subtests run against one
// embedded canonical.json, and a mutation that leaked into the next
// subtest would make failures depend on execution order.
func cloneCanonical(c Canonical) Canonical {
	out := c
	out.Platforms = make(map[string]Platform, len(c.Platforms))
	for k, v := range c.Platforms {
		out.Platforms[k] = v
	}
	out.Architectures = append([]string(nil), c.Architectures...)
	out.Profiles = append([]string(nil), c.Profiles...)
	out.ReadOnlyContainerPaths = append([]string(nil), c.ReadOnlyContainerPaths...)
	out.WritableContainerPaths = append([]string(nil), c.WritableContainerPaths...)
	return out
}

func namesField(d []Drift, field string) bool {
	for _, x := range d {
		if x.Field == field {
			return true
		}
	}
	return false
}
