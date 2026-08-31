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
	Role          string
	HostPath      string
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

var composeVarRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// ExpandCompose resolves compose's ${VAR} and ${VAR:-default} forms. env
// wins over the inline default; a reference with neither is returned
// unchanged and reported through the second result.
func ExpandCompose(s string, env map[string]string) (string, []string) {
	var unresolved []string
	out := composeVarRe.ReplaceAllStringFunc(s, func(m string) string {
		groups := composeVarRe.FindStringSubmatch(m)
		name, def := groups[1], groups[2]
		if v, ok := env[name]; ok && v != "" {
			return v
		}
		if strings.Contains(m, ":-") {
			return def
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
	var raw rawCompose
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	canonical := MustLoad()
	rel := filepath.Base(path)

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
			m, err := parseComposeVolume(expand(vol), rel, canonical)
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

func parseComposeVolume(spec, source string, canonical Canonical) (Mount, error) {
	parts := strings.Split(spec, ":")
	if len(parts) < 2 {
		return Mount{}, fmt.Errorf("%s: %q is not host:container[:mode]", source, spec)
	}
	m := Mount{
		HostPath:      parts[0],
		ContainerPath: parts[1],
		Source:        source,
	}
	if len(parts) > 2 && strings.Contains(parts[2], "ro") {
		m.ReadOnly = true
	}
	m.Role = roleForContainerPath(canonical, m.ContainerPath)
	return m, nil
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
