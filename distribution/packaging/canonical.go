// Package packaging holds the one description every container-native
// provider package shares, plus the checkers that hold those packages to
// it.
//
// Work Package 4.3 (docs/EPIC-B-multi-nas.md §72) puts TrueNAS, Unraid and
// OpenMediaVault in Tier B/C: none of them gets a lifecycle engine or a
// plugin, each just wraps the exact canonical OCI image with
// platform-specific metadata so it appears in that platform's own app
// store. Three platforms, four metadata formats (Compose YAML, a TrueNAS
// catalog app.yaml/questions.yaml pair, an Unraid Docker template XML, an
// OMV env file), all repeating the same image reference, the same
// container-side mount points, the same port and the same auth mode. That
// repetition is the drift risk WP4.3's REFACTOR step names, so this
// package holds the values once, in canonical.json, and its test suite
// fails the build when any platform's own metadata disagrees.
//
// It also implements the two Phase 4 TDD Gate checks that are structural
// rather than behavioural: "no bundled secrets" and "no provider-specific
// lifecycle implementation". See scan.go.
//
// Nothing here runs at runtime. This package is compiled into no binary;
// it exists so the packaging rules are executable rather than prose, in
// the same spirit as apps/common/tests (the cross-provider frontend
// conformance suite) and for the same reason that suite lives under
// apps/common rather than inside any one provider.
package packaging

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

//go:embed canonical.json
var canonicalJSON []byte

// Image is the canonical OCI image every provider package points at.
type Image struct {
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Reference  string `json:"reference"`
	// Published records whether Reference actually resolves anywhere yet.
	// It is false today and honestly so. The registry itself is settled,
	// ghcr.io, and Reference is the target every provider package
	// carries; what has not happened is a push to it, so the reference
	// resolves to nothing. container/release-manifest.json says the same
	// thing from the other side: its per-architecture registry_digest is
	// null for exactly as long as this is false, and
	// TestReleaseManifestRegistryDigestTracksTheCanonicalPublishFlag
	// fails if the two ever disagree. Every acceptance procedure's step 0
	// covers making the image resolvable in the meantime. Doing the push
	// is #88's work.
	Published bool `json:"published"`
}

// ContainerPaths are the mount points inside the container. Identical for
// every provider, because they are baked into the binaries' own defaults
// (apps/generic/cmd/backup-manager-web's defaultConfigPath and
// defaultAuthStorePath) rather than chosen per platform.
type ContainerPaths struct {
	State      string `json:"state"`
	Backups    string `json:"backups"`
	Config     string `json:"config"`
	SSHKey     string `json:"sshKey"`
	KnownHosts string `json:"knownHosts"`
}

// HostPaths are the platform's own default locations on the NAS
// filesystem. Unlike ContainerPaths these genuinely differ per platform,
// which is exactly why they are declared here once instead of being
// retyped into a compose file, a questions.yaml, a template XML, an env
// file, a README and an acceptance procedure.
type HostPaths struct {
	State      string `json:"state"`
	Backups    string `json:"backups"`
	Config     string `json:"config"`
	SSHKey     string `json:"sshKey"`
	KnownHosts string `json:"knownHosts"`
}

// Platform is one packaged target.
type Platform struct {
	DisplayName string `json:"displayName"`
	// Tier is the §4A support tier: "B" for a provider package/catalog
	// wrapper, "C" for a supported deployment profile.
	Tier string `json:"tier"`
	// StorageMount is the backup root: the one host directory retained
	// artifacts land in, and nothing else. It is not the app root, and it
	// is not a container path.
	//
	// The field had drifted into meaning all three at once, which is how
	// the §19.2 containment check became satisfiable two ways: read as an
	// app root it permits the backup root to sit anywhere beneath it,
	// including next to the SSH private key. One meaning, pinned to
	// HostPaths.Backups, is what makes that check bite. It is also what
	// the operator sees, because the shared UI shows this string and the
	// backup-set wizard seeds a destination from it, so a value that
	// includes the secrets directory proposes writing backups beside the
	// key material.
	//
	// It must equal what this platform's own frontend bridge declares in
	// apps/<platform>/frontend/platform.ts. That file and this one are
	// written by different work packages and read by different audiences,
	// so the test suite pins them together.
	StorageMount string    `json:"storageMount"`
	HostPaths    HostPaths `json:"hostPaths"`
	HostPathNote string    `json:"hostPathNote"`
	// TrustForwardedHeaders records whether this platform's engine
	// container may set TRUST_FORWARDED_HEADERS=true.
	//
	// apps/common/auth/local's contract is that the flag is safe only
	// where the Web UI container is the sole possible direct TCP peer "by
	// network topology, not merely by convention". A compose project
	// network satisfies that: it is created, named and torn down with the
	// deployment, and nothing else joins it. An operator-created,
	// durable, host-wide user-defined bridge does not, because every
	// container on it reaches every port of every other container on it,
	// so anything attached later, deliberately or by an unrelated app
	// reusing the name, becomes a direct peer of the engine and can
	// rotate X-Forwarded-For per request to defeat the login, enrollment
	// and password rate limiters.
	//
	// Where this is false the engine keys its rate limiters on the
	// container-network peer address, so every client shares one bucket.
	// That is over-limiting rather than no limiting, which is the
	// fail-safe direction and the same call the Synology package makes.
	TrustForwardedHeaders bool `json:"trustForwardedHeaders"`
	// TrustForwardedHeadersNote records why, in the file an operator or a
	// reviewer reads rather than in a commit message.
	TrustForwardedHeadersNote string `json:"trustForwardedHeadersNote"`
}

