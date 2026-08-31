package packaging

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// This file is the platform-specific half of issue #170: the three store
// or manager formats the four new targets bring with them, and the rules
// that keep each of them honest.
//
// None of the four is a conversion of anything. Phase 4 targeted no
// container manager and no third-party app store, so Portainer, Dockge,
// CasaOS and ZimaOS arrive here with no predecessor packaging to
// convert and nothing they replace.
//
// The shapes are constrained by EPIC B #81's exit gate rather than
// chosen: Portainer is template or stack based and never a product
// plugin, Dockge uses the canonical Compose stack itself, and CasaOS and
// ZimaOS reuse Compose and container semantics with `x-casaos` metadata
// wrapped around them. Each rule below is one of those sentences made
// executable.

const (
	// RulePortainerTemplate is a Portainer App Template that does not
	// describe the stack it points at.
	RulePortainerTemplate = "portainer-template"
	// RuleProductPlugin is the line #170 draws by name: no Portainer
	// plugin, no Portainer agent, no Portainer API dependency, no Dockge
	// plugin.
	RuleProductPlugin = "product-plugin-or-agent"
	// RuleStoreMetadata is a CasaOS/ZimaOS `x-casaos` block that does not
	// describe the services beside it.
	RuleStoreMetadata = "store-metadata"
	// RuleStoreMetadataLeak is store or manager metadata that has reached
	// the provider-neutral core or the shared UI.
	RuleStoreMetadataLeak = "store-metadata-leak"
	// RuleForkedStack is a runtime definition in an adapter whose whole
	// support model is that it has none.
	RuleForkedStack = "forked-stack"
)

// ---------------------------------------------------------------------
// Portainer App Templates
// ---------------------------------------------------------------------

// PortainerTemplates is apps/portainer/templates.json, read as far as the
// rules need it.
type PortainerTemplates struct {
	Version   string              `json:"version"`
	Templates []PortainerTemplate `json:"templates"`
}

