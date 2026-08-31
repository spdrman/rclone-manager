package packaging

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	RuleBespokeAuth = "bespoke-auth-mechanism"
	RuleOMVPlugin   = "native-omv-plugin"
)

// bespokeAuthMarkers are the fingerprints of a provider inventing its own
// authentication instead of using §13A's reusable local auth. WP4.3 is
// explicit: "All three use local-auth from the generic host; no platform
// gets its own auth mechanism."
// bespokeAuthRe matches the fingerprints of a provider inventing its own
// authentication instead of using §13A's reusable local auth. WP4.3 is
// explicit: "All three use local-auth from the generic host; no platform
// gets its own auth mechanism."
//
// The boundary is [^0-9A-Za-z] rather than \b because \b does not match
// between "c" and "_", so \boidc\b misses OIDC_ISSUER, which is precisely
// how a provider would spell it in a compose environment block. The
// positive control in scan_test.go caught that.
var bespokeAuthRe = regexp.MustCompile(`(?i)(?:^|[^0-9A-Za-z])(oidc|ldap|saml|sso|htpasswd|pam|basic[_-]?auth|auth[_-]mode)(?:[^0-9A-Za-z]|$)`)

// ScanForBespokeAuth reports any sign that a provider package wires
// authentication of its own.
//
// Markdown is skipped: a README explaining that this platform has no
// native SSO is documentation, not an implementation, and flagging it
// would be the kind of noise that gets a check switched off.
func ScanForBespokeAuth(root string) ([]Violation, error) {
	return scanText(root, func(rel, text string) []Violation {
		if strings.EqualFold(filepath.Ext(rel), ".md") {
			return nil
		}
		var out []Violation
		seen := map[string]bool{}
		for _, m := range bespokeAuthRe.FindAllStringSubmatch(text, -1) {
			marker := strings.ToLower(m[1])
			if seen[marker] {
				continue
			}
			seen[marker] = true
			out = append(out, Violation{rel, RuleBespokeAuth,
				fmt.Sprintf("mentions %s, but every Tier B/C provider uses the generic host's local auth (§13A)", backquote(marker))})
		}
		return out
	})
}

// omvPluginPathMarkers are directory or file names that only exist in a
// native OpenMediaVault plugin.
var omvPluginPathMarkers = []string{
	"debian", "salt", "workbench", "datamodel", "rpc", "mkconf", "omv-",
}

// omvPluginContentMarkers are strings that only appear in native OMV
// plugin sources.
var omvPluginContentMarkers = []*regexp.Regexp{
	regexp.MustCompile(`omv-mkconf`),
	regexp.MustCompile(`OMV\\+Rpc`),
	regexp.MustCompile(`openmediavault/workbench`),
	regexp.MustCompile(`omv_config`),
	regexp.MustCompile(`(?i)\bopenmediavault-[a-z0-9-]+\s*\(`),
}

// ScanForOMVPlugin enforces §4A's deferral of a native OMV Workbench
// plugin and WP4.3's "Do NOT implement a native OMV plugin in v1".
// Deferrals decay quietly, so this makes the decay a red test.
func ScanForOMVPlugin(root string) ([]Violation, error) {
	var out []Violation

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		if rel == "." {
			return nil
		}
		if d.IsDir() && (d.Name() == "node_modules" || d.Name() == "dist") {
			return fs.SkipDir
		}
		for _, marker := range omvPluginPathMarkers {
			for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
				if strings.EqualFold(part, marker) || (marker == "omv-" && strings.HasPrefix(strings.ToLower(part), "omv-")) {
					out = append(out, Violation{rel, RuleOMVPlugin,
						fmt.Sprintf("path component %q belongs to a native OMV plugin, which §4A defers", part)})
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	content, err := scanText(root, func(rel, text string) []Violation {
		if strings.EqualFold(filepath.Ext(rel), ".md") {
			return nil
		}
		var v []Violation
		for _, re := range omvPluginContentMarkers {
			if m := re.FindString(text); m != "" {
				v = append(v, Violation{rel, RuleOMVPlugin,
					fmt.Sprintf("contains %s, which belongs to a native OMV plugin", backquote(m))})
			}
		}
		return v
	})
	if err != nil {
		return nil, err
	}

	out = append(out, content...)
	sortViolations(out)
	return out, nil
}

func scanText(root string, check func(rel, text string) []Violation) ([]Violation, error) {
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
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if isBinary(data) {
			return nil
		}
		out = append(out, check(rel, string(data))...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sortViolations(out)
	return out, nil
}

// ---------------------------------------------------------------------
// TrueNAS catalog coherence
// ---------------------------------------------------------------------

// TrueNASQuestionVariables returns the dotted path of every leaf variable
// a TrueNAS questions.yaml asks for, e.g. "storage.state.hostPath". Dotted
// paths, not bare names, because that is how the catalog template
// addresses them and because a bare-name comparison would match by
// accident against unrelated text.
func TrueNASQuestionVariables(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Questions []questionNode `yaml:"questions"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	var out []string
	for _, q := range doc.Questions {
		out = append(out, q.leaves("")...)
	}
	return out, nil
}

type questionNode struct {
	Variable string `yaml:"variable"`
	Schema   struct {
		Type  string         `yaml:"type"`
		Attrs []questionNode `yaml:"attrs"`
		Items []questionNode `yaml:"items"`
	} `yaml:"schema"`
}

func (q questionNode) leaves(prefix string) []string {
	name := q.Variable
	if prefix != "" {
		name = prefix + "." + name
	}
	children := append(append([]questionNode{}, q.Schema.Attrs...), q.Schema.Items...)
	if len(children) == 0 {
		return []string{name}
	}
	var out []string
	for _, c := range children {
		out = append(out, c.leaves(name)...)
	}
	return out
}

var truenasValueRe = regexp.MustCompile(`\.Values\.([A-Za-z0-9_]+(?:\.[A-Za-z0-9_]+)*)`)

// TrueNASTemplateVariables returns every `.Values.<dotted.path>` a catalog
// template reads.
func TrueNASTemplateVariables(template string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range truenasValueRe.FindAllStringSubmatch(template, -1) {
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		out = append(out, m[1])
	}
	return out
}

// LookupYAMLPath resolves a dotted path inside a YAML document, so
// ix_values.yaml can be checked for a default for every question.
func LookupYAMLPath(path, dotted string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	cur := doc
	for _, part := range strings.Split(dotted, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return false, nil
		}
		cur, ok = m[part]
		if !ok {
			return false, nil
		}
	}
	return true, nil
}
