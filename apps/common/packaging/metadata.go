package packaging

import (
	"bufio"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Mount is one host-path-to-container-path binding a provider package
// declares, whatever format it declared it in. Reducing a compose
// `volumes:` entry, an Unraid `<Config Type="Path">` element and an OMV
// env-file variable to one shape is what lets a single table-driven
// assertion hold all three platforms to the same storage contract instead
// of three near-identical assertions drifting apart.
type Mount struct {
	// Role is the canonical storage role this mount fills ("state",
	// "backups", "config", "sshKey", "knownHosts"), derived from the
	// CONTAINER path, which is fixed by the binaries themselves. Empty
	// when the container path is not one the canonical image knows about,
	// which is itself a finding.
	Role     string
	HostPath string
	// HostPathRaw is the host side before variable expansion, so a rule
	// can ask how a path would behave when the operator has not set the
	// variable, not merely what it expanded to on this machine.
	HostPathRaw   string
	ContainerPath string
	ReadOnly      bool
	Source        string
}

// Service is one container a provider package declares.
type Service struct {
	Name            string
	Image           string
	Command         []string
	Mounts          []Mount
	Ports           []string
	User            string
	ReadOnlyRootFS  bool
	Environment     map[string]string
	HealthcheckTest []string
	CapDrop         []string
	SecurityOpt     []string
	Tmpfs           []string
	Source          string
	// ExtraParams is Unraid's only seam for anything its template schema
	// has no element for (a read-only rootfs, a dropped capability set, a
	// disabled healthcheck). Compose expresses all of those as first-class
	// keys, so this stays empty for the compose profiles and the tests
	// read whichever of the two a platform actually uses.
	ExtraParams string
	// HealthcheckDisabled records `--no-healthcheck`. The canonical image
	// bakes in `HEALTHCHECK /backup-manager status`, which needs a config
	// file and a state database. The Web UI container has neither, so
	// every profile has to do something about it: the compose profiles
	// override the test with `/backup-manager-web healthcheck`, and Unraid,
	// whose --health-cmd would run through a shell the distroless image
	// does not contain, disables it instead.
	HealthcheckDisabled bool
	// UnresolvedVars lists ${VAR} references with no default. A packaging
	// profile whose image reference or storage path only resolves when an
	// operator happens to have set an environment variable is a profile
	// that silently installs wrong, so the tests treat these as findings
	// rather than as flexibility.
	UnresolvedVars []string
}

type rawCompose struct {
	Services map[string]rawService `yaml:"services"`
}

type rawService struct {
	Image       string            `yaml:"image"`
	Command     []string          `yaml:"command"`
	Volumes     []string          `yaml:"volumes"`
	Ports       []string          `yaml:"ports"`
	User        string            `yaml:"user"`
	ReadOnly    bool              `yaml:"read_only"`
	Environment map[string]string `yaml:"environment"`
	CapDrop     []string          `yaml:"cap_drop"`
	SecurityOpt []string          `yaml:"security_opt"`
	Tmpfs       []string          `yaml:"tmpfs"`
	Healthcheck *struct {
		Test    []string `yaml:"test"`
		Disable bool     `yaml:"disable"`
	} `yaml:"healthcheck"`
}

var composeVarRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::([-?])([^}]*))?\}`)

// VarRef is one ${VAR} reference in a compose file, in whichever of the
// three forms compose allows. The distinction matters to the packaging
// rules rather than to the expansion: a storage path written
// ${STATE_DIR:-/some/path} lands somewhere plausible when the variable is
// unset, and a profile that lands somewhere plausible instead of refusing
// to start is how a backup root ends up on the OS disk.
type VarRef struct {
	Name string
	// Default is the text after `:-`, meaningful only when HasDefault.
	Default    string
	HasDefault bool
	// FailClosed records the ${VAR:?message} form, which stops the
	// deployment rather than substituting anything.
	FailClosed bool
}

// VarRefs returns every variable reference in s, in order.
func VarRefs(s string) []VarRef {
	var out []VarRef
	for _, m := range composeVarRe.FindAllStringSubmatch(s, -1) {
		ref := VarRef{Name: m[1]}
		switch m[2] {
		case "-":
			ref.HasDefault = true
			ref.Default = m[3]
		case "?":
			ref.FailClosed = true
		}
		out = append(out, ref)
	}
	return out
}

// ExpandCompose resolves compose's ${VAR}, ${VAR:-default} and
// ${VAR:?message} forms. env wins over an inline default; a reference with
// no usable value is returned unchanged and reported through the second
// result, which covers both the bare form and the fail-closed form.
//
// The fail-closed form is not a curiosity: container/compose.yaml uses it
// for every host path precisely so that an unset STATE_DIR stops the
// deployment rather than silently landing somewhere. This parser has to
// understand it, otherwise a profile that adopts it has the literal
// ${STATE_DIR:?...} compared against canonical.json's host path.
func ExpandCompose(s string, env map[string]string) (string, []string) {
	var unresolved []string
	out := composeVarRe.ReplaceAllStringFunc(s, func(m string) string {
		groups := composeVarRe.FindStringSubmatch(m)
		name, form, value := groups[1], groups[2], groups[3]
		if v, ok := env[name]; ok && v != "" {
			return v
		}
		if form == "-" {
			return value
		}
		unresolved = append(unresolved, name)
		return m
	})
	return out, unresolved
}

// ReadEnvFile parses a docker-compose style env file: KEY=value lines,
// `#` comments, no shell expansion.
func ReadEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	env := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("%s: %q is not KEY=value", path, line)
		}
		env[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return env, nil
}

