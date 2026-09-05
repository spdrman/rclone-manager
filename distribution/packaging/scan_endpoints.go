package packaging

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Issue #264's third acceptance criterion, as something that can fail.
//
// The two sources this product exists to pull from are real machines on
// real addresses, listening on an SSH port that is deliberately not
// published. The criterion says none of that reaches this repository. A
// rule like that is easy to state and impossible to keep by hand: it is
// broken by a hostname pasted into a doc example, a port hardcoded into
// a fixture so a test would go green, a key path left in a default. Each
// of those is one commit by one person in a hurry, and nothing between
// that commit and a public repository looks at it.
//
// So this is a guard rather than a promise. It sweeps every tracked file
// for the SHAPES that carry an endpoint or key material, and it fails on
// the shape. It contains no production value and it never will: a guard
// written as "does the tree contain <the real port>" would put the real
// port in the tree, in a file everyone can read, which is the exact
// failure it claims to prevent. Every positive control in
// scan_endpoints_test.go plants a made-up value in each shape and watches
// this go red, so the rules are held to being able to fail rather than
// trusted to.
//
// What it deliberately does NOT do is guess. Every discriminator here is
// a fact about the text (this address is in a reserved range, this name
// ends in a reserved TLD, this token is a filename with a line number)
// rather than a judgement about intent. A rule that has to be argued
// about is a rule that gets switched off.

const (
	// RuleCommittedPrivateKey is a private key that arrived as a FILE, or
	// a PEM block in something that is not a test.
	RuleCommittedPrivateKey = "committed-private-key"

	// RulePinnedEndpoint is a known_hosts line pinning a host key to a
	// routable endpoint: the host, the port and the key, all three, in
	// one committed line.
	RulePinnedEndpoint = "pinned-endpoint"

	// RuleRoutableSSHEndpoint is a routable host with a port next to
	// something that says SSH.
	RuleRoutableSSHEndpoint = "routable-ssh-endpoint"
)

var (
	// The header alone is not enough: apps/synology/spk/secrets.go and
	// scan.go in this package both carry it inside a regular expression,
	// correctly, and flagging those would be flagging the detectors. A
	// key that is really in the tree has a base64 BODY, so that is what
	// this asks for.
	pemHeaderRe = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	pemBodyRe   = regexp.MustCompile(`(?m)^[A-Za-z0-9+/=]{40,}$`)

	// A known_hosts entry: a host pattern, an algorithm, and a key. Not
	// anchored to the start of a line on purpose, because the same line
	// pasted into a Go string literal or a YAML value is the same leak and
	// anchoring would let it through.
	knownHostsLineRe = regexp.MustCompile(`(\S+)\s+(?:ssh-(?:rsa|ed25519|dss)|ecdsa-sha2-[a-z0-9-]+|sk-[a-z0-9@.-]+)\s+AAAA[A-Za-z0-9+/]{20,}`)

	// host:port, and the negative lookahead is doing real work: without
	// it `ghcr.io/spdrman/backup-manager:0.3.0` reads as a host on port 0
	// and every image reference in the tree becomes an endpoint. Go's
	// regexp has no lookahead, so the trailing character is captured and
	// checked in code instead.
	// (?s) so the trailing character can be the newline an endpoint at the
	// end of a line is followed by. Without it this regex simply does not
	// match there at all, which is a hole exactly the width of "the last
	// thing on the line", and that is where a YAML value lives.
	hostPortRe = regexp.MustCompile(`(?s)\[?([A-Za-z0-9_](?:[A-Za-z0-9_.-]*[A-Za-z0-9_])?)\]?:([0-9]{1,5})(.|$)`)

	// What makes a line SSH's business rather than anybody's. A routable
	// host with a port is not a finding on its own: this repository talks
	// to a registry, an S3 endpoint and a Web UI, all of which are
	// host-and-port and none of which is what #264 is about.
	sshContextRe = regexp.MustCompile(`(?i)\b(ssh|sftp|scp|known_hosts|knownhosts|authorized_keys|id_ed25519|id_rsa|IdentityFile|ssh-keyscan|host_key|hostkey)\b`)
)

