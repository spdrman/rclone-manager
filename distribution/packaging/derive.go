package packaging

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// This file is issue #169's derivation gate: the mechanical difference
// between a platform whose runtime fields are DERIVED from the
// authoritative Compose contract and one that merely happens to agree
// with it today.
//
// Phase 4 shipped five platforms that agree by review. Each of them
// states its own image reference, its own mounts, its own port and its
// own health check, and nothing compares those statements to a single
// source. Five independently authored copies of one runtime definition is
// not a conversion risk, it is the definition of drift, and #169 exists
// because agreement by review does not survive the sixth platform.
//
// What "derived" means here, concretely:
//
//   - every field below has ONE authoritative value, in canonical.json or
//     in the canonical Compose definition it pins;
//   - every adapter is checked against that value and a mismatch names
//     the field;
//   - every adapter declares WHICH CONTRACT VERSION it was derived from,
//     so a contract change is a visible edit in every adapter rather than
//     a silent divergence in one.
//
// The last one is the part that makes the other two hold over time. A
// derivation check that only compares values passes forever after a
// contract adds a field nobody applied.

// Drift is one derived field an adapter did not derive. Field is the
// derived-field id, so a failure names the rule that produced it rather
// than only the symptom.
type Drift struct {
	Field   string
	Service string
	Detail  string
	Why     string
}

func (d Drift) String() string {
	if d.Service == "" {
		return fmt.Sprintf("%s: %s", d.Field, d.Detail)
	}
	return fmt.Sprintf("%s: service %q: %s", d.Field, d.Service, d.Detail)
}

// The derived-field ids.
const (
	// FieldContractVersion: the adapter declares the contract version it
	// was derived from, and it is the current one.
	FieldContractVersion = "contract-version"
	// FieldImageReference: one canonical image, named identically by
	// every service of every adapter.
	FieldImageReference = "image-reference"
	// FieldRuntimeProfile: every service names a runtime profile, and it
	// is one the canonical definition declares.
	FieldRuntimeProfile = "runtime-profile"
	// FieldStorageMounts: the engine mounts exactly the canonical storage
	// roles, and the Web UI mounts none of them.
	FieldStorageMounts = "storage-mounts"
	// FieldPublishedPort: exactly one published port in the whole
	// adapter, on the Web UI role, at the canonical listen port.
	FieldPublishedPort = "published-port"
	// FieldHealthCheck: each role's health check is the canonical one for
	// that role, or the one documented way of not declaring it.
	FieldHealthCheck = "health-check"
	// FieldArchitectures: the adapter makes no architecture claim of its
	// own, or makes exactly the canonical one.
	FieldArchitectures = "supported-architectures"
)

// DerivedFields is every field an adapter derives, in report order, with
// the reason each one is derived rather than restated. It is exported so
// a test can assert the check covers all of them: a derivation gate whose
// field list and whose implementation disagree is a gate with a hole in
// it, and the hole is invisible from either side alone.
var DerivedFields = []struct {
	ID  string
	Why string
}{
	{FieldContractVersion, "a contract change has to be a visible edit in every adapter. Without this the value checks below keep passing after the contract grows a field nobody applied."},
	{FieldImageReference, "one canonical image. A platform that names its own reference is a platform that can ship a different build."},
	{FieldRuntimeProfile, "host-dependent behaviour is selected, not branched to. A deployment that names no profile is one whose platform identity and capability reporting are implicit."},
	{FieldStorageMounts, "the container side of every mount is fixed by the binaries. A platform that mounts somewhere else produces a container that starts and then cannot find its own state."},
	{FieldPublishedPort, "the engine holds the state database and the credentials and must never be on the edge. Exactly one published port, on the Web UI."},
	{FieldHealthCheck, "what healthy means is the runtime's answer, not the platform's, and it is what the start order is built on: the Web UI does not start until the engine reports healthy, so the engine's check has to be a liveness question and not a backup-freshness verdict, which is non-zero on a fresh install by design. The Web UI cannot run the engine's check at all, so an adapter that inherits it reports unhealthy forever."},
	{FieldArchitectures, "the architectures come from the release, once. A per-platform claim is a second answer that can be wrong on its own."},
}