// Commands are the argv forms the canonical image supports. A provider
// package may select one of these; it may not invent another, and it may
// not wrap one in a shell (see scan.go).
type Commands struct {
	Engine      []string `json:"engine"`
	WebUI       []string `json:"webUI"`
	Headless    []string `json:"headless"`
	Healthcheck []string `json:"healthcheck"`
}

// Canonical is canonical.json.
type Canonical struct {
	Image                  Image               `json:"image"`
	Architectures          []string            `json:"architectures"`
	ListenPort             int                 `json:"listenPort"`
	AuthMode               string              `json:"authMode"`
	Commands               Commands            `json:"commands"`
	Binaries               []string            `json:"binaries"`
	ContainerPaths         ContainerPaths      `json:"containerPaths"`
	ReadOnlyContainerPaths []string            `json:"readOnlyContainerPaths"`
	Platforms              map[string]Platform `json:"platforms"`
}

// Load parses the embedded canonical.json.
func Load() (Canonical, error) {
	var c Canonical
	if err := json.Unmarshal(canonicalJSON, &c); err != nil {
		return Canonical{}, fmt.Errorf("packaging: parse canonical.json: %w", err)
	}
	return c, nil
}

// MustLoad is Load for callers that cannot proceed without it.
func MustLoad() Canonical {
	c, err := Load()
	if err != nil {
		panic(err)
	}
	return c
}

// RepoRoot is the repository root relative to this package's own
// directory, which is where `go test` runs. The checkers read metadata out
// of apps/<platform>/ directories and out of container/, neither of which
// is importable Go.
//
// Two levels, not three: this package moved from apps/common/packaging to
// distribution/packaging in #165, when the distribution layer became its
// own module rather than a subdirectory of the shared application one.
const RepoRoot = "../.."

// PlatformDir is apps/<name>/ relative to this package's directory.
func PlatformDir(name string) string {
	return filepath.Join(RepoRoot, "apps", name)
}

// ContainerPathFor returns the container-side mount point a host path is
// expected to land on, keyed the same way HostPaths is.
func (c ContainerPaths) ByRole(role string) (string, bool) {
	switch role {
	case "state":
		return c.State, true
	case "backups":
		return c.Backups, true
	case "config":
		return c.Config, true
	case "sshKey":
		return c.SSHKey, true
	case "knownHosts":
		return c.KnownHosts, true
	}
	return "", false
}

// ByRole is HostPaths' counterpart to ContainerPaths.ByRole.
func (h HostPaths) ByRole(role string) (string, bool) {
	switch role {
	case "state":
		return h.State, true
	case "backups":
		return h.Backups, true
	case "config":
		return h.Config, true
	case "sshKey":
		return h.SSHKey, true
	case "knownHosts":
		return h.KnownHosts, true
	}
	return "", false
}

// Roles is the fixed set of storage roles every platform maps, in a stable
// order so table-driven tests report deterministically.
var Roles = []string{"state", "backups", "config", "sshKey", "knownHosts"}

// Contains reports whether child is parent or sits underneath it, treating
// both as cleaned absolute-style POSIX paths. It is deliberately textual:
// these are paths on a NAS that does not exist on the machine running the
// test, so there is nothing to stat and no symlink to resolve. That makes
// it strictly weaker than core/internal/retention's real containment check
// (which canonicalises first, because a symlink inside a real backup root
// can point outside it); the two answer different questions, and this one
// only ever guards declared configuration, never a delete.
func Contains(parent, child string) bool {
	p := filepath.Clean(parent)
	c := filepath.Clean(child)
	if p == c {
		return true
	}
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return strings.HasPrefix(c, p)
}
