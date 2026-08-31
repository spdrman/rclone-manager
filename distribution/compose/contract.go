// Package compose is the authoritative Compose runtime contract (issue
// #167, EPIC B #81 Phase 6).
//
// # What "authoritative" means here, and what it does not
//
// Before this package, the canonical compose file was documentation
// shaped. It was correct, carefully reasoned and thoroughly commented,
// and none of that was checkable: its security posture was asserted in a
// header comment, its field set was whatever had accumulated, and an
// adapter agreeing with it was a matter of review. Authority in this
// package is a check, not a path. runtime-contract.json names every field
// the canonical runtime definition must declare and every host privilege
// the deployment must not need, and the tests next to it fail the build
// when either list is not satisfied.
//
// # Why the parse is generic rather than a struct
//
// distribution/packaging already parses compose into a Service struct,
// and that is the right shape for the questions it asks (mount roles,
// image references, forwarded-header trust). It is the wrong shape for
// this one. A prohibition check reading a struct can only ever see keys
// the struct has fields for, so `privileged: true` on a service nobody
// modelled reads as clean: the check fails open, silently, in exactly the
// case it exists for. This package walks the whole document instead, and
// TestProhibitionScanSeesKeysTheParserHasNoFieldFor is the control that
// says so.
package compose

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/spdrman/rclone-manager/distribution/packaging"
)

//go:embed runtime-contract.json
var contractJSON []byte

// Contract is runtime-contract.json.
type Contract struct {
	Schema     string           `json:"schema"`
	Version    string           `json:"version"`
	Canonical  string           `json:"canonical"`
	Derived    []string         `json:"derived"`
	Fields     []Field          `json:"fields"`
	Prohibited []ProhibitedRule `json:"prohibited"`
}

// Field is one entry of the standardised field set.
type Field struct {
	ID    string   `json:"id"`
	Scope string   `json:"scope"`
	Roles []string `json:"roles"`
	Key   string   `json:"key"`
	// MustContain, for a command field, is a substring the declared
	// command has to carry.
	MustContain string `json:"mustContain"`
	// ContainerPath, ReadOnly and Writable describe a mount field.
	// ReadOnly and Writable are separate booleans rather than one
	// tri-state because they are two different claims and a mount may
	// legitimately make neither: a mount the contract has no opinion
	// about sets both false, and a field that accidentally sets both is
	// caught by TestEveryMountFieldDeclaresOneWriteMode rather than
	// silently resolving to whichever branch runs first.
	ContainerPath string `json:"containerPath"`
	ReadOnly      bool   `json:"readOnly"`
	Writable      bool   `json:"writable"`
	// Derived says how this field is checked on the artifacts derived
	// from the canonical definition, and DerivedWhy carries the reason
	// for the two policies that do not check it here. See DerivedPolicy.
	Derived    DerivedPolicy `json:"derived"`
	DerivedWhy string        `json:"derivedWhy"`
	Why        string        `json:"why"`
}

// DerivedPolicy is how one field is held against the derived artifacts.
//
// It exists because "the canonical definition declares it" and "every
// adapter does" were being treated as the same claim, and they are not.
// CheckField was only ever invoked against c.Canonical; c.Derived went
// through CheckProhibited alone, and distribution/packaging's derive.go
// covered seven fields that did not include timezone,
// graceful-shutdown-period, restart-policy, ownership or
// explicit-writable-paths. Those five were checked nowhere on any adapter,
// and nobody noticed because the only check able to notice was pointed at
// a different file. Four of the five converted platforms shipped without
// TZ, on a contract whose own reasoning is that retention's calendar
// boundaries depend on it.
//
// The contract's own _comment warns that holding a derived artifact to the
// full field set would make it a second definition rather than a
// derivation. That reasoning holds for the fields derive.go actually
// derives. For the ones it does not, "derived" only ever meant
// "unchecked", so the policy is per field and has to be typed out.
type DerivedPolicy string

const (
	// DerivedChecked: CheckField runs against every derived artifact.
	DerivedChecked DerivedPolicy = "checked"
	// DerivedByDerivationGate: distribution/packaging's derive.go checks
	// it per adapter, in a form this package cannot express, and it
	// reaches formats this package cannot parse (the Unraid XML
	// template). DerivedWhy names the rule.
	DerivedByDerivationGate DerivedPolicy = "derivation-gate"
	// DerivedCanonicalOnly: deliberately not checked on derived
	// artifacts, with the reason stated in DerivedWhy. The only way a
	// field may go unchecked, and it has to be written down.
	DerivedCanonicalOnly DerivedPolicy = "canonical-only"
)

