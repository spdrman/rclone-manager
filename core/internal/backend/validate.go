package backend

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

// validateManifest is every refusal a manifest's own SHAPE can produce,
// checked at load time and independent of any instance's values. Load
// calls this once per file and accumulates every problem it returns
// rather than stopping at the first, the same "a config wrong in three
// places costs one restart" promise config.validator makes.
func validateManifest(file string, m Manifest) []error {
	var problems []error
	addf := func(format string, args ...any) {
		problems = append(problems, manifestErrorf(file, format, args...))
	}

	switch m.ID {
	case "":
		addf("declares no id")
	case ReservedInstanceID:
		addf("declares id %q, which is config.MediumLocal, the reserved id of the implicit local placement - a backend may never claim it, the same way no instance of any backend may (issue #665)", m.ID)
	}

	if !validRoles[m.Role] {
		addf("declares role %q, and the accepted roles are %s", m.Role, rolesList())
	}

	if !SupportedRcloneBackends[m.RcloneBackend] {
		addf("declares rclone_backend %q, and the accepted backends are %s", m.RcloneBackend, backendsList())
	}

	problems = append(problems, validateManifestFields(file, m)...)
	problems = append(problems, validateManifestProbe(file, m)...)
	problems = append(problems, validateManifestCapabilities(file, m)...)

	return problems
}

func rolesList() string {
	return `"object_store", "local_volume", "remote_filesystem"`
}

func backendsList() string {
	names := make([]string, 0, len(SupportedRcloneBackends))
	for name := range SupportedRcloneBackends {
		names = append(names, fmt.Sprintf("%q", name))
	}
	// Sorted for a deterministic message: SupportedRcloneBackends is a
	// map, and a message whose wording changes between two loads of the
	// same manifest would be a message a test could never pin.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j-1] > names[j]; j-- {
			names[j-1], names[j] = names[j], names[j-1]
		}
	}
	return strings.Join(names, ", ")
}

func validateManifestFields(file string, m Manifest) []error {
	var problems []error
	addf := func(format string, args ...any) {
		problems = append(problems, manifestErrorf(file, format, args...))
	}

	seen := map[string]bool{}
	credentialFields := 0
	for _, f := range m.Fields {
		path := fmt.Sprintf("fields[%s]", f.ID)
		if f.ID != "" {
			if seen[f.ID] {
				addf("declares field id %q more than once", f.ID)
			}
			seen[f.ID] = true
		} else {
			addf("declares a field with no id")
		}

		if !validFieldKinds[f.Kind] {
			addf("%s: kind %q is not one of the accepted field kinds", path, f.Kind)
			continue
		}

		switch f.Kind {
		case KindEnum:
			if len(f.Values) == 0 {
				addf("%s: kind is \"enum\" and declares no values", path)
			}
		default:
			if len(f.Values) > 0 {
				addf("%s: declares values, and only an \"enum\" field may", path)
			}
		}
		seenValue := map[string]bool{}
		for _, v := range f.Values {
			if seenValue[v.Value] {
				addf("%s: declares the value %q more than once", path, v.Value)
			}
			seenValue[v.Value] = true
		}

		if f.Pattern != "" {
			if f.Kind != KindString {
				addf("%s: declares a pattern, and only a \"string\" field may", path)
			} else if _, err := regexp.Compile(f.Pattern); err != nil {
				addf("%s: pattern %q does not compile: %v", path, f.Pattern, err)
			}
		}

		if f.UnsetMeans != "" {
			if f.Kind != KindEnum {
				addf("%s: declares unset_means, and only an \"enum\" field may", path)
			} else if !seenValue[f.UnsetMeans] {
				addf("%s: unset_means %q is not one of this field's declared values", path, f.UnsetMeans)
			}
		}

		if f.Kind == KindCredential {
			credentialFields++
			switch {
			case len(f.Values) > 0:
				addf("%s: a credential field declares nothing but that it is one, and this declares values too", path)
			case f.Pattern != "":
				addf("%s: a credential field declares nothing but that it is one, and this declares a pattern too", path)
			case f.UnsetMeans != "":
				addf("%s: a credential field declares nothing but that it is one, and this declares unset_means too", path)
			}
		}
	}
	if credentialFields > 1 {
		addf("declares %d credential fields, and a manifest may declare at most one", credentialFields)
	}
	return problems
}