// sshContextWindow is how far either side of a candidate endpoint the
// scan looks for something that says SSH. One line is too narrow (a YAML
// source block puts `host:` and `port:` on different lines from the word
// sftp) and a whole file is too wide (every file under
// core/internal/transport/rclone says ssh somewhere).
const sshContextWindow = 240

// reservedTLDs are last labels that cannot resolve on the public
// internet, so a name ending in one is not an endpoint anybody could
// leak. RFC 6761 and RFC 8375 name most of these; `internal` is what
// this repository's own placeholder hosts use.
var reservedTLDs = map[string]bool{
	"internal": true, "local": true, "localdomain": true, "localhost": true,
	"test": true, "invalid": true, "example": true, "home": true, "lan": true,
	"arpa": true, "onion": true, "alt": true, "intranet": true, "private": true,
	"corp": true, "domain": true, "host": true,
}

// sourceFileSuffixes are last labels that make a dotted token a filename
// rather than a hostname. This matters more than it sounds: a Go comment
// citing `s3.go:1514` is a dotted name followed by a colon and a number,
// which is indistinguishable from host:port without this list, and those
// citations are everywhere in this repository's transport package, which
// is also the package that says "ssh" most often.
var sourceFileSuffixes = map[string]bool{
	"go": true, "py": true, "ts": true, "tsx": true, "js": true, "mjs": true,
	"cjs": true, "sh": true, "bash": true, "md": true, "yaml": true, "yml": true,
	"json": true, "html": true, "htm": true, "css": true, "txt": true, "sum": true,
	"mod": true, "toml": true, "lock": true, "supp": true, "spec": true, "snap": true,
	"svg": true, "png": true, "xml": true, "sql": true, "tmpl": true, "conf": true,
	"cfg": true, "ini": true, "env": true, "log": true, "rs": true, "java": true,
	"c": true, "h": true, "cc": true, "cpp": true, "hpp": true, "tf": true,
	"service": true, "timer": true, "socket": true, "sample": true, "dockerfile": true,
	"gitignore": true, "patch": true, "diff": true, "csv": true, "pem": true,
}

// publicResolvers are addresses that are public, routable, and not an
// endpoint anybody has anything to hide about. The installer opens TCP to
// 1.1.1.1:443 to ask whether a bridged container can originate traffic at
// all, which is a documented constant in a help string, not a source.
var publicResolvers = map[string]bool{
	"1.1.1.1": true, "1.0.0.1": true, "8.8.8.8": true, "8.8.4.4": true,
	"9.9.9.9": true, "149.112.112.112": true,
}

// infrastructureDomains are the services this repository is built with
// and against. They are public names by definition, sitting in a go.mod,
// a Dockerfile or a licence URL, so a scan that reported them would report
// a hundred true statements and nothing anybody needs.
//
// Every entry here was added because it actually fired, not in advance:
// an allowlist written by imagining what might match is an allowlist
// that hides what really does.
var infrastructureDomains = []string{
	"github.com", "githubusercontent.com", "ghcr.io", "docker.io", "docker.com",
	"golang.org", "go.dev", "googleapis.com", "gstatic.com", "rclone.org",
	"sqlite.org", "apache.org", "opensource.org", "spdx.org", "ietf.org",
	"rfc-editor.org", "debian.org", "ubuntu.com", "alpinelinux.org", "python.org",
	"w3.org", "mozilla.org", "npmjs.com", "npmjs.org", "jsdelivr.net",
	"opencontainers.org", "sigstore.dev", "slsa.dev", "letsencrypt.org",
	"cloudflare.com", "min.io", "amazonaws.com", "openssh.com", "openbsd.org",
}

