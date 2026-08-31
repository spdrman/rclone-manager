package packaging

import (
	"fmt"
	"strings"
)

// This file holds the packaging rules that need to be callable from
// somewhere other than one assertion, because every one of them is a
// negative claim ("this flag is not passed", "this variable is not set on
// the edge container") and a negative claim needs a positive control
// proving it can fail. A rule expressed as a function over a Service can
// be pointed at a deliberately broken Service in scan_test.go; a rule
// expressed as five strings.Contains calls inside a test cannot.

const (
	// RuleUnsafeRunFlag is a docker run flag that undoes the hardening the
	// same command line asks for.
	RuleUnsafeRunFlag = "unsafe-run-flag"
	// RuleMissingHardening is one of the hardening flags every profile
	// must pass, absent.
	RuleMissingHardening = "missing-hardening"
	// RuleForwardedHeaderTrust covers TRUST_FORWARDED_HEADERS being set
	// where the network topology does not justify it, or missing where
	// the profile's own documentation says it is set.
	RuleForwardedHeaderTrust = "forwarded-header-trust"
)

// RunFlag is one flag parsed out of an Unraid <ExtraParams> string.
type RunFlag struct {
	Name  string
	Value string
}

// ParseRunFlags splits an Unraid <ExtraParams> string into flags, handling
// both `--flag=value` and `--flag value`.
//
// Searching the raw string for substrings instead is what makes the Unraid
// hardening check fail open: `--user` is satisfied by `--userns=host`, and
// a `--privileged` appended anywhere in the same string is matched by
// nothing at all, because the template scanner reads the <Privileged>
// element rather than this one.
func ParseRunFlags(s string) []RunFlag {
	fields := strings.Fields(s)
	var out []RunFlag
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if !strings.HasPrefix(f, "-") {
			continue
		}
		name, value, hasEq := strings.Cut(f, "=")
		if !hasEq && i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "-") {
			value = fields[i+1]
			i++
		}
		out = append(out, RunFlag{Name: name, Value: value})
	}
	return out
}

// deniedRunFlags are flags that undo the hardening ExtraParams exists to
// express. Each entry is a flag name and, where the flag is legitimate in
// some forms, the value substring that makes it a violation.
var deniedRunFlags = []struct {
	name    string
	value   string
	because string
}{
	{name: "--privileged", because: "gives the container the host's full capability set, which is every hardening flag on the same line undone"},
	{name: "--cap-add", because: "adds back a capability the same command line dropped with --cap-drop=ALL"},
	{name: "--pid", value: "host", because: "shares the host PID namespace, so the container sees and can signal host processes"},
	{name: "--network", value: "host", because: "puts the container on the host network, which publishes the engine's port on the LAN"},
	{name: "--net", value: "host", because: "puts the container on the host network, which publishes the engine's port on the LAN"},
	{name: "--userns", value: "host", because: "opts out of user-namespace remapping"},
	{name: "--device", because: "passes a host device through; nothing this image does needs one"},
	{name: "--security-opt", value: "seccomp=unconfined", because: "removes the default seccomp profile"},
	{name: "--security-opt", value: "apparmor=unconfined", because: "removes the default AppArmor profile"},
	{name: "--security-opt", value: "no-new-privileges:false", because: "disables the no-new-privileges bit the same line sets"},
}

// CheckExtraParamsHardening holds an Unraid template's <ExtraParams> to the
// same hardening the compose profiles express as first-class keys, in both
// directions: the required flags have to be there, and nothing on the same
// line may undo them.
func CheckExtraParamsHardening(source, extraParams string) []Violation {
	flags := ParseRunFlags(extraParams)
	value := func(name string) (string, bool) {
		for _, f := range flags {
			if f.Name == name {
				return f.Value, true
			}
		}
		return "", false
	}

	var out []Violation

	if _, ok := value("--read-only"); !ok {
		out = append(out, Violation{source, RuleMissingHardening, "does not pass `--read-only`"})
	}
	if v, ok := value("--cap-drop"); !ok || !strings.EqualFold(v, "ALL") {
		out = append(out, Violation{source, RuleMissingHardening, "does not pass `--cap-drop=ALL`"})
	}
	if v, ok := value("--security-opt"); !ok || !strings.Contains(v, "no-new-privileges:true") {
		out = append(out, Violation{source, RuleMissingHardening, "does not pass `--security-opt=no-new-privileges:true`"})
	}
	if v, ok := value("--tmpfs"); !ok || v == "" {
		out = append(out, Violation{source, RuleMissingHardening, "does not pass a `--tmpfs`, which a read-only rootfs needs for Go's temp directory"})
	}
	switch v, ok := value("--user"); {
	case !ok || v == "":
		out = append(out, Violation{source, RuleMissingHardening, "does not pin a non-root `--user`"})
	case strings.HasPrefix(v, "0:") || v == "0" || strings.HasPrefix(v, "root"):
		out = append(out, Violation{source, RuleUnsafeRunFlag, "runs as root: `--user " + v + "`"})
	}

	for _, f := range flags {
		for _, denied := range deniedRunFlags {
			if f.Name != denied.name {
				continue
			}
			if denied.value != "" && !strings.Contains(f.Value, denied.value) {
				continue
			}
			shown := f.Name
			if f.Value != "" {
				shown += "=" + f.Value
			}
			out = append(out, Violation{source, RuleUnsafeRunFlag,
				fmt.Sprintf("passes %s, which %s", backquote(shown), denied.because)})
		}
	}

	sortViolations(out)
	return out
}

// CheckForwardedHeaderTrust is the one rule covering TRUST_FORWARDED_HEADERS
// across all three metadata formats, which is possible because every one of
// them reduces to a Service with an Environment map.
//
// The flag decides whether the engine believes X-Forwarded-For and
// X-Forwarded-Proto. Believing them means a client can rotate the address
// the login, enrollment and password rate limiters key on, and can assert
// https on a plaintext connection. apps/common/auth/local's own contract is
// that it may only be set where the proxy is the sole possible direct TCP
// peer "by network topology, not merely by convention", so canonical.json
// records per platform whether that holds, and this rule pins each profile
// to that record in both directions: an engine that quietly stops setting
// it changes rate limiting just as silently as one that starts.
//
// edge is the Web UI container, which terminates the operator's own
// connection and is therefore never allowed to trust the header regardless
// of platform.
func CheckForwardedHeaderTrust(svc Service, edge, mayTrust bool) []Violation {
	const key = "TRUST_FORWARDED_HEADERS"
	value, set := svc.Environment[key]
	trusted := set && strings.EqualFold(strings.TrimSpace(value), "true")

	source := svc.Source
	if source == "" {
		source = svc.Name
	}

	switch {
	case edge && trusted:
		return []Violation{{source, RuleForwardedHeaderTrust,
			"the Web UI container sets `" + key + "=true`, but it is the internet-facing edge: every client would then dictate its own rate-limit key on /api/v1/auth/login and /api/v1/auth/enroll, and its own Secure-cookie decision"}}
	case !edge && mayTrust && !trusted:
		return []Violation{{source, RuleForwardedHeaderTrust,
			"the engine does not set `" + key + "=true`, but canonical.json records this platform's engine as reachable only through the Web UI container; leaving it unset silently moves rate limiting onto the proxy's own address"}}
	case !edge && !mayTrust && trusted:
		return []Violation{{source, RuleForwardedHeaderTrust,
			"the engine sets `" + key + "=true`, but canonical.json records this platform's isolation as a convention rather than a topology, so any container an operator later attaches to the same network can forge the header"}}
	}
	return nil
}