// PortainerTemplate is one entry of the App Template file.
// https://docs.portainer.io/advanced/app-templates/format
type PortainerTemplate struct {
	// Type 1 is a container, 2 a Swarm stack, 3 a Compose stack. Only 3
	// is a shape this product can be deployed through without Portainer
	// creating the container itself from template fields.
	Type        int      `json:"type"`
	Title       string   `json:"title"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Note        string   `json:"note"`
	Categories  []string `json:"categories"`
	Platform    string   `json:"platform"`
	Logo        string   `json:"logo"`
	Image       string   `json:"image"`
	Privileged  bool     `json:"privileged"`
	Volumes     []struct {
		Container string `json:"container"`
		Bind      string `json:"bind"`
	} `json:"volumes"`
	Repository struct {
		URL       string `json:"url"`
		Stackfile string `json:"stackfile"`
	} `json:"repository"`
	Env []PortainerEnv `json:"env"`
}

// PortainerEnv is one field of the form Portainer renders before it
// deploys the stack.
type PortainerEnv struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Default     string `json:"default"`
}

// ReadPortainerTemplates parses an App Template file.
func ReadPortainerTemplates(path string) (PortainerTemplates, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PortainerTemplates{}, err
	}
	var t PortainerTemplates
	if err := json.Unmarshal(data, &t); err != nil {
		return PortainerTemplates{}, fmt.Errorf("%s: %w", path, err)
	}
	return t, nil
}

// CheckPortainerTemplate holds the App Template to the stack it deploys.
//
// The interesting failure here is not a malformed template, it is a
// template that is perfectly well formed and describes a different
// deployment from the one in the compose file beside it. Portainer
// renders `env` into a form, so a variable the form does not offer is a
// variable the operator never sets, and the fail-closed `${VAR:?}` forms
// in the stack turn that into a deployment that refuses to start with a
// message about a variable nobody was asked for. A default that differs
// from the env file's is worse: it starts, somewhere else.
//
// composeVars is every variable the stack reads and env is the
// authoritative env file, both passed in so this can run against a
// fixture.
func CheckPortainerTemplate(source string, t PortainerTemplates, composeVars []string, env map[string]string, stackfile string) []Violation {
	var out []Violation

	if t.Version == "" {
		out = append(out, Violation{source, RulePortainerTemplate, "declares no template format version"})
	}
	if len(t.Templates) != 1 {
		out = append(out, Violation{source, RulePortainerTemplate,
			fmt.Sprintf("declares %d templates; this adapter is one product and one stack", len(t.Templates))})
		return out
	}
	tpl := t.Templates[0]

	if tpl.Type != 3 {
		out = append(out, Violation{source, RulePortainerTemplate,
			fmt.Sprintf("is type %d; only type 3 (a Compose stack) deploys the canonical two-container runtime. Types 1 and 2 make Portainer build the container out of template fields, which is a second runtime definition in a format nothing derives", tpl.Type)})
	}
	if tpl.Privileged {
		out = append(out, Violation{source, RulePrivileged, "the template asks Portainer to run the container privileged"})
	}
	for _, v := range tpl.Volumes {
		if hostPathIsAt(v.Bind, "/var/run/docker.sock") || hostPathIsAt(v.Bind, "/run/docker.sock") {
			out = append(out, Violation{source, RuleProhibitedHostPath,
				fmt.Sprintf("the template binds %s: Portainer holds the Docker socket because that is what Portainer is, and backup-manager must never inherit it", v.Bind)})
		}
	}
	if tpl.Image != "" {
		out = append(out, Violation{source, RulePortainerTemplate,
			fmt.Sprintf("names an image (%q) in the template itself; a stack template's image comes from the stack file, and a second place to write it is a second place for it to drift", tpl.Image)})
	}
	if tpl.Repository.Stackfile != stackfile {
		out = append(out, Violation{source, RulePortainerTemplate,
			fmt.Sprintf("points repository.stackfile at %q, and the stack this adapter ships is %q", tpl.Repository.Stackfile, stackfile)})
	}
	if tpl.Repository.URL == "" {
		out = append(out, Violation{source, RulePortainerTemplate, "declares no repository.url, so Portainer has nothing to clone the stack file out of"})
	}
	for _, field := range []struct{ name, value string }{
		{"title", tpl.Title},
		{"name", tpl.Name},
		{"description", tpl.Description},
		{"logo", tpl.Logo},
		{"platform", tpl.Platform},
	} {
		if strings.TrimSpace(field.value) == "" {
			out = append(out, Violation{source, RulePortainerTemplate, fmt.Sprintf("declares no %s, which Portainer's own catalogue needs to render the entry", field.name)})
		}
	}
	if len(tpl.Categories) == 0 {
		out = append(out, Violation{source, RulePortainerTemplate, "declares no categories, so the entry is unfindable in Portainer's catalogue"})
	}

	declared := map[string]string{}
	for _, e := range tpl.Env {
		if _, dup := declared[e.Name]; dup {
			out = append(out, Violation{source, RulePortainerTemplate, fmt.Sprintf("declares %s twice", e.Name)})
		}
		declared[e.Name] = e.Default
		if strings.TrimSpace(e.Label) == "" {
			out = append(out, Violation{source, RulePortainerTemplate, fmt.Sprintf("%s has no label, so Portainer shows an operator a blank form field", e.Name)})
		}
	}

	for _, name := range composeVars {
		want, inEnvFile := env[name]
		got, offered := declared[name]
		switch {
		case !offered:
			out = append(out, Violation{source, RulePortainerTemplate,
				fmt.Sprintf("the stack reads %s and the template never offers it, so an operator is never asked for it", name)})
		case inEnvFile && got != want:
			out = append(out, Violation{source, RulePortainerTemplate,
				fmt.Sprintf("offers %s defaulting to %q, and compose/backup-manager.env declares %q", name, got, want)})
		}
	}
	for name := range declared {
		if !contains(composeVars, name) {
			out = append(out, Violation{source, RulePortainerTemplate,
				fmt.Sprintf("offers %s and the stack reads it nowhere: a form field that changes nothing is worse than no field", name)})
		}
	}

	sortViolations(out)
	return out
}

// productPluginMarkers are the fingerprints of the thing #170 forbids by
// name. Each one has no innocent reading inside a distribution adapter
// whose entire deliverable is a template and a compose file.
//
// Note what is deliberately absent. Prose in a README explaining that no
// plugin is required is documentation, and this scan skips Markdown for
// that reason; and the word "portainer" on its own is the platform's
// name, not a dependency on its API.
var productPluginMarkers = []struct {
	marker string
	detail string
}{
	{"/api/endpoints", "calls the Portainer API"},
	{"portainer-ce/api", "imports the Portainer API"},
	{"portainer/agent", "deploys the Portainer agent, which is a second privileged component this product does not need"},
	{"PORTAINER_API_KEY", "authenticates against the Portainer API, which makes Portainer a runtime dependency of the backup manager"},
	{"X-API-Key", "authenticates against a management API"},
	{"dockge/plugin", "ships a Dockge plugin"},
}

// The Docker socket is deliberately NOT in the list above, and the reason
// is the one that decides whether a check survives. It is already caught
// twice, structurally, on the shape rather than on the spelling:
// CheckMountedHostPaths runs the prohibition over the Service every
// metadata format reduces to, and distribution/compose runs the runtime
// contract's own `docker-socket` rule over the canonical definition and
// every derived artifact. A third, textual copy adds nothing and costs
// something real: apps/portainer's compose file explains in a comment
// that Portainer holds /var/run/docker.sock and that this stack must
// never inherit it, which is exactly the sentence a reviewer needs and
// exactly the string a substring match cannot tell from a mount.

// ScanForProductPlugin reports any sign that an adapter has grown into a
// plugin, an agent or an API client for the platform it packages for.
//
// The match is case-insensitive, exactly as ScanForStoreMetadataLeak's
// already is, and for a stronger reason: `x-api-key` is the canonical
// HTTP/2 spelling of the header stored here as `X-API-Key` and the form
// most OpenAPI documents use, `portainer_api_key` is an ordinary shell
// env var, and `/API/endpoints` is a URL a server treats the same. A
// guard that fires only on the one spelling somebody wrote into both the
// rule and its fixture fails open on every other one. The widening costs
// nothing legitimate here: this runs over apps/<platform>/, whose whole
// deliverable is a template and a compose file, and it skips Markdown, so
// the prose that may say these words is out of scope either way. The
// message keeps the canonical spelling, because that is the one a reader
// can grep the rule for.
func ScanForProductPlugin(root string) ([]Violation, error) {
	return scanText(root, func(rel, text string) []Violation {
		if strings.EqualFold(filepath.Ext(rel), ".md") {
			return nil
		}
		lower := strings.ToLower(text)
		var out []Violation
		for _, m := range productPluginMarkers {
			if strings.Contains(lower, strings.ToLower(m.marker)) {
				out = append(out, Violation{rel, RuleProductPlugin,
					fmt.Sprintf("contains %s, which %s; #170 rules out a product plugin, an agent and an API dependency by name", backquote(m.marker), m.detail)})
			}
		}
		return out
	})
}

// ---------------------------------------------------------------------
// CasaOS and ZimaOS store metadata
// ---------------------------------------------------------------------

// CasaOSStore is the top-level `x-casaos` block, which is what a CasaOS
// or ZimaOS store reads to build the app tile and the install dialog.
type CasaOSStore struct {
	Architectures []string          `yaml:"architectures"`
	Main          string            `yaml:"main"`
	Author        string            `yaml:"author"`
	Developer     string            `yaml:"developer"`
	Category      string            `yaml:"category"`
	StoreAppID    string            `yaml:"store_app_id"`
	Icon          string            `yaml:"icon"`
	Scheme        string            `yaml:"scheme"`
	PortMap       string            `yaml:"port_map"`
	Index         string            `yaml:"index"`
	Title         map[string]string `yaml:"title"`
	Tagline       map[string]string `yaml:"tagline"`
	Description   map[string]string `yaml:"description"`
	Tips          struct {
		BeforeInstall map[string]string `yaml:"before_install"`
	} `yaml:"tips"`
}

// CasaOSService is the per-service `x-casaos` block: how the store
// presents this service's environment values, ports and volumes.
type CasaOSService struct {
	Envs []struct {
		Container   string            `yaml:"container"`
		Description map[string]string `yaml:"description"`
	} `yaml:"envs"`
	Ports []struct {
		Container   string            `yaml:"container"`
		Description map[string]string `yaml:"description"`
	} `yaml:"ports"`
	Volumes []struct {
		Container   string            `yaml:"container"`
		Description map[string]string `yaml:"description"`
	} `yaml:"volumes"`
}

// CasaOSMetadata is one store compose file's metadata, top level and per
// service.
type CasaOSMetadata struct {
	Store    CasaOSStore
	Services map[string]CasaOSService
}

// ReadCasaOSMetadata pulls the `x-casaos` blocks out of a store compose
// file.
func ReadCasaOSMetadata(path string) (CasaOSMetadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return CasaOSMetadata{}, err
	}
	return ParseCasaOSMetadata(data, filepath.Base(path))
}

// ParseCasaOSMetadata is ReadCasaOSMetadata over bytes.
func ParseCasaOSMetadata(data []byte, source string) (CasaOSMetadata, error) {
	var doc struct {
		Store    CasaOSStore `yaml:"x-casaos"`
		Services map[string]struct {
			Store CasaOSService `yaml:"x-casaos"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return CasaOSMetadata{}, fmt.Errorf("%s: %w", source, err)
	}
	out := CasaOSMetadata{Store: doc.Store, Services: map[string]CasaOSService{}}
	for name, svc := range doc.Services {
		out.Services[name] = svc.Store
	}
	return out, nil
}

