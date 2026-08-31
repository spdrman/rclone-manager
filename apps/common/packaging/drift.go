package packaging

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// The adapter conformance drift gate (issue #90, folding in EPIC B's
// Phase 6 / #81).
//
// The refactor's claim is that "a new NAS should cost metadata, not an
// implementation". That claim is either measurable or rhetorical, and
// what makes it measurable is a gate that fails the build when an
// adapter's own metadata stops agreeing with the canonical runtime
// contract on any of the eight things the contract actually decides: the
// image reference, the required mounts, the expected ports, the health
// check, the runtime profile, the declared architecture support, the
// forbidden-privilege set, and compatibility with the /api/v1 surface.
//
// # Four of these already had a check, and none of them is reimplemented
//
// The image reference, the mounts as far as persistence goes, the ports
// and the architecture claim are already decided, per provider, by the
// cross-provider conformance matrix (#86). Writing a second check for
// each would mean two implementations of one rule, drifting apart in
// exactly the way this gate exists to catch. So those four elements
// resolve by consuming the matrix's own verdict for that provider: the
// drift gate fails when the matrix says FAIL, and reports the matrix's
// own detail. The four elements nothing decided yet, the health check,
// the runtime profile, the forbidden-privilege set and API
// compatibility, are implemented here.
//
// # Why every one of these is a function over a Service
//
// Because an adapter registers rather than being coded for. A rule that
// takes a Service can be run against a provider this file has never
// heard of, and against a deliberately broken Service in a test, and the
// second of those is the only way to know the first one works.

// ---------------------------------------------------------------------
// The health check
// ---------------------------------------------------------------------

// CheckHealthCheck holds one service to the canonical health command.
//
// There are exactly three legitimate answers, and the rule is that a
// service has to be giving one of them rather than something adjacent.
// It can override the image's baked-in HEALTHCHECK with the canonical
// web healthcheck command; it can inherit the image's own; or it can
// disable the check outright, which only Unraid does and only because a
// --health-cmd there would run through a shell the distroless runtime
// image does not contain.
//
// What it may not do is declare a health command of its own invention. A
// health check that runs something the image does not ship reports
// unhealthy forever, and a health check that runs something weaker than
// the canonical one (a TCP probe, a `true`) reports healthy through the
// exact failure the check exists to catch. Both are silent, which is why
// this is a gate rather than a review note.
func CheckHealthCheck(svc Service, c Canonical) []Violation {
	source := svc.Source
	if source == "" {
		source = svc.Name
	}
	var out []Violation
	add := func(detail string) { out = append(out, Violation{source, RuleContractDrift, detail}) }

	if svc.HealthcheckDisabled {
		return nil
	}
	if len(svc.HealthcheckTest) == 0 {
		// Inheriting the image's own HEALTHCHECK. Legitimate, and only
		// for a service that actually has what that command needs: the
		// baked-in check is `/backup-manager status`, which reads the
		// config file and the state database.
		if !mountsRole(svc, "state") && !mountsRole(svc, "config") {
			add(fmt.Sprintf("service %s declares no healthcheck, so it inherits the image's own `%s`, which reads the config file and the state database; this service mounts neither, so the check can only ever report unhealthy",
				backquote(svc.Name), strings.Join(c.Commands.Healthcheck, " ")))
		}
		return out
	}

	want := append([]string{"CMD"}, c.Commands.Healthcheck...)
	got := svc.HealthcheckTest
	if !equalStrings(got, want) {
		add(fmt.Sprintf("service %s declares healthcheck %v, and the canonical contract's is %v; a health command the canonical image does not ship reports unhealthy forever, and one weaker than the canonical command reports healthy through the failure it exists to catch",
			backquote(svc.Name), got, want))
	}
	return out
}