// ReadCompose parses a compose file into Services, expanding variables
// against env (which may be nil, in which case only inline defaults
// apply).
func ReadCompose(path string, env map[string]string) ([]Service, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseCompose(data, filepath.Base(path), env)
}

// ParseCompose is ReadCompose over bytes that never had to be a file on
// disk. The TrueNAS catalog's compose is a template rendered from
// ix_values.yaml, and the whole point of M2's rule is to check the
// rendered artifact an app-store install actually gets rather than the
// paste-in compose file sitting next to it.
func ParseCompose(data []byte, source string, env map[string]string) ([]Service, error) {
	var raw rawCompose
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}

	canonical := MustLoad()
	rel := source

	names := make([]string, 0, len(raw.Services))
	for name := range raw.Services {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]Service, 0, len(names))
	for _, name := range names {
		rs := raw.Services[name]
		svc := Service{
			Name:           name,
			Command:        rs.Command,
			User:           rs.User,
			ReadOnlyRootFS: rs.ReadOnly,
			Environment:    map[string]string{},
			CapDrop:        rs.CapDrop,
			SecurityOpt:    rs.SecurityOpt,
			Tmpfs:          rs.Tmpfs,
			Source:         rel,
		}

		var unresolved []string
		expand := func(s string) string {
			v, u := ExpandCompose(s, env)
			unresolved = append(unresolved, u...)
			return v
		}

		svc.Image = expand(rs.Image)
		for k, v := range rs.Environment {
			svc.Environment[k] = expand(v)
		}
		for _, p := range rs.Ports {
			svc.Ports = append(svc.Ports, expand(p))
		}
		if rs.Healthcheck != nil {
			svc.HealthcheckTest = rs.Healthcheck.Test
			svc.HealthcheckDisabled = rs.Healthcheck.Disable
		}
		for _, vol := range rs.Volumes {
			m, err := parseComposeVolume(vol, expand(vol), rel, canonical)
			if err != nil {
				return nil, err
			}
			svc.Mounts = append(svc.Mounts, m)
		}

		svc.UnresolvedVars = unresolved
		out = append(out, svc)
	}
	return out, nil
}

func parseComposeVolume(raw, spec, source string, canonical Canonical) (Mount, error) {
	parts := splitVolumeSpec(spec)
	if len(parts) < 2 {
		return Mount{}, fmt.Errorf("%s: %q is not host:container[:mode]", source, spec)
	}
	rawParts := splitVolumeSpec(raw)
	m := Mount{
		HostPath:      parts[0],
		HostPathRaw:   rawParts[0],
		ContainerPath: parts[1],
		Source:        source,
	}
	if len(parts) > 2 && strings.Contains(parts[2], "ro") {
		m.ReadOnly = true
	}
	m.Role = roleForContainerPath(canonical, m.ContainerPath)
	return m, nil
}

// splitVolumeSpec splits host:container[:mode] on the separators only.
// A plain strings.Split cannot be used: ${STATE_DIR:?set STATE_DIR} is one
// field containing two colons, and an unresolved reference survives
// expansion intact, so the naive split turns one volume into four
// meaningless parts.
func splitVolumeSpec(spec string) []string {
	var parts []string
	depth, start := 0, 0
	for i := 0; i < len(spec); i++ {
		switch {
		case strings.HasPrefix(spec[i:], "${"):
			depth++
			i++
		case spec[i] == '}' && depth > 0:
			depth--
		case spec[i] == ':' && depth == 0:
			parts = append(parts, spec[start:i])
			start = i + 1
		}
	}
	return append(parts, spec[start:])
}