func validateManifestProbe(file string, m Manifest) []error {
	var problems []error
	addf := func(format string, args ...any) {
		problems = append(problems, manifestErrorf(file, format, args...))
	}

	steps := m.Probe.Steps
	if len(steps) != len(ProbeStepNames) {
		addf("probe declares %d step(s), and the vocabulary has exactly %d: %s", len(steps), len(ProbeStepNames), strings.Join(ProbeStepNames, ", "))
		return problems
	}
	for i, want := range ProbeStepNames {
		got := steps[i]
		if got.Step != want {
			addf("probe step %d is %q, and the fixed order requires %q here: %s", i, got.Step, want, strings.Join(ProbeStepNames, ", "))
			continue
		}
		switch {
		case got.Run && got.Reason != "":
			addf("probe step %q runs and also carries a reason, which is only for a step that does not run", got.Step)
		case !got.Run && got.Reason == "":
			addf("probe step %q does not run and carries no reason; a surface has to be able to tell an operator why", got.Step)
		}
	}
	return problems
}

// validateManifestCapabilities checks a DECLARED capability block. A
// manifest that declares none is legal and is refused later, by name, at
// the moment something asks to enumerate it (Manifest.PlanEnumeration):
// the registry's job is to refuse a document that is wrong, and a
// manifest that has not answered this yet is incomplete rather than
// wrong. capability.go's header has the whole argument.
//
// Once there IS a block, two rules. It answers the whole vocabulary,
// because a missing key reads as a capability nobody claimed and a
// consumer would have to guess which of those two it was. And every
// value is in its own closed set, because "sometimes" is the shape of a
// real mistake: an answer no consumer can branch on, in a document that
// otherwise looks complete.
func validateManifestCapabilities(file string, m Manifest) []error {
	if m.Capabilities == nil {
		return nil
	}
	var problems []error
	addf := func(format string, args ...any) {
		problems = append(problems, manifestErrorf(file, format, args...))
	}
	caps := *m.Capabilities

	if missing := caps.Undeclared(); len(missing) > 0 {
		names := make([]string, len(missing))
		for i, key := range missing {
			names[i] = string(key)
		}
		addf("declares capabilities and omits %s; the block answers the whole vocabulary or none of it, because a key nobody declared is one every consumer has to guess about",
			strings.Join(names, ", "))
	}

	if caps.Declares(CapMTimePrecision) && !validMTimePrecisions[caps.MTimePrecision] {
		addf("capabilities.mtime_precision is %q, and the accepted values are %s",
			caps.MTimePrecision, `"1ns", "1ms", "1s", "2s", "unknown"`)
	}
	if caps.Declares(CapSymlinkSemantics) && !validSymlinkSemantics[caps.SymlinkSemantics] {
		addf("capabilities.symlink_semantics is %q, and the accepted values are %s",
			caps.SymlinkSemantics, `"store", "follow", "skip", "unsupported"`)
	}
	if caps.Declares(CapMetadataSupport) && !validMetadataSupport[caps.MetadataSupport] {
		addf("capabilities.metadata_support is %q, and the accepted values are %s",
			caps.MetadataSupport, `"full", "partial", "none"`)
	}
	if caps.Declares(CapCaseSensitivity) && !validCaseSensitivity[caps.CaseSensitivity] {
		addf("capabilities.case_sensitivity is %q, and the accepted values are %s",
			caps.CaseSensitivity, `"sensitive", "insensitive", "preserving", "unknown"`)
	}

	// A hash this boundary cannot ask for is a claim nothing can act on.
	// The names are rclone's own (transport.SHA256 is the only algorithm
	// this product's own verification speaks; see transport.go), and the
	// list is sorted so two manifests declaring the same set are the same
	// document.
	seen := map[string]bool{}
	for i, name := range caps.HashSupport {
		switch {
		case name == "":
			addf("capabilities.hash_support[%d] is empty", i)
		case seen[name]:
			addf("capabilities.hash_support declares %q more than once", name)
		}
		seen[name] = true
	}
	for i := 1; i < len(caps.HashSupport); i++ {
		if caps.HashSupport[i-1] > caps.HashSupport[i] {
			addf("capabilities.hash_support is not sorted (%q before %q); a set declared in two orders is two documents saying one thing",
				caps.HashSupport[i-1], caps.HashSupport[i])
			break
		}
	}

	return problems
}

