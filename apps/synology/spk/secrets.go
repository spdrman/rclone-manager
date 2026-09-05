// §72's Phase 4 TDD Gate lists "no bundled secrets" as one of the
// properties provider conformance tests SHALL verify. This file is that
// check for the Synology package.
//
// It looks at two things: what a file is called, and what is written in
// it. Neither on its own is enough - a private key named notes.txt is
// still a private key, and an empty file called id_rsa is still a
// mistake worth refusing.
package spk

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// secretFileNames are basenames that should never appear in a package,
// whatever is in them.
var secretFileNames = map[string]bool{
	"id_rsa": true, "id_dsa": true, "id_ecdsa": true, "id_ed25519": true,
	".netrc": true, "netrc": true, ".npmrc": true, ".pgpass": true,
	"credentials": true, "secrets.yaml": true, "secrets.yml": true,
}

// secretFileExtensions are suffixes that carry key material by convention.
var secretFileExtensions = []string{".pem", ".key", ".p12", ".pfx", ".jks", ".keystore"}

// pemPrivateKey matches the armour every PEM private key carries.
var pemPrivateKey = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)

// assignedSecret matches a secret-shaped name assigned a value, in either
// shell/env (`NAME=value`) or YAML/JSON (`name: value`) form.
var assignedSecret = regexp.MustCompile(`(?i)\b([a-z0-9_.-]*(?:password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|credential)[a-z0-9_.-]*)\s*[:=]\s*("?)([^\s"',]+)`)

// placeholderValues are the values a secret-shaped key is ALLOWED to have:
// documentation and templates legitimately name these keys, and refusing
// every mention of the word "password" would make the check unusable and
// therefore ignored.
var placeholderValues = map[string]bool{
	"": true, "null": true, "nil": true, "none": true, "~": true,
	"changeme": true, "change_me": true, "replace_me": true, "replaceme": true,
	"example": true, "your_password_here": true, "xxx": true, "todo": true,
	"true": true, "false": true, "yes": true, "no": true,
}

// scanForSecrets returns one finding per credential-shaped thing in the
// named file.
func scanForSecrets(name string, body []byte) []string {
	var findings []string

	base := path.Base(name)
	if secretFileNames[base] {
		findings = append(findings, fmt.Sprintf("%s: %q is a credential filename; no package should carry one", name, base))
	}
	for _, ext := range secretFileExtensions {
		if strings.HasSuffix(strings.ToLower(base), ext) {
			findings = append(findings, fmt.Sprintf("%s: %q carries key material by convention", name, ext))
		}
	}

	// The two release binaries are the only non-text members, and their
	// bytes are already pinned by hash parity against the release
	// manifest. Running a text scan over compiled code produces noise, not
	// findings, so binary content is skipped - deliberately, and only
	// after the filename rules above have already applied to it.
	if !looksLikeText(body) {
		return findings
	}

	text := string(body)
	if loc := pemPrivateKey.FindString(text); loc != "" {
		findings = append(findings, fmt.Sprintf("%s: contains %q", name, loc))
	}
	for _, m := range assignedSecret.FindAllStringSubmatch(text, -1) {
		key, value := m[1], strings.TrimSpace(m[3])
		if placeholderValues[strings.ToLower(value)] || strings.HasPrefix(value, "$") || strings.HasPrefix(value, "<") {
			continue
		}
		// A filesystem path is a pointer to a credential, not a
		// credential. Every deployment shape this project ships names one
		// (key_file, known_hosts, --auth-store), and refusing them would
		// mean the package could not document where to put a key.
		if strings.HasPrefix(value, "/") || strings.HasPrefix(value, "./") {
			continue
		}
		findings = append(findings, fmt.Sprintf("%s: %s is assigned a literal value", name, key))
	}
	return findings
}

// looksLikeText reports whether content is plausibly text, judged the way
// diff and grep judge it: a NUL byte in the first few kilobytes means no.
func looksLikeText(body []byte) bool {
	head := body
	if len(head) > 8192 {
		head = head[:8192]
	}
	for _, b := range head {
		if b == 0 {
			return false
		}
	}
	return true
}