// HealthcheckSeam is how one platform's metadata format expresses (or
// cannot express) a container health check.
type HealthcheckSeam string

const (
	// SeamDeclared: the format has a first-class health check key and the
	// adapter uses it.
	SeamDeclared HealthcheckSeam = "declared"
	// SeamImageInherited: the adapter declares nothing, so the image's
	// own HEALTHCHECK instruction applies.
	//
	// That instruction is `/backup-manager status`, FR-24's
	// backup-freshness verdict, and it is deliberately NOT the canonical
	// engine check any more (issue #206). It is the right default for a
	// plain `docker run` and for the headless `daemon` command, which
	// serves no HTTP and so has no liveness endpoint to ask; it is the
	// wrong thing for anything to WAIT on, because it is non-zero on a
	// fresh install by design. Never correct for the Web UI, which has
	// neither the config file nor the state database it reads.
	SeamImageInherited HealthcheckSeam = "image-inherited"
	// SeamDisabled: the adapter turns the check off. The one legitimate
	// use is Unraid's Web UI container: Unraid's only seam is `docker run
	// --health-cmd`, which is shell form, and the distroless runtime
	// image has no shell, so an override there would be a permanently
	// failing check. Off is honest; broken is not.
	SeamDisabled HealthcheckSeam = "disabled"
)

// AdapterRuntime is one platform's runtime definition, reduced to the two
// roles every profile has, whatever format it was written in.
type AdapterRuntime struct {
	// Platform is the canonical.json platform key.
	Platform string
	// Engine and WebUI are the two services. Both are required: an
	// adapter missing one is not a thin adapter over this runtime.
	Engine *Service
	WebUI  *Service
	// Others are any further services the adapter declares. Reported
	// rather than ignored, because a third container is the shape a fork
	// arrives in.
	Others []Service
}

// ReduceToRoles sorts an adapter's services into the two canonical roles
// by the COMMAND each one runs, never by its name. apps/truenas calls
// them backup-manager/backup-manager-ui and container/compose.yaml calls
// them rclone-manager/web-ui; a check keyed on the name would silently
// stop checking the moment someone renamed one.
func ReduceToRoles(platform string, svcs []Service, c Canonical) (AdapterRuntime, []Drift) {
	out := AdapterRuntime{Platform: platform}
	var drift []Drift

	for i := range svcs {
		svc := svcs[i]
		switch {
		case runsCommand(svc.Command, c.Commands.Engine):
			if out.Engine != nil {
				drift = append(drift, Drift{FieldRuntimeProfile, svc.Name,
					fmt.Sprintf("a second service runs the engine command %v; one adapter declares one engine", c.Commands.Engine),
					"two engines over one state directory is a corruption, not a deployment"})
				continue
			}
			out.Engine = &svc
		case runsCommand(svc.Command, c.Commands.WebUI):
			if out.WebUI != nil {
				drift = append(drift, Drift{FieldRuntimeProfile, svc.Name,
					fmt.Sprintf("a second service runs the Web UI command %v", c.Commands.WebUI), "one edge, not two"})
				continue
			}
			out.WebUI = &svc
		default:
			out.Others = append(out.Others, svc)
		}
	}
	return out, drift
}

// runsCommand reports whether got is want followed only by flags. The
// binary and every positional argument must match exactly; only
// leading-dash arguments may follow, which is what lets `serve
// --profile=truenas` be the engine command and `serve-ui something-else`
// not be.
func runsCommand(got, want []string) bool {
	if len(got) < len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	for _, extra := range got[len(want):] {
		if !strings.HasPrefix(extra, "-") {
			return false
		}
	}
	return true
}