// ValidateInstance checks one instance's collected values against the
// manifest that declares them, and returns every problem rather than the
// first, because an operator filling in a form fixes them all at once.
//
// path is the caller's own prefix for a message ("storage_mediums[2]"),
// so a refusal reads in the caller's vocabulary rather than this
// package's. #665 ships this with no production caller; #667 makes
// config.Validate the first one (see doc.go).
//
// # No message ever echoes an operator-supplied VALUE
//
// Issue #665 section 1.6, written out literally, has the enum-mismatch
// and unknown-field messages quote the offending value (`%q is not one
// of: %s`, `%q is not a field of the %q backend`). This package does
// NOT do that, on purpose, and the deviation is deliberate rather than
// an oversight: C5 (section 3.3, "the error message - the one nobody
// tests") plants a credential-shaped canary in every position a message
// can be built from, including an enum value and an unknown field's
// key, and asserts none of them ever reach a message. An operator who
// pastes a real secret into the wrong field - the bucket box instead of
// the credential one, or a JSON body built by a caller with a bug in it
// - would otherwise get it echoed straight back in the refusal. The
// ground truth this package is built against already states the
// alternative in so many words (core/internal/transport/rclone/
// mediumcreds.go: MediumCredentials' "refusals by SHAPE, never by
// content"), and every message in this file now follows it, not only
// the ones section 1.6 already wrote that way (the credential and path
// rules). What every message still safely names: the field id, the
// backend id, and the SCHEMA's own values (declared field ids, declared
// enum choices) - none of that is anything an operator typed.
//
// A KindCredential field's value is a credential REFERENCE, never
// material: see doc.go. This function checks that the reference is
// present when the field is required and that it carries no path
// separator, and nothing else. Whether the reference resolves to a file
// this deployment minted is core/service's question
// (resolveMediumCredentialsFileIn), and whether the material behind it
// authenticates is the adapter's.
func (r *Registry) ValidateInstance(backendID, path string, values map[string]string) []error {
	m, err := r.Backend(backendID)
	if err != nil {
		return []error{err}
	}

	var problems []error
	addf := func(format string, args ...any) {
		problems = append(problems, manifestError(fmt.Sprintf(format, args...)))
	}

	declared := map[string]bool{}
	for _, f := range m.Fields {
		declared[f.ID] = true
	}
	for key := range values {
		if !declared[key] {
			// The unrecognized key is never quoted back (issue #665's
			// own section 1.6 literally specifies %q here; deviated
			// from on purpose, see this function's docblock).
			addf("%s: names a field the %q backend does not declare; declared fields: %s", path, backendID, declaredFieldsList(m))
		}
	}

	for _, f := range m.Fields {
		value, present := values[f.ID]
		if !present || value == "" {
			if f.Required {
				addf("%s: %s is required", path, f.ID)
			}
			// Unset optional field: no check runs, and UnsetMeans is
			// never substituted here (issue #294) - a caller resolves it
			// itself, the same way EffectiveStorageClass does.
			continue
		}
		problems = append(problems, validateFieldValue(path, f, value)...)
	}
	return problems
}

