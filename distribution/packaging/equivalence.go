package packaging

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// This file is issue #170's semantic-equivalence gate, and it exists
// because the derivation gate in derive.go answers a narrower question
// than the four adapters #170 adds need answered.
//
// derive.go holds an adapter to seven fields that have one authority
// each. That is the right rule for a platform whose packaging genuinely
// differs from the canonical runtime: an Unraid Docker template cannot
// declare a compose health check, and a TrueNAS catalog renders its
// mounts out of a questions file. Seven fields is what those formats have
// in common.
//
// The four targets #170 adds are different in kind. Portainer, CasaOS and
// ZimaOS consume Compose directly and Dockge consumes the canonical file
// itself, so for them "derived" is not the strongest claim available:
// their runtime SHOULD be the canonical runtime, service for service,
// with only the store metadata wrapped around it. #170's own contract
// says so twice, once as "the runtime service definition stays
// semantically equivalent to the canonical Compose contract" and once as
// "Dockge uses the canonical Compose stack".
//
// So this compares the whole reduced runtime rather than seven fields.
// What it deliberately does NOT compare is the image reference: the
// canonical definition builds `backup-manager:${VERSION:-dev}` from
// container/Dockerfile because it is also the file that produces the
// image, and an adapter pulls the published reference. That difference is
// real, it is the one thing an adapter is supposed to change here, and
// derive.go already pins the adapter side of it to canonical.json.

// Divergence is one property on which an adapter's runtime is not the
// canonical runtime.
type Divergence struct {
	Property string
	Role     string
	Detail   string
	Why      string
}

func (d Divergence) String() string {
	if d.Role == "" {
		return fmt.Sprintf("%s: %s", d.Property, d.Detail)
	}
	return fmt.Sprintf("%s: %s role: %s", d.Property, d.Role, d.Detail)
}

// The equivalence property ids.
const (
	// PropRoleSet: the adapter declares the same two roles and nothing
	// else.
	PropRoleSet = "role-set"
	// PropCommand: each role runs the canonical command for it, argument
	// for argument, once the runtime profile is set aside.
	PropCommand = "command"
	// PropContainerMounts: each role mounts the same container paths with
	// the same write mode.
	PropContainerMounts = "container-mounts"
	// PropPublishedPort: the same container-side port is published, by
	// the same role, and by no other.
	PropPublishedPort = "published-port"
	// PropHealthCheck: each role's health check is the canonical one for
	// that role.
	PropHealthCheck = "health-check"
	// PropEngineEnvironment: the engine reads the same environment keys.
	PropEngineEnvironment = "engine-environment-keys"
)

// EquivalenceProperties is every property compared, in report order, with
// what a difference in it would actually do. Exported so a test can
// assert the checker covers all of them and that every one has a
// mutation that breaks it: a comparison whose property list and whose
// implementation disagree has a hole that is invisible from either side.
var EquivalenceProperties = []struct {
	ID  string
	Why string
}{
	{PropRoleSet, "two containers from one image, one holding the state database and one facing the network. A third container, or a missing one, is a different product with the same name."},
	{PropCommand, "the two roles differ by argv and by nothing else. An adapter that runs a different subcommand starts cleanly and serves the wrong thing."},
	{PropContainerMounts, "the container side of every mount is fixed by the binaries, and the write mode is a claim about what the application does with it. A mount that moves produces a container that starts and cannot find its own state; a write mode that flips makes the settings, backup-set and first-run write paths inert (issue #196)."},
	{PropPublishedPort, "exactly one published port, on the Web UI role. The engine holds the credentials and the catalogue and must never be on the edge."},
	{PropHealthCheck, "what healthy means is the runtime's answer. The Web UI container cannot run the engine's check at all, so an adapter that inherits it reports unhealthy forever."},
	{PropEngineEnvironment, "the engine's environment is the runtime's own configuration surface: the timezone retention is evaluated in, the listen address, the enrollment link's base URL, and whether forwarded headers are believed. A key that is absent here is a default nobody chose. The Web UI's environment is deliberately NOT compared, because bundle selection is the one thing that legitimately varies between carriers."},
}