// CheckDerivation holds one adapter to every derived field.
//
// It takes the whole Canonical rather than reaching for MustLoad itself,
// so a test can drive it against a deliberately altered source of truth
// and watch the answer change. A checker that loads its own expectations
// can only ever be tested against the one tree it ships in.
func CheckDerivation(a AdapterRuntime, c Canonical) []Drift {
	var out []Drift
	platform, known := c.Platforms[a.Platform]
	if !known {
		return []Drift{{FieldContractVersion, "", fmt.Sprintf("canonical.json declares no platform %q, so there is no derivation to check", a.Platform), "an adapter nothing derives is an adapter nothing checks"}}
	}

	out = append(out, checkContractVersion(a, platform, c)...)

	if a.Engine == nil {
		out = append(out, Drift{FieldRuntimeProfile, "", fmt.Sprintf("no service runs the canonical engine command %v", c.Commands.Engine), "an adapter without the engine is not this runtime"})
	}
	if a.WebUI == nil {
		out = append(out, Drift{FieldRuntimeProfile, "", fmt.Sprintf("no service runs the canonical Web UI command %v", c.Commands.WebUI), "an adapter without the Web UI publishes the engine or nothing"})
	}
	for _, svc := range a.Others {
		out = append(out, Drift{FieldRuntimeProfile, svc.Name,
			fmt.Sprintf("runs %v, which is neither canonical command", svc.Command),
			"a third container is how a second implementation arrives"})
	}

	for _, svc := range a.services() {
		out = append(out, checkImage(svc, c)...)
		out = append(out, checkProfile(svc, platform, c)...)
	}
	out = append(out, checkMounts(a, c)...)
	out = append(out, checkPort(a, c)...)
	out = append(out, checkHealth(a, c)...)

	sortDrift(out)
	return out
}

func (a AdapterRuntime) services() []*Service {
	var out []*Service
	if a.Engine != nil {
		out = append(out, a.Engine)
	}
	if a.WebUI != nil {
		out = append(out, a.WebUI)
	}
	return out
}

func checkContractVersion(a AdapterRuntime, p Platform, c Canonical) []Drift {
	why := DerivedFields[0].Why
	var out []Drift
	if p.DerivesFrom.Source == "" {
		out = append(out, Drift{FieldContractVersion, "", fmt.Sprintf("platform %q declares no derivesFrom.source", a.Platform), why})
	}
	if p.DerivesFrom.Contract == "" {
		out = append(out, Drift{FieldContractVersion, "", fmt.Sprintf("platform %q declares no derivesFrom.contract", a.Platform), why})
		return out
	}
	if p.DerivesFrom.Contract != c.RuntimeContract {
		out = append(out, Drift{FieldContractVersion, "",
			fmt.Sprintf("platform %q was derived from runtime contract %s, and the contract is now %s; re-derive it and say so here", a.Platform, p.DerivesFrom.Contract, c.RuntimeContract), why})
	}
	return out
}

func checkImage(svc *Service, c Canonical) []Drift {
	if svc.Image == c.Image.Reference {
		return nil
	}
	return []Drift{{FieldImageReference, svc.Name,
		fmt.Sprintf("uses image %q, and the canonical reference is %q", svc.Image, c.Image.Reference),
		DerivedFields[1].Why}}
}

// profileArg is the `--profile=<name>` the runtime contract standardises
// as part of "command and runtime profile".
const profileArg = "--profile="

func checkProfile(svc *Service, p Platform, c Canonical) []Drift {
	why := DerivedFields[2].Why
	var got string
	found := false
	for _, arg := range svc.Command {
		if strings.HasPrefix(arg, profileArg) {
			got = strings.TrimPrefix(arg, profileArg)
			found = true
		}
	}
	if !found {
		return []Drift{{FieldRuntimeProfile, svc.Name,
			fmt.Sprintf("runs %v, which never names a runtime profile", svc.Command), why}}
	}
	if !contains(c.Profiles, got) {
		return []Drift{{FieldRuntimeProfile, svc.Name,
			fmt.Sprintf("selects profile %q, which the canonical definition does not declare (it declares %v)", got, c.Profiles), why}}
	}
	if p.Profile != "" && got != p.Profile {
		return []Drift{{FieldRuntimeProfile, svc.Name,
			fmt.Sprintf("selects profile %q, and canonical.json gives this platform %q", got, p.Profile), why}}
	}
	return nil
}