// validateFieldValue is one field's rule, exactly as issue #665 section
// 1.6 states it.
func validateFieldValue(path string, f Field, value string) []error {
	var problems []error
	addf := func(format string, args ...any) {
		problems = append(problems, manifestError(fmt.Sprintf(format, args...)))
	}

	switch f.Kind {
	case KindPath:
		if !filepath.IsAbs(value) {
			addf("%s: %s must be an absolute, already-clean path with no \".\" or \"..\" segment", path, f.ID)
			break
		}
		clean := filepath.Clean(value)
		bad := clean != value
		for _, seg := range strings.Split(value, "/") {
			if seg == "." || seg == ".." {
				bad = true
			}
		}
		if bad {
			addf("%s: %s must be an absolute, already-clean path with no \".\" or \"..\" segment", path, f.ID)
		}

	case KindURL:
		u, err := url.Parse(value)
		switch {
		case err != nil, u.Scheme != "http" && u.Scheme != "https", u.Host == "", u.User != nil:
			addf("%s: %s must be an http or https URL, and must not carry a username or password", path, f.ID)
		}

	case KindKeyPrefix:
		problems = append(problems, validateKeyPrefix(path, f.ID, value)...)

	case KindEnum:
		ok := false
		for _, v := range f.Values {
			if v.Value == value {
				ok = true
				break
			}
		}
		if !ok {
			addf("%s: %s is not one of the declared values: %s", path, f.ID, enumValuesList(f.Values))
		}

	case KindBool:
		if value != "true" && value != "false" {
			addf("%s: %s must be true or false", path, f.ID)
		}

	case KindString:
		if f.Pattern != "" {
			// validateManifest already refused a Pattern that does not
			// compile, so by the time an instance is checked this is
			// always the manifest's own already-proven regex.
			if re, err := regexp.Compile(f.Pattern); err == nil && !re.MatchString(value) {
				addf("%s: %s does not match the shape this backend accepts", path, f.ID)
			}
		}

	case KindCredential:
		if strings.ContainsAny(value, `/\`) {
			addf("%s: %s is not a credential this deployment minted", path, f.ID)
		}
	}
	return problems
}

// validateKeyPrefix is config.validateMediumPrefix's four rules
// (validate.go:1592-1610), reworded only to take the caller's path
// prefix: copied rather than reworded a second time, because an operator
// may already have read one of these sentences about a different field.
func validateKeyPrefix(path, fieldID, prefix string) []error {
	var problems []error
	if strings.HasPrefix(prefix, "/") || strings.HasSuffix(prefix, "/") {
		problems = append(problems, manifestError(fmt.Sprintf(
			"%s: %s must not start or end with \"/\"; the key layout joins it with \"/\" already, so a slash here produces an empty key segment",
			path, fieldID)))
		return problems
	}
	for _, seg := range strings.Split(prefix, "/") {
		switch seg {
		case "":
			problems = append(problems, manifestError(fmt.Sprintf(
				"%s: %s must not contain an empty segment (\"//\")", path, fieldID)))
			return problems
		case ".", "..":
			problems = append(problems, manifestError(fmt.Sprintf(
				"%s: %s must not contain a \".\" or \"..\" segment; a key namespace has no traversal to express, and a restore writes to a local path derived from the key",
				path, fieldID)))
			return problems
		}
	}
	return problems
}

func declaredFieldsList(m Manifest) string {
	names := make([]string, 0, len(m.Fields))
	for _, f := range m.Fields {
		names = append(names, f.ID)
	}
	return strings.Join(names, ", ")
}

func enumValuesList(values []EnumValue) string {
	names := make([]string, 0, len(values))
	for _, v := range values {
		names = append(names, v.Value)
	}
	return strings.Join(names, ", ")
}