// CheckCasaOSMetadata holds a store submission to what its own store
// needs and to the services sitting beside it in the same file.
//
// The two halves matter for different reasons. The first is store-build
// validation: a submission missing an icon, a title or a category is
// rejected by the store, and finding that out at submission time is
// finding it out from the wrong system. The second is the one a store
// cannot check for us: `x-casaos` is presentation for the runtime
// underneath it, so a volume the store does not describe is a mount an
// operator is never shown and a port_map that disagrees with the
// published port is an app tile that opens nothing.
func CheckCasaOSMetadata(source string, md CasaOSMetadata, runtime AdapterRuntime, c Canonical) []Violation {
	var out []Violation
	store := md.Store

	for _, field := range []struct{ name, value string }{
		{"main", store.Main},
		{"author", store.Author},
		{"developer", store.Developer},
		{"category", store.Category},
		{"store_app_id", store.StoreAppID},
		{"icon", store.Icon},
		{"scheme", store.Scheme},
		{"index", store.Index},
		{"port_map", store.PortMap},
	} {
		if strings.TrimSpace(field.value) == "" {
			out = append(out, Violation{source, RuleStoreMetadata,
				fmt.Sprintf("x-casaos declares no %s, and the store build requires it", field.name)})
		}
	}
	for _, field := range []struct {
		name  string
		value map[string]string
	}{
		{"title", store.Title},
		{"tagline", store.Tagline},
		{"description", store.Description},
		{"tips.before_install", store.Tips.BeforeInstall},
	} {
		if strings.TrimSpace(field.value["en_us"]) == "" {
			out = append(out, Violation{source, RuleStoreMetadata,
				fmt.Sprintf("x-casaos declares no %s.en_us", field.name)})
		}
	}

	if len(store.Architectures) == 0 {
		out = append(out, Violation{source, RuleStoreMetadata,
			"x-casaos claims no architectures, so the store cannot tell an arm64 box that this app runs on it"})
	}
	claimed := append([]string(nil), store.Architectures...)
	built := append([]string(nil), c.Architectures...)
	sort.Strings(claimed)
	sort.Strings(built)
	if len(store.Architectures) > 0 && !equalStrings(claimed, built) {
		out = append(out, Violation{source, RuleStoreMetadata,
			fmt.Sprintf("x-casaos claims architectures %v and the release builds %v", claimed, built)})
	}

	if want := CanonicalListenPort(c); store.PortMap != "" && store.PortMap != want {
		out = append(out, Violation{source, RuleStoreMetadata,
			fmt.Sprintf("x-casaos port_map is %q and the canonical listen port is %s, so the app tile opens a port nothing serves", store.PortMap, want)})
	}

	// `main` names the service whose port the tile opens, so it has to be
	// the Web UI. Naming the engine would put the state database and the
	// credentials behind the tile.
	if runtime.WebUI != nil && store.Main != "" && store.Main != runtime.WebUI.Name {
		detail := fmt.Sprintf("x-casaos main is %q and the Web UI service is %q", store.Main, runtime.WebUI.Name)
		if runtime.Engine != nil && store.Main == runtime.Engine.Name {
			detail += "; that is the engine, which holds the state database and the credentials and publishes no port at all"
		}
		out = append(out, Violation{source, RuleStoreMetadata, detail})
	}

	for _, svc := range []*Service{runtime.Engine, runtime.WebUI} {
		if svc == nil {
			continue
		}
		presentation, ok := md.Services[svc.Name]
		if !ok {
			out = append(out, Violation{source, RuleStoreMetadata,
				fmt.Sprintf("service %q has no x-casaos block, so the store shows an operator none of its mounts or ports", svc.Name)})
			continue
		}
		described := map[string]bool{}
		for _, v := range presentation.Volumes {
			described[v.Container] = true
			if strings.TrimSpace(v.Description["en_us"]) == "" {
				out = append(out, Violation{source, RuleStoreMetadata,
					fmt.Sprintf("service %q describes volume %s with no en_us text", svc.Name, v.Container)})
			}
		}
		for _, m := range svc.Mounts {
			if !described[m.ContainerPath] {
				out = append(out, Violation{source, RuleStoreMetadata,
					fmt.Sprintf("service %q mounts %s and x-casaos describes no such volume, so the install dialog hides a mount the operator has to have created", svc.Name, m.ContainerPath)})
			}
		}
		shown := map[string]bool{}
		for _, p := range presentation.Ports {
			shown[p.Container] = true
		}
		for _, spec := range svc.Ports {
			parts := strings.Split(spec, ":")
			if !shown[parts[len(parts)-1]] {
				out = append(out, Violation{source, RuleStoreMetadata,
					fmt.Sprintf("service %q publishes %s and x-casaos describes no such port", svc.Name, spec)})
			}
		}
	}

	sortViolations(out)
	return out
}