func checkMounts(a AdapterRuntime, c Canonical) []Drift {
	why := DerivedFields[3].Why
	var out []Drift

	if a.Engine != nil {
		want := map[string]bool{}
		for _, role := range Roles {
			p, _ := c.ContainerPaths.ByRole(role)
			want[p] = true
		}
		got := map[string]bool{}
		for _, m := range a.Engine.Mounts {
			got[m.ContainerPath] = true
			if !want[m.ContainerPath] {
				out = append(out, Drift{FieldStorageMounts, a.Engine.Name,
					fmt.Sprintf("mounts %s, which is not a container path the canonical runtime declares", m.ContainerPath), why})
			}
		}
		for _, role := range Roles {
			p, _ := c.ContainerPaths.ByRole(role)
			if !got[p] {
				out = append(out, Drift{FieldStorageMounts, a.Engine.Name,
					fmt.Sprintf("mounts nothing at %s, the canonical %q role", p, role), why})
			}
		}
	}

	if a.WebUI != nil {
		for _, m := range a.WebUI.Mounts {
			out = append(out, Drift{FieldStorageMounts, a.WebUI.Name,
				fmt.Sprintf("mounts %s; the Web UI role mounts nothing at all in the canonical definition, and every mount it gains is attack surface on the one container that faces the LAN", m.ContainerPath), why})
		}
	}

	return out
}

func checkPort(a AdapterRuntime, c Canonical) []Drift {
	why := DerivedFields[4].Why
	var out []Drift
	want := strconv.Itoa(c.ListenPort)

	if a.Engine != nil && len(a.Engine.Ports) > 0 {
		out = append(out, Drift{FieldPublishedPort, a.Engine.Name,
			fmt.Sprintf("publishes %v; the engine holds the state database and the credentials and publishes nothing", a.Engine.Ports), why})
	}
	if a.WebUI == nil {
		return out
	}
	if len(a.WebUI.Ports) != 1 {
		out = append(out, Drift{FieldPublishedPort, a.WebUI.Name,
			fmt.Sprintf("publishes %v, want exactly one port", a.WebUI.Ports), why})
		return out
	}
	spec := a.WebUI.Ports[0]
	parts := strings.Split(spec, ":")
	container := parts[len(parts)-1]
	if container != want {
		out = append(out, Drift{FieldPublishedPort, a.WebUI.Name,
			fmt.Sprintf("publishes %q, whose container side is %s and the canonical listen port is %s", spec, container, want), why})
	}
	return out
}

// SeamOf reads which health-check seam a service actually used.
func SeamOf(svc *Service) HealthcheckSeam {
	switch {
	case svc.HealthcheckDisabled:
		return SeamDisabled
	case len(svc.HealthcheckTest) > 0:
		return SeamDeclared
	default:
		return SeamImageInherited
	}
}