// DerivedCheckedFields is the subset of fields CheckField is run over for
// each derived artifact.
func (c Contract) DerivedCheckedFields() []Field {
	var out []Field
	for _, f := range c.Fields {
		if f.Derived == DerivedChecked {
			out = append(out, f)
		}
	}
	return out
}

// CheckFieldPolicies holds the contract itself to the two rules that stop
// a field from quietly going unchecked.
//
// Without the first, a mount field that declares neither write mode is
// verified for presence and says nothing about `:ro`, which is exactly how
// the configuration mount shipped read-only while every test fixture was
// writable. /data/state and /data/backups were in that position until this
// check existed: a `:ro` on the backup destination in the canonical file
// passed every gate in the tree.
//
// Without the second, a field added later lands in the derived gap by
// default, which is how five of them got there.
func CheckFieldPolicies(c Contract) []Finding {
	var out []Finding
	for _, f := range c.Fields {
		if f.Scope == "service-mount" {
			switch {
			case f.ReadOnly && f.Writable:
				out = append(out, Finding{Rule: f.ID, Detail: fmt.Sprintf("mount field %q at %s declares both readOnly and writable, so checkServiceMount's verdict depends on which branch runs first", f.ID, f.ContainerPath), Why: f.Why})
			case !f.ReadOnly && !f.Writable:
				out = append(out, Finding{Rule: f.ID, Detail: fmt.Sprintf("mount field %q at %s declares neither readOnly nor writable, so its write mode is checked by nothing and a `:ro` there passes every gate", f.ID, f.ContainerPath), Why: f.Why})
			}
		}
		switch f.Derived {
		case DerivedChecked:
		case DerivedByDerivationGate, DerivedCanonicalOnly:
			if strings.TrimSpace(f.DerivedWhy) == "" {
				out = append(out, Finding{Rule: f.ID, Detail: fmt.Sprintf("field %q is %q on derived artifacts and states no derivedWhy; a field that is not checked here has to say what checks it, or that nothing does and why that is right", f.ID, f.Derived), Why: f.Why})
			}
		default:
			out = append(out, Finding{Rule: f.ID, Detail: fmt.Sprintf("field %q declares derived policy %q, which is none of %q, %q or %q; a field with no policy is a field nothing decides to check", f.ID, f.Derived, DerivedChecked, DerivedByDerivationGate, DerivedCanonicalOnly), Why: f.Why})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rule < out[j].Rule })
	return out
}

// ProhibitedRule is one entry of the prohibition list.
type ProhibitedRule struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Key      string   `json:"key"`
	Value    string   `json:"value"`
	Paths    []string `json:"paths"`
	Prefixes []string `json:"prefixes"`
	Why      string   `json:"why"`
}

// LoadContract parses the embedded contract.
func LoadContract() (Contract, error) {
	var c Contract
	if err := json.Unmarshal(contractJSON, &c); err != nil {
		return Contract{}, fmt.Errorf("compose: parse runtime-contract.json: %w", err)
	}
	return c, nil
}

// MustLoadContract is LoadContract for callers that cannot proceed.
func MustLoadContract() Contract {
	c, err := LoadContract()
	if err != nil {
		panic(err)
	}
	return c
}

// Path resolves a repository-relative path from this package's directory,
// which is where `go test` runs.
func Path(rel string) string { return filepath.Join(packaging.RepoRoot, rel) }

// Contains reports whether child is parent or sits underneath it. It is
// packaging's own textual containment check, re-exported so a reader of
// this package's tests does not have to know that two modules' worth of
// path reasoning agree.
func Contains(parent, child string) bool { return packaging.Contains(parent, child) }

// ---------------------------------------------------------------------
// The document
// ---------------------------------------------------------------------

// Role is what a service does, derived from the command it runs.
type Role string

const (
	// RoleEngine is the core service, scheduler, local authentication and
	// the versioned /api/v1 API.
	RoleEngine Role = "engine"
	// RoleWebUI serves the shared UI and reverse-proxies to the engine.
	RoleWebUI Role = "web-ui"
	// RoleUnknown is a service whose command matches neither.
	RoleUnknown Role = ""
)

// Document is one parsed compose file, held as the generic tree it
// actually is.
type Document struct {
	Source string
	root   map[string]any
	env    map[string]string
}

// Read parses the compose file at path, expanding ${VAR} references from
// env the way `docker compose` would.
func Read(path string, env map[string]string) (Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Document{}, err
	}
	rel, relErr := filepath.Rel(Path("."), path)
	if relErr != nil {
		rel = path
	}
	return Parse(data, filepath.ToSlash(rel), env)
}

