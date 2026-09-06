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

// The rules that are about one named platform rather than about
// packaging in general, plus the readers that make a TrueNAS catalog
// entry checkable at all.
//
// scan.go asks what a provider package IS. This file asks two questions
// that only make sense once you know which platform you are looking at:
// has this provider grown its own authentication, and has the OMV
// adapter quietly become the native Workbench plugin v1 said it would
// not be. Both are deferrals rather than impossibilities, and a deferral
// decays silently, so each one is written as something that goes red.
//
// The TrueNAS half is here for a different reason. A catalog entry is
// three files that only mean something together: questions.yaml asks the
// operator for values, the template reads them, ix_values.yaml supplies
// the defaults for the answers nobody changes. Checking them separately
// proves nothing about the artifact a store install actually produces,
// so the readers below reduce all three to one rendered document, which
// the ordinary packaging rules can then read like any other Compose
// file. The renderer understands one expression form and refuses
// anything else on purpose: the template's own header promises to stay
// loop-free and conditional-free precisely so this is possible, and a
// renderer that grew to match a cleverer template would be re-deriving
// what the install does rather than reading it.

// The two rule identifiers this file adds to scan.go's set. Named, like
// those, so a positive control can assert that this rule fired and not
// merely that something did.
const (
	RuleBespokeAuth = "bespoke-auth-mechanism"
	RuleOMVPlugin   = "native-omv-plugin"
)

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

// scanText is the walk both content rules above share: skip npm's and the
// build's output, skip binaries, hand every remaining file's text to
// check, and sort so the report is diffable. It exists so a new
// text-level rule is a closure rather than a fourth copy of a WalkDir
// that could quietly disagree with the others about what it skips.
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

// questionNode is the recursive shape of a TrueNAS question. attrs and
// items are both children (one for objects, one for lists), and both are
// followed, because a variable nested inside a list item is addressed by
// the template exactly like any other and skipping it would make the
// coverage check quietly incomplete.
type questionNode struct {
	Variable string `yaml:"variable"`
	Schema   struct {
		Type  string         `yaml:"type"`
		Attrs []questionNode `yaml:"attrs"`
		Items []questionNode `yaml:"items"`
	} `yaml:"schema"`
}

// leaves flattens a question tree to the dotted paths a template can
// actually read. Only leaves: an intermediate node is a grouping in the
// installer's form and holds no value, so counting it would make the
// template look as though it ignores a question that was never askable.
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

// YAMLValue resolves a dotted path inside a YAML document and returns the
// scalar it holds. LookupYAMLPath can only answer "a key exists there",
// which is enough to prove a question has some default and not enough to
// prove the default is the canonical one.
func YAMLValue(path, dotted string) (string, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", false, fmt.Errorf("%s: %w", path, err)
	}
	v, ok := lookupYAMLValue(doc, dotted)
	if !ok {
		return "", false, nil
	}
	switch v.(type) {
	case map[string]any, []any, nil:
		return "", false, fmt.Errorf("%s: %s is not a scalar", path, dotted)
	}
	return fmt.Sprintf("%v", v), true, nil
}

// lookupYAMLValue is the shared descent behind LookupYAMLPath, YAMLValue
// and the renderer. It returns whatever it lands on, scalar or not, and
// leaves the "is that a usable value" judgement to the caller, because
// the three callers answer it differently: existence is enough for one,
// the other two need a scalar and report a map or a list as a missing
// default rather than stringifying it into the output.
func lookupYAMLValue(doc any, dotted string) (any, bool) {
	cur := doc
	for _, part := range strings.Split(dotted, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

var truenasTemplateExprRe = regexp.MustCompile(`\{\{\s*\.Values\.([A-Za-z0-9_]+(?:\.[A-Za-z0-9_]+)*)\s*\}\}`)

// RenderTrueNASCatalogTemplate substitutes a catalog template's
// `.Values.<path>` expressions with the defaults in ix_values.yaml, which
// is what a TrueNAS install renders when the operator changes no answer.
//
// This is a literal substitution and nothing more, which is exactly as
// much as the template allows: its own header states it is loop-free and
// conditional-free on purpose, so that the artifact an app-store install
// gets is the artifact the packaging rules can read. Without this, the
// catalog entry, which is the deliverable, is checked for existence and
// for question/template agreement and for nothing else: not its image, not
// its host paths, not its ports, not its hardening.
func RenderTrueNASCatalogTemplate(templatePath, valuesPath string) (string, error) {
	tpl, err := os.ReadFile(templatePath)
	if err != nil {
		return "", err
	}
	values, err := os.ReadFile(valuesPath)
	if err != nil {
		return "", err
	}
	var doc any
	if err := yaml.Unmarshal(values, &doc); err != nil {
		return "", fmt.Errorf("%s: %w", valuesPath, err)
	}

	var missing []string
	out := truenasTemplateExprRe.ReplaceAllStringFunc(string(tpl), func(m string) string {
		dotted := truenasTemplateExprRe.FindStringSubmatch(m)[1]
		v, ok := lookupYAMLValue(doc, dotted)
		if !ok {
			missing = append(missing, dotted)
			return m
		}
		switch v.(type) {
		case map[string]any, []any, nil:
			missing = append(missing, dotted)
			return m
		}
		return fmt.Sprintf("%v", v)
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("%s: no scalar default in %s for %s", templatePath, valuesPath, strings.Join(missing, ", "))
	}
	if strings.Contains(out, "{{") {
		return "", fmt.Errorf("%s: still holds a template expression after rendering; this renderer only understands `{{ .Values.<path> }}`, and the template is required to stay that simple", templatePath)
	}
	return out, nil
}