func checkHealth(a AdapterRuntime, c Canonical) []Drift {
	why := DerivedFields[5].Why
	var out []Drift

	if a.Engine != nil {
		switch SeamOf(a.Engine) {
		case SeamDeclared:
			if !sameTest(a.Engine.HealthcheckTest, c.Healthchecks.Engine) {
				out = append(out, Drift{FieldHealthCheck, a.Engine.Name,
					fmt.Sprintf("declares health check %v, and the canonical engine check is %v", a.Engine.HealthcheckTest, c.Healthchecks.Engine), why})
			}
		case SeamDisabled:
			out = append(out, Drift{FieldHealthCheck, a.Engine.Name,
				"disables the health check; the engine's health is the start gate every other container in the deployment waits on, and a disabled check reports nothing for them to wait for", why})
		case SeamImageInherited:
			// The image's baked-in HEALTHCHECK applies, and it is the
			// backup-freshness verdict rather than the canonical start
			// gate (see SeamImageInherited's own doc). That is a report
			// where nothing waits on it, and issue #206 where something
			// does: a fresh install has backed nothing up, so the
			// verdict is non-zero, so the gate never releases and the
			// only LAN-facing container never starts.
			//
			// Unraid is the adapter this leaves standing, and it stands
			// on the rule rather than on an exemption: its template
			// schema has no health-check seam at all, and it also
			// declares no start-ordering dependency, so nothing there
			// waits on the verdict and the badge it produces is exactly
			// the freshness report FR-24 means it to be.
			if waiters := waitingOnHealthOf(a, a.Engine.Name); len(waiters) > 0 {
				out = append(out, Drift{FieldHealthCheck, a.Engine.Name,
					fmt.Sprintf("declares no health check, so it inherits the image's own %v, and %v will not start until that reports healthy; it is the backup-freshness verdict and exits non-zero on a fresh install by design, so the gate never releases. The canonical engine check is %v",
						c.Commands.ImageHealthcheck, waiters, c.Healthchecks.Engine), why})
			}
		}
	}

	if a.WebUI != nil {
		switch SeamOf(a.WebUI) {
		case SeamDeclared:
			if !sameTest(a.WebUI.HealthcheckTest, c.Healthchecks.WebUI) {
				out = append(out, Drift{FieldHealthCheck, a.WebUI.Name,
					fmt.Sprintf("declares health check %v, and the canonical Web UI check is %v", a.WebUI.HealthcheckTest, c.Healthchecks.WebUI), why})
			}
		case SeamDisabled:
			// Legitimate, and only where the format has no usable seam.
			// See SeamDisabled's own doc.
		case SeamImageInherited:
			out = append(out, Drift{FieldHealthCheck, a.WebUI.Name,
				fmt.Sprintf("declares no health check, so it inherits the image's %v, which needs a config file and a state database this container does not have; it would report unhealthy forever", c.Healthchecks.Engine), why})
		}
	}

	return out
}

// waitingOnHealthOf names every service in this adapter that refuses to
// start until service reports healthy.
//
// Sorted and complete rather than a boolean: a drift message that says
// WHICH container is being held back is the difference between a rule an
// operator can act on and one they have to go and reproduce.
func waitingOnHealthOf(a AdapterRuntime, service string) []string {
	var out []string
	for _, svc := range a.services() {
		for _, dep := range svc.WaitsForHealthy {
			if dep == service {
				out = append(out, svc.Name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// sameTest compares two compose healthcheck test vectors, tolerating the
// CMD prefix being present on one side only, because that is a spelling
// and not a difference in what runs.
func sameTest(got, want []string) bool {
	return strings.Join(stripCMD(got), " ") == strings.Join(stripCMD(want), " ")
}

func stripCMD(test []string) []string {
	if len(test) > 0 && (test[0] == "CMD" || test[0] == "CMD-SHELL") {
		return test[1:]
	}
	return test
}

// CheckArchitectureDerivation is FieldArchitectures, and it is separate
// because it reads a provider's conformance declaration rather than its
// compose services.
//
// An adapter over a multi-arch image by reference makes no architecture
// claim of its own: the release does, once. A claim here is not
// forbidden, it just has to be the same claim, because a second answer is
// a second thing that can be wrong.
func CheckArchitectureDerivation(platform string, claimed []string, c Canonical) []Drift {
	why := DerivedFields[6].Why
	if len(claimed) == 0 {
		return nil
	}
	got := append([]string(nil), claimed...)
	want := append([]string(nil), c.Architectures...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		return []Drift{{FieldArchitectures, "",
			fmt.Sprintf("platform %q claims architectures %v, and the release builds %v", platform, got, want), why}}
	}
	return nil
}

func sortDrift(d []Drift) {
	sort.Slice(d, func(i, j int) bool {
		if d[i].Field != d[j].Field {
			return d[i].Field < d[j].Field
		}
		if d[i].Service != d[j].Service {
			return d[i].Service < d[j].Service
		}
		return d[i].Detail < d[j].Detail
	})
}

// FormatDrift renders drift for a test failure.
func FormatDrift(d []Drift) string {
	if len(d) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(d))
	for _, x := range d {
		parts = append(parts, "  - "+x.String()+"\n    why it is derived: "+x.Why)
	}
	return strings.Join(parts, "\n")
}