// Parse is Read over bytes.
func Parse(data []byte, source string, env map[string]string) (Document, error) {
	var root map[string]any
	if err := yaml.Unmarshal(data, &root); err != nil {
		return Document{}, fmt.Errorf("%s: %w", source, err)
	}
	if root == nil {
		root = map[string]any{}
	}
	return Document{Source: source, root: root, env: env}, nil
}

func (d Document) clone() Document {
	return Document{Source: d.Source, root: deepCopy(d.root).(map[string]any), env: d.env}
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = deepCopy(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = deepCopy(val)
		}
		return out
	default:
		return v
	}
}

// services returns the services map, or an empty one.
func (d Document) services() map[string]any {
	svc, _ := d.root["services"].(map[string]any)
	if svc == nil {
		return map[string]any{}
	}
	return svc
}

// ServiceNames lists the declared services, sorted.
func (d Document) ServiceNames() []string {
	out := make([]string, 0)
	for name := range d.services() {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Roles maps each service name to its role.
//
// The role comes from the command, never from the name. apps/truenas
// calls its two services backup-manager and backup-manager-ui and the
// canonical file calls them rclone-manager and web-ui; a check keyed on
// the name would silently stop checking the moment someone renamed one.
func (d Document) Roles() map[string]Role {
	out := map[string]Role{}
	for name, raw := range d.services() {
		out[name] = roleOf(stringList(mapOf(raw)["command"]))
	}
	return out
}

func roleOf(command []string) Role {
	for _, arg := range command {
		switch arg {
		case "serve-ui":
			return RoleWebUI
		case "serve":
			return RoleEngine
		}
	}
	return RoleUnknown
}

// Service returns one service's own declarations by name, or nil. It
// exists so a caller can reach a single service's keys without this
// package exporting the whole generic tree.
func (d Document) Service(name string) map[string]any {
	return mapOf(d.services()[name])
}

// serviceFor returns the one service filling role, and whether there was
// exactly one.
func (d Document) serviceFor(role Role) (string, map[string]any, bool) {
	var name string
	found := 0
	for svc, r := range d.Roles() {
		if r == role {
			name = svc
			found++
		}
	}
	if found != 1 {
		return "", nil, false
	}
	return name, mapOf(d.services()[name]), true
}

// Mount is one declared volume, container side and host side.
type Mount struct {
	HostPath      string
	ContainerPath string
	ReadOnly      bool
}

// Mounts lists a service's declared volumes with ${VAR} references
// expanded. An entry this parser cannot resolve to a host path is NOT
// returned as a Mount: see UnparseableMounts for why answering nothing
// beats answering wrongly.
func (d Document) Mounts(service map[string]any) []Mount {
	parsed, _ := d.mounts(service)
	return parsed
}

// UnparseableMounts lists the raw volume entries this parser refused.
//
// The refusal exists because the corrupt answer was silent. The contract
// mandates the ${VAR:?message} form for every host path, that form
// carries a colon inside its message, and splitting the raw string on
// ":" turned
//
//	${KEY_FILE:?set KEY_FILE in .env to the SFTP private key}:/etc/backup-manager/id_ed25519:ro
//
// into HostPath "${KEY_FILE", ContainerPath "?set KEY_FILE in .env to
// the SFTP private key" and ReadOnly false. Every prohibited-path
// comparison against that HostPath then matched nothing, with no
// diagnostic, which is a security gate failing open rather than a parse
// bug. apps/proxmox/compose/backup-manager.yml was already being checked
// that way. Compose's long volume syntax degraded the same way, through
// a map rendered as one string.
func (d Document) UnparseableMounts(service map[string]any) []string {
	_, refused := d.mounts(service)
	return refused
}

func (d Document) mounts(service map[string]any) ([]Mount, []string) {
	var out []Mount
	var refused []string

	for _, entry := range stringListAny(service["volumes"]) {
		if long, ok := entry.(map[string]any); ok {
			m, parsed, skip := d.longSyntaxMount(long)
			switch {
			case skip:
			case parsed:
				out = append(out, m)
			default:
				refused = append(refused, scalarString(entry))
			}
			continue
		}

		raw := scalarString(entry)
		expanded, unresolved := packaging.ExpandCompose(raw, d.env)
		if len(unresolved) != 0 {
			refused = append(refused, raw)
			continue
		}
		parts := strings.Split(expanded, ":")
		if len(parts) == 1 {
			// An anonymous volume ("- /data/cache"): a container path and
			// no host side at all, so there is no host path to check.
			continue
		}
		if len(parts) > 3 || parts[0] == "" || parts[1] == "" {
			refused = append(refused, raw)
			continue
		}
		m := Mount{HostPath: parts[0], ContainerPath: parts[1]}
		if len(parts) > 2 && strings.Contains(parts[2], "ro") {
			m.ReadOnly = true
		}
		out = append(out, m)
	}
	return out, refused
}

// longSyntaxMount reads compose's long volume syntax. The third result
// asks the caller to skip the entry rather than refuse it: a named
// volume or a tmpfs has no host path to check, which is a different
// claim from "this could not be parsed".
func (d Document) longSyntaxMount(long map[string]any) (Mount, bool, bool) {
	switch scalarString(long["type"]) {
	case "", "bind":
		// Compose's own default for a typeless entry is "volume". It is
		// read as a bind here on purpose: guessing "volume" would skip
		// the prohibition check, and a prohibition that skips is a
		// prohibition that fails open.
	default:
		return Mount{}, false, true
	}

	source, sourceUnresolved := packaging.ExpandCompose(scalarString(long["source"]), d.env)
	target, targetUnresolved := packaging.ExpandCompose(scalarString(long["target"]), d.env)
	if len(sourceUnresolved) != 0 || len(targetUnresolved) != 0 || source == "" || target == "" {
		return Mount{}, false, false
	}

	readOnly, _ := long["read_only"].(bool)
	return Mount{HostPath: source, ContainerPath: target, ReadOnly: readOnly}, true, false
}

// MountFor finds a role's mount at containerPath.
func (d Document) MountFor(role Role, containerPath string) (Mount, bool) {
	_, svc, ok := d.serviceFor(role)
	if !ok {
		return Mount{}, false
	}
	for _, m := range d.Mounts(svc) {
		if m.ContainerPath == containerPath {
			return m, true
		}
	}
	return Mount{}, false
}

// ---------------------------------------------------------------------
// Findings
// ---------------------------------------------------------------------

// Finding is one contract violation. Rule is the id from
// runtime-contract.json, so a failure names the rule that produced it
// rather than only the symptom.
type Finding struct {
	Rule    string
	Service string
	Detail  string
	Why     string
}

// ---------------------------------------------------------------------
// Required fields
// ---------------------------------------------------------------------

// CheckField reports every way this document fails to declare field.
func (d Document) CheckField(field Field) []Finding {
	switch field.Scope {
	case "service":
		return d.checkServiceKey(field)
	case "service-env":
		return d.checkServiceEnv(field)
	case "service-mount":
		return d.checkServiceMount(field)
	case "service-healthcheck":
		return d.checkServiceHealthcheck(field)
	case "document":
		return d.checkDocumentKey(field)
	default:
		return []Finding{{Rule: field.ID, Detail: fmt.Sprintf("unknown field scope %q", field.Scope), Why: field.Why}}
	}
}

func (d Document) checkServiceKey(field Field) []Finding {
	var out []Finding
	for _, roleName := range field.Roles {
		role := Role(roleName)
		name, svc, ok := d.serviceFor(role)
		if !ok {
			out = append(out, Finding{Rule: field.ID, Detail: fmt.Sprintf("%s declares no single service in the %q role, so %q cannot be checked", d.Source, role, field.Key), Why: field.Why})
			continue
		}
		v, present := svc[field.Key]
		if !present || isEmpty(v) {
			out = append(out, Finding{Rule: field.ID, Service: name, Detail: fmt.Sprintf("service %q (%s) declares no %q", name, role, field.Key), Why: field.Why})
			continue
		}
		if field.MustContain != "" {
			joined := strings.Join(stringList(v), " ")
			if !strings.Contains(joined, field.MustContain) {
				out = append(out, Finding{Rule: field.ID, Service: name, Detail: fmt.Sprintf("service %q (%s) declares %s %q, which never contains %q", name, role, field.Key, joined, field.MustContain), Why: field.Why})
			}
		}
	}
	return out
}

// checkServiceHealthcheck reads inside a service's healthcheck block
// rather than at the key holding it, because what an orchestrator waits
// on is the declared test, not the presence of a healthcheck.
func (d Document) checkServiceHealthcheck(field Field) []Finding {
	var out []Finding
	for _, roleName := range field.Roles {
		role := Role(roleName)
		name, svc, ok := d.serviceFor(role)
		if !ok {
			out = append(out, Finding{Rule: field.ID, Detail: fmt.Sprintf("%s declares no single service in the %q role", d.Source, role), Why: field.Why})
			continue
		}
		check := mapOf(svc["healthcheck"])
		v, present := check[field.Key]
		if !present || isEmpty(v) {
			out = append(out, Finding{Rule: field.ID, Service: name, Detail: fmt.Sprintf("service %q (%s) declares no healthcheck %q", name, role, field.Key), Why: field.Why})
			continue
		}
		if field.MustContain != "" {
			joined := strings.Join(stringList(v), " ")
			if !strings.Contains(joined, field.MustContain) {
				out = append(out, Finding{Rule: field.ID, Service: name, Detail: fmt.Sprintf("service %q (%s) declares the healthcheck %s %q, which never contains %q", name, role, field.Key, joined, field.MustContain), Why: field.Why})
			}
		}
	}
	return out
}

func (d Document) checkServiceEnv(field Field) []Finding {
	var out []Finding
	for _, roleName := range field.Roles {
		role := Role(roleName)
		name, svc, ok := d.serviceFor(role)
		if !ok {
			out = append(out, Finding{Rule: field.ID, Detail: fmt.Sprintf("%s declares no single service in the %q role", d.Source, role), Why: field.Why})
			continue
		}
		env := mapOf(svc["environment"])
		if v, present := env[field.Key]; !present || isEmpty(v) {
			out = append(out, Finding{Rule: field.ID, Service: name, Detail: fmt.Sprintf("service %q (%s) sets no %s environment variable", name, role, field.Key), Why: field.Why})
		}
	}
	return out
}

func (d Document) checkServiceMount(field Field) []Finding {
	var out []Finding
	for _, roleName := range field.Roles {
		role := Role(roleName)
		name, svc, ok := d.serviceFor(role)
		if !ok {
			out = append(out, Finding{Rule: field.ID, Detail: fmt.Sprintf("%s declares no single service in the %q role", d.Source, role), Why: field.Why})
			continue
		}
		var found *Mount
		for _, m := range d.Mounts(svc) {
			if m.ContainerPath == field.ContainerPath {
				found = &m
				break
			}
		}
		if found == nil {
			out = append(out, Finding{Rule: field.ID, Service: name, Detail: fmt.Sprintf("service %q (%s) mounts nothing at %s", name, role, field.ContainerPath), Why: field.Why})
			continue
		}
		if field.ReadOnly && !found.ReadOnly {
			out = append(out, Finding{Rule: field.ID, Service: name, Detail: fmt.Sprintf("service %q (%s) mounts %s writable; the contract requires it read-only", name, role, field.ContainerPath), Why: field.Why})
		}
		if field.Writable && found.ReadOnly {
			out = append(out, Finding{Rule: field.ID, Service: name, Detail: fmt.Sprintf("service %q (%s) mounts %s read-only; the contract requires it writable, because the application creates and atomically replaces what is under it", name, role, field.ContainerPath), Why: field.Why})
		}
	}
	return out
}

func (d Document) checkDocumentKey(field Field) []Finding {
	v, ok := lookupDotted(d.root, field.Key)
	if !ok || isEmpty(v) {
		return []Finding{{Rule: field.ID, Detail: fmt.Sprintf("%s declares no %s", d.Source, field.Key), Why: field.Why}}
	}
	return nil
}

// WithoutField returns a copy of this document with field removed, and a
// description of what was removed. It exists for the positive controls:
// a required-field check nobody has watched fail is a check that may not
// work at all.
func (d Document) WithoutField(field Field) (Document, string) {
	out := d.clone()
	switch field.Scope {
	case "service":
		for _, roleName := range field.Roles {
			name, svc, ok := out.serviceFor(Role(roleName))
			if !ok {
				continue
			}
			if field.MustContain != "" {
				// Removing the key outright would also trip the
				// "present" half, which would let a MustContain rule
				// that never actually inspects the value still pass its
				// own control. Strip only the required substring.
				svc[field.Key] = withoutArg(stringList(svc[field.Key]), field.MustContain)
				return out, fmt.Sprintf("%s's %q from service %q", field.MustContain, field.Key, name)
			}
			delete(svc, field.Key)
			return out, fmt.Sprintf("%q from service %q", field.Key, name)
		}
	case "service-healthcheck":
		for _, roleName := range field.Roles {
			name, svc, ok := out.serviceFor(Role(roleName))
			if !ok {
				continue
			}
			check := mapOf(svc["healthcheck"])
			if check == nil {
				continue
			}
			if field.MustContain != "" {
				// Same reasoning as the service case above: removing the
				// whole test would also trip the "declared at all" half,
				// so only the required argument goes.
				check[field.Key] = withoutArg(stringList(check[field.Key]), field.MustContain)
				return out, fmt.Sprintf("%s's %q from service %q's healthcheck", field.MustContain, field.Key, name)
			}
			delete(check, field.Key)
			return out, fmt.Sprintf("healthcheck %q from service %q", field.Key, name)
		}
	case "service-env":
		for _, roleName := range field.Roles {
			name, svc, ok := out.serviceFor(Role(roleName))
			if !ok {
				continue
			}
			delete(mapOf(svc["environment"]), field.Key)
			return out, fmt.Sprintf("environment %s from service %q", field.Key, name)
		}
	case "service-mount":
		for _, roleName := range field.Roles {
			name, svc, ok := out.serviceFor(Role(roleName))
			if !ok {
				continue
			}
			var kept []any
			for _, raw := range stringList(svc["volumes"]) {
				expanded, _ := packaging.ExpandCompose(raw, out.env)
				if strings.Contains(expanded, ":"+field.ContainerPath) {
					continue
				}
				kept = append(kept, raw)
			}
			svc["volumes"] = kept
			return out, fmt.Sprintf("the %s mount from service %q", field.ContainerPath, name)
		}
	case "document":
		parts := strings.Split(field.Key, ".")
		parent := out.root
		for _, p := range parts[:len(parts)-1] {
			parent = mapOf(parent[p])
			if parent == nil {
				return out, ""
			}
		}
		delete(parent, parts[len(parts)-1])
		return out, field.Key
	}
	return out, ""
}

// WithWrongWriteMode returns a copy of this document whose mount for
// field carries the opposite write mode, and a description of what was
// changed. It is the positive control for the two write-mode branches of
// checkServiceMount: a rule that only ever runs against a correct
// document is a rule nobody has watched fail.
//
// It returns ok=false for a field that declares no write mode at all,
// so a caller cannot quietly "control" a rule that does not exist.
func (d Document) WithWrongWriteMode(field Field) (Document, string, bool) {
	if field.Scope != "service-mount" || (!field.ReadOnly && !field.Writable) {
		return d, "", false
	}
	out := d.clone()
	for _, roleName := range field.Roles {
		name, svc, ok := out.serviceFor(Role(roleName))
		if !ok {
			continue
		}
		vols := stringList(svc["volumes"])
		changed := make([]any, 0, len(vols))
		what := ""
		for _, raw := range vols {
			expanded, _ := packaging.ExpandCompose(raw, out.env)
			parts := strings.Split(expanded, ":")
			if len(parts) < 2 || parts[1] != field.ContainerPath {
				changed = append(changed, raw)
				continue
			}
			if field.Writable {
				changed = append(changed, raw+":ro")
				what = fmt.Sprintf("%s read-only on service %q", field.ContainerPath, name)
			} else {
				changed = append(changed, strings.TrimSuffix(raw, ":ro"))
				what = fmt.Sprintf("%s writable on service %q", field.ContainerPath, name)
			}
		}
		if what == "" {
			continue
		}
		svc["volumes"] = changed
		return out, what, true
	}
	return out, "", false
}

func withoutArg(args []string, contains string) []any {
	out := make([]any, 0, len(args))
	for _, a := range args {
		if strings.Contains(a, contains) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// ---------------------------------------------------------------------
// Prohibitions
// ---------------------------------------------------------------------

// CheckProhibited reports every prohibited host privilege this document
// requires, wherever in the tree it appears.
func (d Document) CheckProhibited(c Contract) []Finding {
	var out []Finding
	for _, rule := range c.Prohibited {
		out = append(out, d.checkRule(rule)...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rule != out[j].Rule {
			return out[i].Rule < out[j].Rule
		}
		return out[i].Detail < out[j].Detail
	})
	return out
}

func (d Document) checkRule(rule ProhibitedRule) []Finding {
	var out []Finding
	add := func(service, detail string) {
		out = append(out, Finding{Rule: rule.ID, Service: service, Detail: detail, Why: rule.Why})
	}

	switch rule.Kind {
	case "key-value":
		walk(d.root, "", func(owner, key string, value any) {
			if key != rule.Key {
				return
			}
			if strings.EqualFold(scalarString(value), rule.Value) {
				add(owner, fmt.Sprintf("service %q declares %s=%s", owner, rule.Key, rule.Value))
			}
		})
	case "key-present":
		walk(d.root, "", func(owner, key string, value any) {
			if key == rule.Key && !isEmpty(value) {
				add(owner, fmt.Sprintf("service %q declares %s: %v", owner, rule.Key, value))
			}
		})
	case "mount-host-path":
		for name, raw := range d.services() {
			parsed, refused := d.mounts(mapOf(raw))
			for _, m := range parsed {
				for _, p := range rule.Paths {
					if hostPathMatches(m.HostPath, p) {
						add(name, fmt.Sprintf("service %q mounts the host path %s (at %s)", name, p, m.ContainerPath))
					}
				}
			}
			// A volume nothing could resolve is reported rather than
			// passed over. The rule's question is "does this deployment
			// mount one of these paths", and "I could not tell" is not
			// an answer of no - which is exactly how the corrupted parse
			// used to answer it.
			for _, entry := range refused {
				add(name, fmt.Sprintf("service %q declares the volume %s, which this contract cannot resolve to a host path, so it cannot be shown to be free of %s", name, entry, strings.Join(rule.Paths, ", ")))
			}
		}
	case "list-value-prefix":
		walk(d.root, "", func(owner, key string, value any) {
			if key != rule.Key {
				return
			}
			for _, entry := range stringList(value) {
				for _, prefix := range rule.Prefixes {
					if strings.HasPrefix(entry, prefix) {
						add(owner, fmt.Sprintf("service %q declares %s %s", owner, rule.Key, prefix))
					}
				}
			}
		})
	}
	return out
}

// hostPathMatches decides whether a declared host path is the prohibited
// one. It is packaging.HostPathIsAt, re-expressed here only so this
// package's call sites read locally.
//
// It used to be a second implementation, and the two normalised
// differently: this one cleaned the path and packaging's trimmed a
// trailing slash, so //var/run/docker.sock was caught here and missed
// there. The test that claimed to pin the two rules pinned the two path
// LISTS, so the behavioural difference was invisible to it while its name
// read as though it were covered. One function, two callers, no drift.
func hostPathMatches(hostPath, prohibited string) bool {
	return packaging.HostPathIsAt(hostPath, prohibited)
}

// walk visits every key/value pair in the tree, carrying the name of the
// nearest enclosing service so a finding can say where it was found. It
// descends into maps this package has no type for on purpose: that is the
// whole difference between this and a struct-based scan.
func walk(node any, owner string, visit func(owner, key string, value any)) {
	switch t := node.(type) {
	case map[string]any:
		for k, v := range t {
			next := owner
			if owner == "" {
				if k == "services" {
					if services, ok := v.(map[string]any); ok {
						for name, svc := range services {
							walk(svc, name, visit)
						}
						continue
					}
				}
			}
			visit(next, k, v)
			walk(v, next, visit)
		}
	case []any:
		for _, v := range t {
			walk(v, owner, visit)
		}
	}
}

// WithProhibited returns a copy of this document that requires rule's
// prohibited setting, and a description of what was injected. The tests
// use it to prove each rule actually fires.
func (d Document) WithProhibited(rule ProhibitedRule) (Document, string) {
	out := d.clone()
	name, svc, ok := out.serviceFor(RoleEngine)
	if !ok {
		for _, n := range out.ServiceNames() {
			name, svc = n, mapOf(out.services()[n])
			break
		}
	}
	if svc == nil {
		return out, ""
	}
	_ = name

	switch rule.Kind {
	case "key-value":
		svc[rule.Key] = parseScalar(rule.Value)
		return out, fmt.Sprintf("%s=%s", rule.Key, rule.Value)
	case "key-present":
		svc[rule.Key] = []any{"SYS_ADMIN"}
		return out, rule.Key
	case "mount-host-path":
		if len(rule.Paths) == 0 {
			return out, ""
		}
		p := rule.Paths[len(rule.Paths)-1]
		svc["volumes"] = append(stringListAny(svc["volumes"]), p+":/mnt/injected:ro")
		return out, p
	case "list-value-prefix":
		if len(rule.Prefixes) == 0 {
			return out, ""
		}
		p := rule.Prefixes[len(rule.Prefixes)-1]
		svc[rule.Key] = append(stringListAny(svc[rule.Key]), p)
		return out, p
	}
	return out, ""
}

// WithVolume returns a copy of this document with entry appended to the
// engine service's volumes, and a description of what was injected. Like
// WithProhibited it exists for the positive controls, and specifically
// for the two mount shapes WithProhibited's own injection cannot reach:
// it appends a literal host path in short syntax, so it only ever
// exercises the case that already worked.
func (d Document) WithVolume(entry any) (Document, string) {
	out := d.clone()
	_, svc, ok := out.serviceFor(RoleEngine)
	if !ok || svc == nil {
		return out, ""
	}
	svc["volumes"] = append(stringListAny(svc["volumes"]), entry)
	return out, fmt.Sprintf("%v", entry)
}

// HealthcheckTest reads one role's declared healthcheck test, and says
// whether the role declared one at all.
//
// Exported because the canonical definition is where the health checks
// are DECIDED (issue #206) and distribution/packaging/canonical.json
// only restates them, so something has to be able to read both and
// compare. A restatement nothing compares is how the engine's start gate
// came to say two different things in two files for three work packages
// running.
func (d Document) HealthcheckTest(role Role) ([]string, bool) {
	_, svc, ok := d.serviceFor(role)
	if !ok || svc == nil {
		return nil, false
	}
	check := mapOf(svc["healthcheck"])
	if check == nil {
		return nil, false
	}
	v, present := check["test"]
	if !present || isEmpty(v) {
		return nil, false
	}
	return stringList(v), true
}

// WithServiceHealthcheckTest returns a copy of this document with role's
// healthcheck test replaced. Like WithProhibited and WithVolume it exists
// for the positive controls, here so a control can put back the exact
// check the start gate must never be again.
func (d Document) WithServiceHealthcheckTest(role Role, test []any) Document {
	out := d.clone()
	_, svc, ok := out.serviceFor(role)
	if !ok || svc == nil {
		return out
	}
	check := mapOf(svc["healthcheck"])
	if check == nil {
		check = map[string]any{}
		svc["healthcheck"] = check
	}
	check["test"] = test
	return out
}

// ---------------------------------------------------------------------
// The document-level runtime declarations
// ---------------------------------------------------------------------

// Architectures reads x-canonical-runtime.architectures.
func (d Document) Architectures() ([]string, error) {
	v, ok := lookupDotted(d.root, "x-canonical-runtime.architectures")
	if !ok {
		return nil, fmt.Errorf("%s declares no x-canonical-runtime.architectures", d.Source)
	}
	out := stringList(v)
	if len(out) == 0 {
		return nil, fmt.Errorf("%s declares an empty x-canonical-runtime.architectures", d.Source)
	}
	return out, nil
}

// Profiles reads x-canonical-runtime.profiles.
func (d Document) Profiles() ([]string, error) {
	v, ok := lookupDotted(d.root, "x-canonical-runtime.profiles")
	if !ok {
		return nil, fmt.Errorf("%s declares no x-canonical-runtime.profiles", d.Source)
	}
	out := stringList(v)
	if len(out) == 0 {
		return nil, fmt.Errorf("%s declares an empty x-canonical-runtime.profiles", d.Source)
	}
	return out, nil
}

// Policy is the declared digest policy.
type Policy struct {
	Manifest string
	Pin      string
}

// DigestPolicy reads x-canonical-runtime.digest_policy.
func (d Document) DigestPolicy() (Policy, error) {
	v, ok := lookupDotted(d.root, "x-canonical-runtime.digest_policy")
	if !ok {
		return Policy{}, fmt.Errorf("%s declares no x-canonical-runtime.digest_policy", d.Source)
	}
	m := mapOf(v)
	return Policy{
		Manifest: scalarString(m["manifest"]),
		Pin:      scalarString(m["pin"]),
	}, nil
}

// ---------------------------------------------------------------------
// What the executable actually implements
// ---------------------------------------------------------------------

// profileConstRe matches one row of the profile table's ID constants.
var profileConstRe = regexp.MustCompile(`(?m)^\t([A-Z][A-Za-z0-9]*)\s+ID\s*=\s*"([a-z0-9-]+)"`)

// ImplementedProfiles reads the runtime profile table's selector tokens
// out of apps/common/platform/profile.
//
// Read as source rather than linked, because distribution/ is its own
// module and the layer rules forbid it importing the application layer. A
// profile constant is a declaration, and reading a declaration across
// that boundary is exactly what the boundary permits; importing the
// package to run its code is what it does not.
func ImplementedProfiles() ([]string, error) {
	path := Path(filepath.Join("apps", "common", "platform", "profile", "profile.go"))
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	matches := profileConstRe.FindAllStringSubmatch(string(data), -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("compose: found no runtime profile constants in %s, so this check would compare against an empty list", path)
	}
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[2])
	}
	sort.Strings(out)
	return out, nil
}

// ---------------------------------------------------------------------
// Small helpers over the generic tree
// ---------------------------------------------------------------------

func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// stringList reads a YAML scalar, sequence or mapping as a list of
// strings. A mapping (compose's alternative `environment:` form) yields
// "KEY=VALUE" entries.
func stringList(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			out = append(out, scalarString(e))
		}
		return out
	case string:
		return []string{t}
	default:
		return []string{scalarString(v)}
	}
}

func stringListAny(v any) []any {
	if l, ok := v.([]any); ok {
		return l
	}
	if v == nil {
		return nil
	}
	return []any{v}
}

func scalarString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", t)
	}
}

func parseScalar(v string) any {
	switch strings.ToLower(v) {
	case "true":
		return true
	case "false":
		return false
	default:
		return v
	}
}

func isEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	default:
		return false
	}
}

func lookupDotted(root map[string]any, dotted string) (any, bool) {
	cur := any(root)
	for _, part := range strings.Split(dotted, ".") {
		m := mapOf(cur)
		if m == nil {
			return nil, false
		}
		v, ok := m[part]
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}