// CheckStackEquivalence reports every property on which adapter is not
// the canonical runtime.
//
// Both sides are already reduced to roles by command, never by service
// name, so an adapter that renames its services is still compared against
// the right half of the canonical definition.
func CheckStackEquivalence(adapter, canonical AdapterRuntime) []Divergence {
	var out []Divergence

	out = append(out, equivalentRoleSet(adapter, canonical)...)
	out = append(out, equivalentRole("engine", adapter.Engine, canonical.Engine)...)
	out = append(out, equivalentRole("web-ui", adapter.WebUI, canonical.WebUI)...)
	out = append(out, equivalentEngineEnvironment(adapter, canonical)...)

	sort.Slice(out, func(i, j int) bool {
		if out[i].Property != out[j].Property {
			return out[i].Property < out[j].Property
		}
		if out[i].Role != out[j].Role {
			return out[i].Role < out[j].Role
		}
		return out[i].Detail < out[j].Detail
	})
	return out
}

func whyFor(id string) string {
	for _, p := range EquivalenceProperties {
		if p.ID == id {
			return p.Why
		}
	}
	return ""
}

func equivalentRoleSet(adapter, canonical AdapterRuntime) []Divergence {
	why := whyFor(PropRoleSet)
	var out []Divergence
	if adapter.Engine == nil {
		out = append(out, Divergence{PropRoleSet, "engine", "the adapter declares no service running the engine command", why})
	}
	if adapter.WebUI == nil {
		out = append(out, Divergence{PropRoleSet, "web-ui", "the adapter declares no service running the Web UI command", why})
	}
	for _, svc := range adapter.Others {
		out = append(out, Divergence{PropRoleSet, "", fmt.Sprintf("the adapter declares a third service %q running %v, and the canonical stack declares two", svc.Name, svc.Command), why})
	}
	if canonical.Engine == nil || canonical.WebUI == nil {
		out = append(out, Divergence{PropRoleSet, "", "the canonical stack itself did not reduce to two roles, so there is nothing to compare against and this result means nothing", why})
	}
	return out
}

func equivalentRole(role string, got, want *Service) []Divergence {
	if got == nil || want == nil {
		return nil
	}
	var out []Divergence

	if a, b := withoutProfile(got.Command), withoutProfile(want.Command); !equalStrings(a, b) {
		out = append(out, Divergence{PropCommand, role,
			fmt.Sprintf("runs %v and the canonical stack runs %v (the runtime profile is set aside on both sides, because selecting one is what an adapter is for)", a, b),
			whyFor(PropCommand)})
	}

	out = append(out, equivalentMounts(role, got, want)...)
	out = append(out, equivalentPorts(role, got, want)...)
	out = append(out, equivalentHealth(role, got, want)...)
	return out
}

// withoutProfile drops the `--profile=` argument, which is the one
// argument an adapter is supposed to change.
func withoutProfile(argv []string) []string {
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		if strings.HasPrefix(a, profileArg) {
			continue
		}
		out = append(out, a)
	}
	return out
}

func equivalentMounts(role string, got, want *Service) []Divergence {
	why := whyFor(PropContainerMounts)
	var out []Divergence

	gotMounts := mountModes(got)
	wantMounts := mountModes(want)

	for _, path := range sortedMountPaths(wantMounts) {
		mode, ok := gotMounts[path]
		if !ok {
			out = append(out, Divergence{PropContainerMounts, role,
				fmt.Sprintf("mounts nothing at %s, and the canonical stack mounts it %s", path, wantMounts[path]), why})
			continue
		}
		if mode != wantMounts[path] {
			out = append(out, Divergence{PropContainerMounts, role,
				fmt.Sprintf("mounts %s %s, and the canonical stack mounts it %s", path, mode, wantMounts[path]), why})
		}
	}
	for _, path := range sortedMountPaths(gotMounts) {
		if _, ok := wantMounts[path]; !ok {
			out = append(out, Divergence{PropContainerMounts, role,
				fmt.Sprintf("mounts %s, and the canonical stack's %s role mounts nothing there", path, role), why})
		}
	}
	return out
}