func mountsRole(svc Service, role string) bool {
	for _, m := range svc.Mounts {
		if m.Role == role {
			return true
		}
	}
	return false
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

// ---------------------------------------------------------------------
// The runtime profile
// ---------------------------------------------------------------------

// CheckRuntimeProfile holds one service to the hardening every profile in
// this repository expresses: a read-only rootfs, a dropped capability
// set, no-new-privileges, a writable tmpfs for Go's temp directory, and a
// non-root user.
//
// One rule, two metadata formats. A compose service states all five as
// first-class keys; an Unraid template has no schema element for any of
// them and states them as docker run flags in <ExtraParams>. Rather than
// two rules that agree today and drift next quarter, this delegates to
// CheckExtraParamsHardening for the flag form, which is the same set of
// requirements already written once in rules.go.
func CheckRuntimeProfile(svc Service) []Violation {
	source := svc.Source
	if source == "" {
		source = svc.Name
	}
	if strings.TrimSpace(svc.ExtraParams) != "" {
		return CheckExtraParamsHardening(source, svc.ExtraParams)
	}

	var out []Violation
	add := func(detail string) { out = append(out, Violation{source, RuleMissingHardening, detail}) }

	if !svc.ReadOnlyRootFS {
		add(fmt.Sprintf("service %s does not set `read_only: true`, so the reviewed image's own filesystem is writable at runtime", backquote(svc.Name)))
	}
	if !containsFold(svc.CapDrop, "ALL") {
		add(fmt.Sprintf("service %s does not declare `cap_drop: [ALL]`", backquote(svc.Name)))
	}
	if !anyContains(svc.SecurityOpt, "no-new-privileges:true") {
		add(fmt.Sprintf("service %s does not declare `security_opt: [no-new-privileges:true]`", backquote(svc.Name)))
	}
	if len(svc.Tmpfs) == 0 {
		add(fmt.Sprintf("service %s declares no tmpfs, which a read-only rootfs needs for Go's temp directory", backquote(svc.Name)))
	}
	switch user := strings.TrimSpace(svc.User); {
	case user == "":
		add(fmt.Sprintf("service %s pins no user, so it runs as whatever uid the image defaults to", backquote(svc.Name)))
	case user == "0", strings.HasPrefix(user, "0:"), strings.HasPrefix(user, "root"):
		add(fmt.Sprintf("service %s runs as root (`user: %s`)", backquote(svc.Name), user))
	}

	sortViolations(out)
	return out
}

func anyContains(list []string, want string) bool {
	for _, v := range list {
		if strings.Contains(v, want) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------
// The forbidden-privilege set
// ---------------------------------------------------------------------

// CheckForbiddenPrivileges is the negative half of the runtime profile,
// read structurally rather than textually: whatever the profile granted,
// nothing in it may hand back the privileges cap_drop, read_only and a
// non-root user took away.
//
// The distinction from CheckRuntimeProfile is not cosmetic. That one
// fails when a required key is absent; this one fails when a forbidding
// key is present, and a profile can satisfy every requirement and still
// be privileged, because `privileged: true` alongside `cap_drop: ALL` is
// a legal compose file that runs with the host's full capability set.
func CheckForbiddenPrivileges(svc Service) []Violation {
	source := svc.Source
	if source == "" {
		source = svc.Name
	}
	var out []Violation
	add := func(detail string) { out = append(out, Violation{source, RuleForbiddenPrivilege, detail}) }

	if svc.Privileged {
		add(fmt.Sprintf("service %s runs privileged, which grants the host's full capability set and silently overrides every other hardening key beside it", backquote(svc.Name)))
	}
	for _, c := range svc.CapAdd {
		add(fmt.Sprintf("service %s adds capability %s back after dropping ALL", backquote(svc.Name), backquote(strings.TrimSpace(c))))
	}
	if strings.EqualFold(strings.TrimSpace(svc.NetworkMode), "host") {
		add(fmt.Sprintf("service %s joins the host network, which publishes the engine's listener on the LAN whatever the ports declaration says", backquote(svc.Name)))
	}
	if strings.EqualFold(strings.TrimSpace(svc.PIDMode), "host") {
		add(fmt.Sprintf("service %s shares the host PID namespace, so it can see and signal host processes", backquote(svc.Name)))
	}
	for _, d := range svc.Devices {
		add(fmt.Sprintf("service %s passes host device %s through; nothing this image does needs one", backquote(svc.Name), backquote(strings.TrimSpace(d))))
	}
	for _, o := range svc.SecurityOpt {
		if unconfinedRe.MatchString(o) || noNewPrivFalseRe.MatchString(o) {
			add(fmt.Sprintf("service %s sets security option %s, which removes mediation the store's review assumes is on", backquote(svc.Name), backquote(strings.TrimSpace(o))))
		}
	}
	// The Unraid form. <ExtraParams> is the only seam that template has
	// for a run flag, so a privilege granted there is granted in a string
	// none of the fields above model.
	if strings.TrimSpace(svc.ExtraParams) != "" {
		for _, v := range CheckExtraParamsHardening(source, svc.ExtraParams) {
			if v.Rule == RuleUnsafeRunFlag {
				out = append(out, Violation{source, RuleForbiddenPrivilege, v.Detail})
			}
		}
	}

	sortViolations(out)
	return out
}

// ---------------------------------------------------------------------
// The required mounts
// ---------------------------------------------------------------------

// CheckRequiredMounts holds one service's storage declaration to
// canonical.json: every role the canonical image expects is mounted at
// the container path the binaries themselves default to, and the three
// roles the contract marks read-only are mounted read-only.
//
// The read-only half is the part no existing check covers and the part a
// store reviewer would care about: config.yaml, the SFTP private key and
// the pinned known_hosts are mounted `:ro` in every profile precisely so
// a compromised process cannot rewrite the host key it is supposed to be
// pinned to. A profile that quietly drops the `:ro` still passes every
// persistence check ever written, because the path is still there.
//
// A service that mounts nothing at all is not held to this: the Web UI
// container deliberately mounts nothing, which is what makes it a smaller
// surface than the engine by construction rather than by discipline.
func CheckRequiredMounts(svc Service, c Canonical) []Violation {
	source := svc.Source
	if source == "" {
		source = svc.Name
	}
	if len(svc.Mounts) == 0 {
		return nil
	}

	var out []Violation
	add := func(detail string) { out = append(out, Violation{source, RuleContractDrift, detail}) }

	byRole := map[string]Mount{}
	for _, m := range svc.Mounts {
		if m.Role == "" {
			add(fmt.Sprintf("service %s mounts %s, which is not a container path the canonical image knows about; a mount the binaries never read is either a typo that silently loses data or a path nobody reviewed",
				backquote(svc.Name), backquote(m.ContainerPath)))
			continue
		}
		byRole[m.Role] = m
	}

	readOnly := map[string]bool{}
	for _, p := range c.ReadOnlyContainerPaths {
		readOnly[p] = true
	}

	for _, role := range Roles {
		m, ok := byRole[role]
		if !ok {
			want, _ := c.ContainerPaths.ByRole(role)
			add(fmt.Sprintf("service %s mounts five roles' worth of storage but nothing for %s, whose container path %s the binaries default to; the process starts and writes that role onto the container's own filesystem, where it is lost on the next upgrade",
				backquote(svc.Name), backquote(role), backquote(want)))
			continue
		}
		if readOnly[m.ContainerPath] && !m.ReadOnly {
			add(fmt.Sprintf("service %s mounts %s writable, and canonical.json declares it read-only; a writable known_hosts or private key is one compromised process away from being repinned to an attacker's host",
				backquote(svc.Name), backquote(m.ContainerPath)))
		}
		if !readOnly[m.ContainerPath] && m.ReadOnly {
			add(fmt.Sprintf("service %s mounts %s read-only, and the canonical contract needs it writable", backquote(svc.Name), backquote(m.ContainerPath)))
		}
	}

	sortViolations(out)
	return out
}

// ---------------------------------------------------------------------
// API compatibility
// ---------------------------------------------------------------------

// APIBasePath is the versioned API prefix every part of this product has
// to agree on (#166's contract, §17).
const APIBasePath = "/api/v1"

// apiContractAnchors are the three places the base path is written down,
// and every one of them decides whether an installed adapter works. The
// engine registers its routes under it, the Web UI host proxies exactly
// that prefix to the engine, and the shared browser client builds its
// URLs from it. Any two of them agreeing while the third moved is a
// deployment where the UI loads and every request 404s.
var apiContractAnchors = []struct {
	path string
	re   *regexp.Regexp
}{
	{filepath.Join("apps", "common", "webhost", "router.go"), regexp.MustCompile(`r\.Route\("(/api/v\d+)"`)},
	{filepath.Join("apps", "common", "webhost", "serve", "ui.go"), regexp.MustCompile(`mux\.Handle\("(/api/v\d+)/"`)},
	{filepath.Join("ui", "shared", "src", "api", "client.ts"), regexp.MustCompile(`const BASE = "(/api/v\d+)"`)},
}

// APIContractBase reads the base path out of each of the three anchors
// and reports it, or says which one disagrees.
//
// A regular expression over source is crude, and it is crude in the safe
// direction: an anchor whose pattern matches nothing is an error naming
// the file, never a silent skip, so the check breaks loudly when someone
// restructures a file rather than quietly stopping.
func APIContractBase() (string, error) {
	base := ""
	for _, anchor := range apiContractAnchors {
		data, err := os.ReadFile(Path(anchor.path))
		if err != nil {
			return "", err
		}
		m := anchor.re.FindSubmatch(data)
		if m == nil {
			return "", fmt.Errorf("%s no longer states the API base path in a form this check can read; the extractor needs updating, not deleting", anchor.path)
		}
		got := string(m[1])
		if base == "" {
			base = got
			continue
		}
		if got != base {
			return "", fmt.Errorf("%s serves %s and an earlier anchor serves %s: the UI would load and every API request would 404", anchor.path, got, base)
		}
	}
	return base, nil
}

// CheckAPICompatibility holds one service to the /api/v1 contract as far
// as an adapter can move it.
//
// An adapter cannot rename a route. What it can do, and what every one of
// these formats gives it a way to do, is publish the engine's port
// straight to the LAN, bind the listener somewhere the proxy is not
// looking, point the Web UI host's upstream at the wrong place, or
// override an API prefix through an environment variable. All four
// produce an app that installs cleanly from the store and does not work,
// which is the failure mode a reviewer finds and the operator reports.
func CheckAPICompatibility(svc Service, c Canonical, base string) []Violation {
	source := svc.Source
	if source == "" {
		source = svc.Name
	}
	var out []Violation
	add := func(detail string) { out = append(out, Violation{source, RuleContractDrift, detail}) }

	port := fmt.Sprintf("%d", c.ListenPort)

	if addr, ok := svc.Environment["LISTEN_ADDR"]; ok {
		if _, got, found := strings.Cut(strings.TrimSpace(addr), ":"); !found || got != port {
			add(fmt.Sprintf("service %s binds %s, and the canonical contract's listen port is %s; the proxy in front of it looks for %s",
				backquote(svc.Name), backquote(addr), backquote(port), backquote(":"+port)))
		}
	}
	if up, ok := svc.Environment["UPSTREAM_ADDR"]; ok {
		if !strings.HasSuffix(strings.TrimRight(strings.TrimSpace(up), "/"), ":"+port) {
			add(fmt.Sprintf("service %s proxies to %s, which is not the canonical listen port %s, so every %s request reaches nothing",
				backquote(svc.Name), backquote(up), backquote(port), backquote(base)))
		}
	}
	for _, key := range []string{"API_BASE", "API_BASE_PATH", "API_PREFIX", "BASE_PATH"} {
		if v, ok := svc.Environment[key]; ok && strings.TrimSpace(v) != base {
			add(fmt.Sprintf("service %s sets %s to %s, and the shipped browser client builds every URL from %s",
				backquote(svc.Name), backquote(key), backquote(v), backquote(base)))
		}
	}
	for _, p := range svc.Ports {
		parts := strings.Split(p, ":")
		container := parts[len(parts)-1]
		if strings.TrimSpace(container) == "" {
			continue
		}
		if container != port {
			add(fmt.Sprintf("service %s publishes container port %s, and the canonical contract listens on %s",
				backquote(svc.Name), backquote(container), backquote(port)))
		}
	}

	sortViolations(out)
	return out
}

// ---------------------------------------------------------------------
// The eight elements, as one registry
// ---------------------------------------------------------------------

// DriftElement is one thing the drift gate compares against the canonical
// runtime contract.
//
// Whether an element is decided here or by consuming the cross-provider
// matrix's own verdict is a property of the element, recorded once, in
// this list. That is what stops the gate growing a per-platform branch:
// an adapter registers a column, and every element in this table is run
// against it without the adapter naming any of them.
type DriftElement struct {
	// Capability is the preflight capability id this element decides.
	Capability string
	// MatrixCapability is the cross-provider conformance capability whose
	// verdict already answers this element, or empty when the element is
	// decided by Service below.
	//
	// Reusing the matrix's answer rather than writing a second check is
	// the point: two checks for one rule is the drift this gate exists to
	// catch, applied to the gate itself.
	MatrixCapability string
	// Service is the rule, for an element decided here. Run against every
	// service the adapter declares.
	Service func(svc Service, c Canonical, base string) []Violation
}

// DriftElements is the eight-element contract from #81's adapter
// conformance drift gate, in the order that issue lists them.
var DriftElements = []DriftElement{
	{Capability: "drift-image-reference", MatrixCapability: "canonical-image-parity"},
	{Capability: "drift-required-mounts", Service: func(s Service, c Canonical, _ string) []Violation { return CheckRequiredMounts(s, c) }},
	{Capability: "drift-expected-ports", MatrixCapability: "api-path-isolation"},
	{Capability: "drift-health-check", Service: func(s Service, c Canonical, _ string) []Violation { return CheckHealthCheck(s, c) }},
	{Capability: "drift-runtime-profile", Service: func(s Service, _ Canonical, _ string) []Violation { return CheckRuntimeProfile(s) }},
	{Capability: "drift-architecture-support", MatrixCapability: "architecture-parity"},
	{Capability: "drift-forbidden-privileges", Service: func(s Service, _ Canonical, _ string) []Violation { return CheckForbiddenPrivileges(s) }},
	{Capability: "drift-api-compatibility", Service: CheckAPICompatibility},
}

// DriftElementFor returns the element deciding a capability.
func DriftElementFor(capability string) (DriftElement, bool) {
	for _, e := range DriftElements {
		if e.Capability == capability {
			return e, true
		}
	}
	return DriftElement{}, false
}

// DriftCapabilityIDs returns the eight capability ids, sorted, for a test
// that has to pin the set rather than iterate it.
func DriftCapabilityIDs() []string {
	out := make([]string, 0, len(DriftElements))
	for _, e := range DriftElements {
		out = append(out, e.Capability)
	}
	sort.Strings(out)
	return out
}