func roleForContainerPath(canonical Canonical, containerPath string) string {
	for _, role := range Roles {
		p, _ := canonical.ContainerPaths.ByRole(role)
		if p == containerPath {
			return role
		}
	}
	return ""
}

// UnraidTemplate is the Unraid Docker template schema, read only as far as
// this package needs it.
type UnraidTemplate struct {
	XMLName     xml.Name       `xml:"Container"`
	Name        string         `xml:"Name"`
	Repository  string         `xml:"Repository"`
	Registry    string         `xml:"Registry"`
	Network     string         `xml:"Network"`
	Privileged  string         `xml:"Privileged"`
	Support     string         `xml:"Support"`
	Project     string         `xml:"Project"`
	Overview    string         `xml:"Overview"`
	Category    string         `xml:"Category"`
	WebUI       string         `xml:"WebUI"`
	Icon        string         `xml:"Icon"`
	ExtraParms  string         `xml:"ExtraParams"`
	PostArgs    string         `xml:"PostArgs"`
	Requires    string         `xml:"Requires"`
	Config      []UnraidConfig `xml:"Config"`
	TemplateURL string         `xml:"TemplateURL"`
}

type UnraidConfig struct {
	Name        string `xml:"Name,attr"`
	Target      string `xml:"Target,attr"`
	Default     string `xml:"Default,attr"`
	Mode        string `xml:"Mode,attr"`
	Description string `xml:"Description,attr"`
	Type        string `xml:"Type,attr"`
	Display     string `xml:"Display,attr"`
	Required    string `xml:"Required,attr"`
	Mask        string `xml:"Mask,attr"`
	Value       string `xml:",chardata"`
}

// ReadUnraidTemplate parses one Unraid Docker template XML.
func ReadUnraidTemplate(path string) (UnraidTemplate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return UnraidTemplate{}, err
	}
	var t UnraidTemplate
	if err := xml.Unmarshal(data, &t); err != nil {
		return UnraidTemplate{}, fmt.Errorf("%s: %w", path, err)
	}
	return t, nil
}

// AsService renders an Unraid template into the same shape a compose
// service reduces to, so one set of assertions covers both.
func (t UnraidTemplate) AsService(source string) Service {
	canonical := MustLoad()
	svc := Service{
		Name:                t.Name,
		Image:               t.Repository,
		Environment:         map[string]string{},
		Source:              source,
		ExtraParams:         t.ExtraParms,
		HealthcheckDisabled: strings.Contains(t.ExtraParms, "--no-healthcheck"),
		// <PostArgs> is Unraid's container command (see scan.go's
		// scanXML), so it lands in the same field a compose `command:`
		// does and the same assertions cover both.
		Command: strings.Fields(t.PostArgs),
	}
	for _, c := range t.Config {
		value := strings.TrimSpace(c.Value)
		if value == "" {
			value = c.Default
		}
		switch strings.ToLower(c.Type) {
		case "path":
			m := Mount{
				HostPath:      value,
				ContainerPath: c.Target,
				ReadOnly:      strings.EqualFold(c.Mode, "ro"),
				Source:        source,
			}
			m.Role = roleForContainerPath(canonical, m.ContainerPath)
			svc.Mounts = append(svc.Mounts, m)
		case "port":
			svc.Ports = append(svc.Ports, value+":"+c.Target)
		case "variable":
			svc.Environment[c.Target] = value
		}
	}
	return svc
}

// bridgeStorageMountRe reads deployment.storageMount out of a provider's
// own frontend bridge. Reading TypeScript with a regular expression is
// crude, but the alternative is letting apps/<platform>/frontend/
// platform.ts and apps/common/packaging/canonical.json disagree about
// where a platform stores things, which is exactly the drift WP4.3's
// REFACTOR step is about. The regex is anchored on the property name and
// the test fails loudly when it matches nothing, so it cannot silently
// stop checking.
var bridgeStorageMountRe = regexp.MustCompile(`storageMount:\s*"([^"]+)"`)

// BridgeStorageMount extracts deployment.storageMount from a provider
// frontend bridge module.
func BridgeStorageMount(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	m := bridgeStorageMountRe.FindSubmatch(data)
	if m == nil {
		return "", fmt.Errorf("%s: no deployment.storageMount found; the extractor needs updating, not deleting", path)
	}
	return string(m[1]), nil
}
