package packaging

import (
	"encoding/xml"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Violation is one thing a provider package did that WP4.3 forbids.
type Violation struct {
	// Path is relative to the scanned root, so failures read the same
	// whether the scan ran against a real apps/<platform>/ directory or
	// against a positive-control fixture in t.TempDir().
	Path   string
	Rule   string
	Detail string
}

func (v Violation) String() string {
	return fmt.Sprintf("%s: %s (%s)", v.Path, v.Rule, v.Detail)
}

// The rule identifiers. Named constants rather than inline strings so the
// positive controls can assert that a specific rule fired, not merely that
// "something" failed: a control that passes because an unrelated rule
// tripped proves nothing about the rule it claims to exercise.
const (
	RuleDisallowedFileType  = "disallowed-file-type"
	RuleExecutableBit       = "executable-bit"
	RuleShebang             = "shebang"
	RuleInlineShell         = "inline-shell"
	RuleEntrypointOverride  = "entrypoint-override"
	RuleBuildsOwnImage      = "builds-its-own-image"
	RuleLifecycleHook       = "lifecycle-hook"
	RuleNonCanonicalCommand = "non-canonical-command"
	RulePrivileged          = "privileged-container"
	RuleBundledSecret       = "bundled-secret"
)

// metadataExtensions is what a Tier B/C provider package may contain
// outside its frontend/ directory. The list is an allowlist on purpose: a
// denylist of "no .sh, no .go" is trivially defeated by a .pl, a .rb, or a
// file with no extension at all.
var metadataExtensions = map[string]bool{
	".yaml":    true,
	".yml":     true,
	".xml":     true,
	".json":    true,
	".md":      true,
	".svg":     true,
	".png":     true,
	".env":     true,
	".example": true,
	".txt":     true,
}

// frontendExtensions is what apps/<platform>/frontend/ may contain. That
// subtree is the shared platform bridge (§3.5) rather than packaging
// metadata: it is TypeScript by design, it holds no lifecycle behaviour,
// and apps/common/tests/providerConformance.test.ts already governs what
// it may do. Scanning it under the metadata allowlist would flag every
// bridge in the repository, so it gets its own list, and the list is still
// an allowlist: a .go file or a shell script under frontend/ is as much a
// violation there as anywhere else.
var frontendExtensions = map[string]bool{
	".ts":   true,
	".tsx":  true,
	".css":  true,
	".json": true,
	".svg":  true,
	".png":  true,
	".md":   true,
}

var allowedBareNames = map[string]bool{
	"LICENSE":    true,
	"NOTICE":     true,
	".gitignore": true,
}

// lifecycleKeys are keys whose mere presence means the package is trying
// to run something of its own around install, update or removal, which is
// the "no provider-specific lifecycle implementation" line WP4.3's gate
// draws.
var lifecycleKeys = map[string]bool{
	"lifecycle":        true,
	"hooks":            true,
	"pre_install":      true,
	"post_install":     true,
	"pre_upgrade":      true,
	"post_upgrade":     true,
	"pre_delete":       true,
	"post_delete":      true,
	"pre_uninstall":    true,
	"post_uninstall":   true,
	"install_script":   true,
	"uninstall_script": true,
	"update_script":    true,
}

var (
	inlineShellRe = regexp.MustCompile(`\b(?:ba|da|a|z|k)?sh\s+-[a-z]*c\b`)
	entrypointRe  = regexp.MustCompile(`--entrypoint\b`)

	privateKeyRe = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	publicKeyRe  = regexp.MustCompile(`\bssh-(?:rsa|ed25519|dss)\s+AAAA[0-9A-Za-z+/]{20,}`)
	// No \b before the key name on purpose: \b never matches between "_"
	// and "p", so \bpassword would miss ADMIN_PASSWORD, which is exactly
	// the shape a bundled credential takes in an env file. The positive
	// control in scan_test.go caught that; the leading [A-Za-z0-9_]* is
	// the fix.
	credentialRe = regexp.MustCompile(`(?i)[A-Za-z0-9_]*(password|passwd|secret|token|api[_-]?key|access[_-]?key)\s*[:=]\s*['"]?([^\s'"#$` + "`" + `{}<>]{8,})`)
)

// placeholderValues are credential-shaped values that are obviously not
// credentials. Without this, a template that says CHANGEME would be
// reported as a bundled secret, and the check would get switched off.
var placeholderValues = []string{
	"changeme", "change-me", "placeholder", "example", "your-", "yourpassword",
	"redacted", "generated", "xxxxxxxx", "notasecret",
}

// ScanLifecycle walks root and reports every way the tree under it steps
// beyond "metadata and templates".
//
// The scan is structural, not stylistic: it reads what the files ARE (an
// executable script, a Go source file, a compose service that builds its
// own image, a command wrapped in a shell) rather than what they say they
// are. That is what makes it able to fail. Its positive controls in
// scan_test.go run this exact function against fixtures that each contain
// one violation, so a refactor that quietly narrows a rule to nothing gets
// caught by the control rather than by a reviewer.
func ScanLifecycle(root string) ([]Violation, error) {
	var out []Violation

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// node_modules is npm's, not ours; dist is a build output.
			if d.Name() == "node_modules" || d.Name() == "dist" {
				return fs.SkipDir
			}
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		// Either the file sits under a frontend/ directory inside the
		// scanned tree, or the scanned tree IS one. The second case is
		// not hypothetical: a platform whose directory also holds
		// something other than container packaging is scanned root by
		// root (apps/synology, which carries the .spk builder as well),
		// and without this every bridge under such a root would be
		// reported as a disallowed file type in packaging metadata.
		inFrontend := isUnderDir(rel, "frontend") || filepath.Base(root) == "frontend"

		out = append(out, scanFileType(rel, path, d, inFrontend)...)

		// Content rules apply to machine-read metadata only. A README
		// legitimately shows `docker buildx build`, `ssh-keygen` and a
		// `sh -c` example inside a fenced block; flagging that would be
		// noise, and the file-type and secret rules still cover .md.
		ext := strings.ToLower(filepath.Ext(rel))
		if ext == ".md" {
			return nil
		}

		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if isBinary(data) {
			return nil
		}
		text := string(data)

		if strings.HasPrefix(text, "#!") {
			out = append(out, Violation{rel, RuleShebang, "starts with a shebang, so it is a script, not metadata"})
		}
		if m := inlineShellRe.FindString(text); m != "" {
			out = append(out, Violation{rel, RuleInlineShell, "contains " + backquote(m) + ", which is lifecycle logic smuggled into a template"})
		}
		if entrypointRe.MatchString(text) {
			out = append(out, Violation{rel, RuleEntrypointOverride, "overrides the image entrypoint"})
		}

		switch ext {
		case ".yaml", ".yml":
			v, yErr := scanYAML(rel, data)
			if yErr != nil {
				return yErr
			}
			out = append(out, v...)
		case ".xml":
			v, xErr := scanXML(rel, data)
			if xErr != nil {
				return xErr
			}
			out = append(out, v...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sortViolations(out)
	return out, nil
}

// ScanSecrets walks root looking for credential material a provider
// package must never carry. §13A: "provider packaging must not bake
// credentials into images", and the Phase 4 TDD Gate lists "no bundled
// secrets" as its own check.
func ScanSecrets(root string) ([]Violation, error) {
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
		text := string(data)

		if privateKeyRe.MatchString(text) {
			out = append(out, Violation{rel, RuleBundledSecret, "contains a PEM private-key header"})
		}
		if publicKeyRe.MatchString(text) {
			out = append(out, Violation{rel, RuleBundledSecret, "contains an SSH public key, which pins this package to one operator's key material"})
		}

		// The credential-assignment rule is skipped for .md: prose about
		// passwords is not a password, and the two sharp patterns above
		// still cover documentation.
		if strings.EqualFold(filepath.Ext(rel), ".md") {
			return nil
		}
		for _, m := range credentialRe.FindAllStringSubmatch(text, -1) {
			value := m[2]
			if isPlaceholder(value) {
				continue
			}
			out = append(out, Violation{rel, RuleBundledSecret,
				fmt.Sprintf("assigns a literal value to %q", strings.ToLower(m[1]))})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sortViolations(out)
	return out, nil
}

func scanFileType(rel, path string, d fs.DirEntry, inFrontend bool) []Violation {
	var out []Violation

	info, err := d.Info()
	if err == nil && info.Mode().Perm()&0o111 != 0 {
		out = append(out, Violation{rel, RuleExecutableBit,
			fmt.Sprintf("mode %s: a metadata file is never executable", info.Mode().Perm())})
	}

	base := filepath.Base(rel)
	if allowedBareNames[base] {
		return out
	}

	ext := strings.ToLower(filepath.Ext(base))
	allowed := metadataExtensions
	which := "packaging metadata"
	if inFrontend {
		allowed = frontendExtensions
		which = "a platform bridge"
	}
	if !allowed[ext] {
		out = append(out, Violation{rel, RuleDisallowedFileType,
			fmt.Sprintf("%q is not a file type %s may contain", ext, which)})
	}
	return out
}

func scanYAML(rel string, data []byte) ([]Violation, error) {
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	var out []Violation
	walkYAML(doc, func(key string, val any) {
		lower := strings.ToLower(key)
		switch {
		case lower == "build":
			out = append(out, Violation{rel, RuleBuildsOwnImage,
				"a `build:` key means this platform builds its own image instead of reusing the canonical one"})
		case lower == "entrypoint":
			out = append(out, Violation{rel, RuleEntrypointOverride,
				"overrides the image entrypoint"})
		case lower == "privileged":
			if b, ok := val.(bool); ok && b {
				out = append(out, Violation{rel, RulePrivileged, "runs privileged"})
			}
		case lifecycleKeys[lower]:
			out = append(out, Violation{rel, RuleLifecycleHook,
				fmt.Sprintf("`%s` runs provider-specific code around install/update/removal", key)})
		case lower == "command":
			out = append(out, checkCommand(rel, val)...)
		}
	})
	return out, nil
}

// checkCommand holds a compose `command:` to the canonical argv forms. The
// list form with a canonical binary first is the only shape allowed: a
// bare string is shell form, and a shell is where lifecycle logic hides.
func checkCommand(rel string, val any) []Violation {
	seq, ok := val.([]any)
	if !ok {
		return []Violation{{rel, RuleNonCanonicalCommand,
			"`command:` must be a list of arguments; a bare string is shell form"}}
	}
	argv := make([]string, 0, len(seq))
	for _, e := range seq {
		s, ok := e.(string)
		if !ok {
			return []Violation{{rel, RuleNonCanonicalCommand, "`command:` contains a non-string argument"}}
		}
		argv = append(argv, s)
	}
	return CheckArgv(rel, argv)
}

// shellMetacharacters are what turns an argument vector back into a
// script. The canonical image is distroless and has no shell at all, so
// any of these in a command line is either dead on arrival or evidence
// that the profile expects a shell it should not have.
var shellMetacharacters = []string{"&&", "||", ";", "|", "$(", "`", ">", "<"}

// CheckArgv holds one command line to the canonical image's binaries. It
// is shared between compose `command:` values and Unraid's <PostArgs>,
// which is Unraid's only seam for a container command (the image
// deliberately ships no ENTRYPOINT and no CMD, since no single default
// would be right for both of its binaries).
func CheckArgv(rel string, argv []string) []Violation {
	canonical := MustLoad()

	if len(argv) == 0 {
		return []Violation{{rel, RuleNonCanonicalCommand, "empty command"}}
	}
	if !contains(canonical.Binaries, argv[0]) {
		return []Violation{{rel, RuleNonCanonicalCommand,
			fmt.Sprintf("%q is not one of the canonical image's binaries %v", argv[0], canonical.Binaries)}}
	}
	for _, arg := range argv {
		for _, meta := range shellMetacharacters {
			if strings.Contains(arg, meta) {
				return []Violation{{rel, RuleInlineShell,
					fmt.Sprintf("argument %q contains %s, which needs a shell the distroless image does not have", arg, backquote(meta))}}
			}
		}
	}
	return nil
}

// xmlNode is a format-agnostic reader for an Unraid Docker template. The
// template schema has a fixed shape, but reading it generically means the
// scan also sees an element nobody anticipated, which is the point.
type xmlNode struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:",any,attr"`
	Content  string     `xml:",chardata"`
	Children []xmlNode  `xml:",any"`
}

func scanXML(rel string, data []byte) ([]Violation, error) {
	var root xmlNode
	if err := xml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("%s: %w", rel, err)
	}
	var out []Violation
	var walk func(n xmlNode)
	walk = func(n xmlNode) {
		name := strings.ToLower(n.XMLName.Local)
		text := strings.TrimSpace(n.Content)
		switch name {
		case "postargs":
			// <PostArgs> is everything Unraid appends after the image
			// name in `docker run`, which makes it that platform's only
			// way to supply a container command. The canonical image
			// ships no ENTRYPOINT and no CMD on purpose (no single
			// default is right for both of its binaries), so a template
			// with an empty <PostArgs> would not start at all. It is
			// therefore held to the same rule a compose `command:` is:
			// a canonical binary, and nothing a shell would be needed
			// for.
			if text != "" {
				out = append(out, CheckArgv(rel, strings.Fields(text))...)
			}
		case "privileged":
			if strings.EqualFold(text, "true") {
				out = append(out, Violation{rel, RulePrivileged, "<Privileged>true</Privileged>"})
			}
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(root)
	return out, nil
}

// walkYAML visits every mapping key in a decoded YAML document.
func walkYAML(node any, visit func(key string, val any)) {
	switch n := node.(type) {
	case map[string]any:
		keys := make([]string, 0, len(n))
		for k := range n {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			visit(k, n[k])
			walkYAML(n[k], visit)
		}
	case map[any]any:
		for k, v := range n {
			if ks, ok := k.(string); ok {
				visit(ks, v)
			}
			walkYAML(v, visit)
		}
	case []any:
		for _, v := range n {
			walkYAML(v, visit)
		}
	}
}

func isUnderDir(rel, dir string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for _, p := range parts[:max(0, len(parts)-1)] {
		if p == dir {
			return true
		}
	}
	return false
}

func isBinary(data []byte) bool {
	limit := min(len(data), 8000)
	for i := 0; i < limit; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}

func isPlaceholder(value string) bool {
	lower := strings.ToLower(value)
	for _, p := range placeholderValues {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func sortViolations(v []Violation) {
	sort.Slice(v, func(i, j int) bool {
		if v[i].Path != v[j].Path {
			return v[i].Path < v[j].Path
		}
		if v[i].Rule != v[j].Rule {
			return v[i].Rule < v[j].Rule
		}
		return v[i].Detail < v[j].Detail
	})
}

func backquote(s string) string { return "`" + s + "`" }