// TrackedFiles is every path `git ls-files` reports under root.
//
// Tracked, not present. The criterion is about what reaches the
// repository, and a scan of the working tree would sweep build outputs,
// a local .env, node_modules and whatever an operator left lying about -
// none of which is committed, all of which would make this noisy enough
// to switch off.
func TrackedFiles(root string) ([]string, error) {
	cmd := exec.Command("git", "-C", root, "ls-files", "-z")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files in %s: %w", root, err)
	}
	var files []string
	for _, name := range bytes.Split(out, []byte{0}) {
		if len(name) > 0 {
			files = append(files, string(name))
		}
	}
	return files, nil
}

// ScanForLeakedEndpoints reports every production-shaped endpoint or key
// in the named files, which are relative to root.
//
// The file list is a parameter rather than a walk so the positive
// controls can point this at a directory of planted fixtures. A scan that
// could only ever run against the real tree could only ever be proven by
// its own green light, which is not a proof.
func ScanForLeakedEndpoints(root string, rel []string) ([]Violation, error) {
	var out []Violation
	for _, name := range rel {
		path := filepath.Join(root, name)
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				// A submodule or a path git lists that is not a regular
				// file. Nothing to read is nothing to leak.
				continue
			}
			return nil, err
		}
		if isBinary(data) {
			continue
		}
		out = append(out, scanOneForEndpoints(name, string(data))...)
	}
	return out, nil
}

// isTestSource says whether a path is a test, by the conventions this
// repository actually uses.
//
// Test sources are exempt from the PEM-block rule and from nothing else.
// The reason is specific rather than general: this tree deliberately
// carries generated throwaway keypairs as test constants, because a test
// that parses a private key needs one, and those are not a leak: they
// were made for the test and authorise nothing. What is never exempt is a
// key that arrived as a FILE, which is what wholeFileIsAPrivateKey below
// catches wherever it sits, testdata included.
func isTestSource(name string) bool {
	base := filepath.Base(name)
	switch {
	case strings.HasSuffix(base, "_test.go"),
		strings.HasSuffix(base, "_test.py"),
		strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"),
		strings.HasSuffix(base, ".test.ts"), strings.HasSuffix(base, ".test.tsx"),
		strings.HasSuffix(base, ".spec.ts"), strings.HasSuffix(base, ".spec.tsx"),
		strings.HasSuffix(base, ".test.sh"):
		return true
	}
	return isUnderDir(name, "testdata")
}

func scanOneForEndpoints(name, text string) []Violation {
	var out []Violation

	if wholeFileIsAPrivateKey(text) {
		out = append(out, Violation{name, RuleCommittedPrivateKey,
			"this file IS a private key, so it arrived by being copied in rather than generated for a test"})
	} else if !isTestSource(name) && pemHeaderRe.MatchString(text) && pemBodyRe.MatchString(text) {
		out = append(out, Violation{name, RuleCommittedPrivateKey,
			"carries a PEM private key block with a real body, outside a test"})
	}

	for _, m := range knownHostsLineRe.FindAllStringSubmatch(text, -1) {
		for _, pattern := range strings.Split(m[1], ",") {
			host, _, ok := splitPinnedPattern(pattern)
			if !ok || !routableHost(host) {
				continue
			}
			out = append(out, Violation{name, RulePinnedEndpoint,
				"pins a host key to a routable endpoint, which is a host, a port and a key in one committed line"})
			break
		}
	}

	for _, m := range hostPortRe.FindAllStringSubmatchIndex(text, -1) {
		host := text[m[2]:m[3]]
		portText := text[m[4]:m[5]]
		trailing := text[m[6]:m[7]]
		if trailing != "" && (trailing[0] >= '0' && trailing[0] <= '9' || trailing[0] == '.') {
			// A version tag, a timestamp, or a longer number. Not a port.
			continue
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			continue
		}
		if !routableHost(host) {
			continue
		}
		if !hasSSHContext(text, m[0], m[1]) {
			continue
		}
		out = append(out, Violation{name, RuleRoutableSSHEndpoint,
			"names a routable host and a port next to something that says SSH"})
	}
	return out
}