// storeMetadataMarkers are the store and manager concerns that must never
// reach the provider-neutral core or the shared UI. #170 states the
// CasaOS half as an acceptance criterion in exactly these words: "no
// CasaOS or ZimaOS import appears in either".
var storeMetadataMarkers = []string{
	"x-casaos",
	"casaos",
	"zimaos",
	"store_app_id",
	"portainer",
	"dockge",
}

// ScanForStoreMetadataLeak reports any of the four new platforms' store
// or manager concerns appearing under a tree that is supposed to be
// provider-neutral.
//
// The match is case-insensitive and substring-based, which is blunt on
// purpose: this runs over core/ and ui/shared/src/, where none of these
// words has any legitimate use at all, so there is no false positive to
// trade against.
func ScanForStoreMetadataLeak(root string) ([]Violation, error) {
	var out []Violation
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "node_modules" || d.Name() == "dist" {
				return fs.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".go", ".ts", ".tsx", ".js", ".json", ".yaml", ".yml":
		default:
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		lower := strings.ToLower(string(data))
		for _, marker := range storeMetadataMarkers {
			if strings.Contains(lower, marker) {
				out = append(out, Violation{rel, RuleStoreMetadataLeak,
					fmt.Sprintf("mentions %s; store and manager metadata is the distribution adapter's and reaches neither the Go core nor the shared UI", backquote(marker))})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortViolations(out)
	return out, nil
}

// ---------------------------------------------------------------------
// Dockge: the adapter that is deliberately not a package
// ---------------------------------------------------------------------

// runtimeDefinitionExtensions are the file types a runtime definition
// arrives in. An adapter supported by compatibility rather than by
// packaging must contain none of them.
var runtimeDefinitionExtensions = map[string]bool{
	".yaml": true,
	".yml":  true,
	".xml":  true,
	".json": true,
	".env":  true,
}

// ScanForForkedStack reports any runtime definition inside an adapter
// whose whole support model is that it does not have one.
//
// Dockge is that adapter. Its integration surface is a directory holding
// a compose file, and container/compose.yaml already is one, so shipping
// a second copy here would create two definitions of the same stack for
// the same kind of host, which is the fork #170 rules out. A directory
// holding only prose is an unusual deliverable and an easy one to "fix"
// by helpfully adding a compose file, so the emptiness is a check rather
// than a convention.
func ScanForForkedStack(root string) ([]Violation, error) {
	var out []Violation
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		if runtimeDefinitionExtensions[strings.ToLower(filepath.Ext(path))] {
			out = append(out, Violation{rel, RuleForkedStack,
				"is a runtime definition, and this adapter deploys the canonical Compose stack itself. A second copy of that stack is the fork the support model rules out"})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortViolations(out)
	return out, nil
}