func mountModes(svc *Service) map[string]string {
	out := map[string]string{}
	for _, m := range svc.Mounts {
		mode := "writable"
		if m.ReadOnly {
			mode = "read-only"
		}
		out[m.ContainerPath] = mode
	}
	return out
}

func sortedMountPaths(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func equivalentPorts(role string, got, want *Service) []Divergence {
	why := whyFor(PropPublishedPort)
	g := containerPorts(got)
	w := containerPorts(want)
	if equalStrings(g, w) {
		return nil
	}
	return []Divergence{{PropPublishedPort, role,
		fmt.Sprintf("publishes container port(s) %v and the canonical stack publishes %v (the host side is the operator's and is not compared)", g, w), why}}
}

// containerPorts reduces a published-port spec to its container side,
// sorted. The host side is deliberately dropped: an adapter that lets an
// operator pick a different host port has changed nothing about the
// runtime, and comparing it would make every adapter with a configurable
// port look like a fork.
func containerPorts(svc *Service) []string {
	out := make([]string, 0, len(svc.Ports))
	for _, spec := range svc.Ports {
		parts := strings.Split(spec, ":")
		out = append(out, parts[len(parts)-1])
	}
	sort.Strings(out)
	return out
}

func equivalentHealth(role string, got, want *Service) []Divergence {
	why := whyFor(PropHealthCheck)
	gotSeam, wantSeam := SeamOf(got), SeamOf(want)
	if gotSeam != wantSeam {
		return []Divergence{{PropHealthCheck, role,
			fmt.Sprintf("expresses its health check as %q and the canonical stack expresses it as %q", gotSeam, wantSeam), why}}
	}
	if gotSeam != SeamDeclared {
		return nil
	}
	if !sameTest(got.HealthcheckTest, want.HealthcheckTest) {
		return []Divergence{{PropHealthCheck, role,
			fmt.Sprintf("declares health check %v and the canonical stack declares %v", got.HealthcheckTest, want.HealthcheckTest), why}}
	}
	return nil
}

func equivalentEngineEnvironment(adapter, canonical AdapterRuntime) []Divergence {
	if adapter.Engine == nil || canonical.Engine == nil {
		return nil
	}
	why := whyFor(PropEngineEnvironment)
	got := sortedEnvKeys(adapter.Engine.Environment)
	want := sortedEnvKeys(canonical.Engine.Environment)
	if equalStrings(got, want) {
		return nil
	}
	var missing, extra []string
	for _, k := range want {
		if _, ok := adapter.Engine.Environment[k]; !ok {
			missing = append(missing, k)
		}
	}
	for _, k := range got {
		if _, ok := canonical.Engine.Environment[k]; !ok {
			extra = append(extra, k)
		}
	}
	detail := fmt.Sprintf("reads %v and the canonical engine reads %v", got, want)
	if len(missing) > 0 {
		detail += fmt.Sprintf("; missing %v", missing)
	}
	if len(extra) > 0 {
		detail += fmt.Sprintf("; extra %v", extra)
	}
	return []Divergence{{PropEngineEnvironment, "engine", detail, why}}
}

func sortedEnvKeys(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k := range env {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// CanonicalListenPort is the canonical listen port as a string, for the
// checks that compare it against a store metadata field written as text.
func CanonicalListenPort(c Canonical) string { return strconv.Itoa(c.ListenPort) }

// FormatDivergence renders divergences for a test failure.
func FormatDivergence(d []Divergence) string {
	if len(d) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(d))
	for _, x := range d {
		parts = append(parts, "  - "+x.String()+"\n    why it has to be the same: "+x.Why)
	}
	return strings.Join(parts, "\n")
}