// wholeFileIsAPrivateKey is true for a file that holds a PEM private key
// and nothing else, whatever it is called and wherever it sits.
func wholeFileIsAPrivateKey(text string) bool {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "-----BEGIN ") || !pemHeaderRe.MatchString(trimmed) {
		return false
	}
	return strings.HasSuffix(trimmed, "PRIVATE KEY-----")
}

// splitPinnedPattern takes one known_hosts host pattern apart into a host
// and a port. `[host]:port` for a source that is not on 22, bare `host`
// for one that is.
func splitPinnedPattern(pattern string) (host string, port int, ok bool) {
	pattern = strings.Trim(strings.TrimSpace(pattern), `"'`+"`,")
	if pattern == "" {
		return "", 0, false
	}
	if strings.HasPrefix(pattern, "|") {
		// Hashed. Nothing is readable out of it, which is the point of
		// hashing it, so there is nothing here to report either.
		return "", 0, false
	}
	if strings.HasPrefix(pattern, "[") {
		end := strings.LastIndex(pattern, "]:")
		if end < 0 {
			return "", 0, false
		}
		n, err := strconv.Atoi(pattern[end+2:])
		if err != nil {
			return "", 0, false
		}
		return pattern[1:end], n, true
	}
	return pattern, 22, true
}

// hasSSHContext looks either side of a match for something that says this
// endpoint is reached over SSH.
func hasSSHContext(text string, start, end int) bool {
	from := start - sshContextWindow
	if from < 0 {
		from = 0
	}
	to := end + sshContextWindow
	if to > len(text) {
		to = len(text)
	}
	return sshContextRe.MatchString(text[from:to])
}

// routableHost is the whole discriminator, and every clause in it is a
// fact rather than an opinion.
func routableHost(host string) bool {
	host = strings.Trim(strings.TrimSpace(host), `"'`+"`[]<>(),;")
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return routableIP(ip)
	}
	lower := strings.ToLower(host)
	labels := strings.Split(lower, ".")
	if len(labels) < 2 {
		// A single label is a container alias, a compose service name or
		// a placeholder. It resolves nowhere off the machine that defines
		// it, so it cannot be the address of a production host. Both of
		// this issue's hosts are already named in this repository by their
		// single-label role names, deliberately.
		return false
	}
	last := labels[len(labels)-1]
	if reservedTLDs[last] || sourceFileSuffixes[last] {
		return false
	}
	if last == "" || !isAlphaOnly(last) || len(last) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" {
			return false
		}
	}
	if strings.Contains(lower, "example") || strings.Contains(lower, "placeholder") {
		return false
	}
	registrable := strings.Join(labels[len(labels)-2:], ".")
	for _, allowed := range infrastructureDomains {
		if registrable == allowed || lower == allowed {
			return false
		}
	}
	return true
}

func isAlphaOnly(s string) bool {
	for _, r := range s {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return len(s) > 0
}

// reservedNets are every IPv4 and IPv6 range that cannot be a production
// host on the public internet: the private ranges, loopback, link-local,
// carrier-grade NAT, the three RFC 5737 documentation ranges, RFC 3849's
// IPv6 documentation range, benchmarking, multicast and reserved.
var reservedNets = func() []*net.IPNet {
	cidrs := []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8",
		"169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24",
		"192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
		"224.0.0.0/4", "240.0.0.0/4",
		"::1/128", "fc00::/7", "fe80::/10", "2001:db8::/32", "ff00::/8",
	}
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("scan_endpoints: bad reserved CIDR " + c)
		}
		out = append(out, n)
	}
	return out
}()

func routableIP(ip net.IP) bool {
	if publicResolvers[ip.String()] {
		return false
	}
	for _, n := range reservedNets {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}
