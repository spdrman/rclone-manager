package packaging

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// This file answers the question every bridge-derived capability turns
// on, and that nothing could answer until now: does anything a user
// actually installs load apps/<provider>/frontend/platform.ts?
//
// Before #167 the answer was "no, for everybody but generic".
// ui/shared/vite.config.ts picks the shell at BUILD time from
// VITE_PLATFORM, and serve-ui served one go:embed'ed bundle with no way
// to serve another, so the canonical image and the .spk that wraps the
// same binaries all served the generic bridge. A flag set in a provider's
// platform.ts was a statement of repository intent, and the conformance
// matrix recorded those cells BLOCKED against #180 rather than reporting
// intent as a pass.
//
// #167 removed the constraint by making bundle selection a run-time
// decision (--ui-dir, then --ui-root/<profile>, then embedded, failing
// closed). It did not ship the packaging, and deliberately: seven bundles
// at roughly 347 KB each is about 2.4 MB against a gated 5% image budget
// of about 2.15 MB, so "put them all in the image" was never available.
//
// #169 ships it, per adapter, and there are exactly three mechanisms
// because there are exactly three kinds of carrier:
//
//	embedded        the bundle compiled into the binary. Correct for
//	                exactly one provider, whichever the release build
//	                names, and that is generic.
//	image-ui-root   the canonical image carries the bundle and the
//	                adapter selects it with UI_ROOT plus its profile.
//	                For an adapter that is metadata and nothing else: a
//	                catalog entry, a template, a compose profile. They
//	                have no file payload, so the image is their only
//	                carrier.
//	package-payload the package carries the bundle itself and points
//	                --ui-dir at it. For a package that installs native
//	                binaries and never pulls the image at all, which is
//	                the .spk.
//
// Each is decided from what is actually checked in, never from a
// declaration that it was done.

// UIBundleMarkerName is the file ui/shared/scripts/build-bundles.mjs
// writes into every bundle directory naming the provider it was built
// for.
const UIBundleMarkerName = "bundle.json"

// UIBundleMechanism is how one provider's shipped artifact gets its
// bridge.
type UIBundleMechanism string

const (
	UIBundleEmbedded       UIBundleMechanism = "embedded"
	UIBundleImageRoot      UIBundleMechanism = "image-ui-root"
	UIBundlePackagePayload UIBundleMechanism = "package-payload"
	// UIBundleNone: nothing this repository produces serves a UI for
	// this provider at all.
	UIBundleNone UIBundleMechanism = "none"
)

// UIBundleSelection is which bridge a provider's shipped artifact serves,
// and the evidence for it.
type UIBundleSelection struct {
	Mechanism UIBundleMechanism
	// Provider is whose bridge gets served. Empty when nothing decides.
	Provider string
	// Detail is the evidence, for a report a reader can check.
	Detail string
}

var (
	// dockerfileBundleBuildRe matches the frontend stage's per-provider
	// bundle build and captures its argument list.
	dockerfileBundleBuildRe = regexp.MustCompile(`npm run build:bundles([^\n]*)`)
	// dockerfileBundleCopyRe matches the runtime stage's COPY of the
	// built bundles and captures the destination root.
	dockerfileBundleCopyRe = regexp.MustCompile(`COPY --from=frontend-build\s+\S*/dist-bundles/?\s+(\S+)`)
	// dockerfileGenericBuildRe matches the plain single-bundle build
	// whose output is the one go:embed sees.
	dockerfileGenericBuildRe = regexp.MustCompile(`npm run build(?:\s|$)`)
)

// ImageBundleRoot reads container/Dockerfile for the per-provider bundles
// the canonical image carries and the path it carries them at.
//
// Reading the Dockerfile rather than trusting a declaration is the whole
// point: "the image carries the TrueNAS bundle" is exactly the kind of
// claim that stays in a JSON file long after the line that made it true
// was edited out.
func ImageBundleRoot() (root string, providers []string, err error) {
	data, readErr := os.ReadFile(Path(filepath.Join("container", "Dockerfile")))
	if readErr != nil {
		return "", nil, readErr
	}
	text := string(data)

	build := dockerfileBundleBuildRe.FindStringSubmatch(text)
	if build == nil {
		return "", nil, fmt.Errorf("container/Dockerfile never runs `npm run build:bundles`, so the image carries no per-provider bundle")
	}
	for _, f := range strings.Fields(build[1]) {
		if strings.HasPrefix(f, "-") || strings.Contains(f, "&&") {
			continue
		}
		providers = append(providers, f)
	}
	if len(providers) == 0 {
		return "", nil, fmt.Errorf("container/Dockerfile runs `npm run build:bundles` with no provider named, which builds every one of them and blows the image-size budget")
	}
	sort.Strings(providers)

	copyMatch := dockerfileBundleCopyRe.FindStringSubmatch(text)
	if copyMatch == nil {
		return "", nil, fmt.Errorf("container/Dockerfile builds per-provider bundles and never COPYs them into the runtime stage, so the image does not carry them")
	}
	root = strings.TrimSuffix(copyMatch[1], "/")
	return root, providers, nil
}

// GenericBundleIsBuiltLast reports whether the plain `npm run build`,
// whose dist/ is the bundle go:embed compiles into the binary, runs after
// the per-provider build.
//
// It has to. `build:bundles` runs `npm run build` once per provider and
// leaves dist/ holding whichever one it built last, so reversing the two
// lines ships a canonical image whose compiled-in UI is a NAS provider's
// bridge. Nothing downstream would notice: the image builds, the binary
// runs, and the generic deployment serves somebody else's shell.
func GenericBundleIsBuiltLast() (bool, string) {
	data, err := os.ReadFile(Path(filepath.Join("container", "Dockerfile")))
	if err != nil {
		return false, err.Error()
	}
	text := string(data)

	bundles := dockerfileBundleBuildRe.FindStringIndex(text)
	if bundles == nil {
		return false, "container/Dockerfile never runs `npm run build:bundles`"
	}
	// The generic build is the last plain `npm run build` that is not
	// the `build:bundles` one.
	last := -1
	for _, loc := range dockerfileGenericBuildRe.FindAllStringIndex(text, -1) {
		if loc[0] >= bundles[0] && loc[0] < bundles[1] {
			continue
		}
		if loc[0] > last {
			last = loc[0]
		}
	}
	if last < 0 {
		return false, "container/Dockerfile never runs a plain `npm run build`, so nothing produces the bundle compiled into the binary"
	}
	if last < bundles[0] {
		return false, "container/Dockerfile runs `npm run build` BEFORE `npm run build:bundles`, so dist/ ends up holding whichever provider the bundle build ran last, and that is the bundle compiled into the canonical binary"
	}
	return true, "the generic build runs after the per-provider bundles, so dist/ is generic"
}

// serveUIProfileRe pulls the profile out of a serve-ui command line.
var serveUIProfileRe = regexp.MustCompile(`--profile=?[ ]?["']?([a-z0-9-]+)`)

// SelectUIBundle decides which provider's bridge one adapter's shipped
// artifact actually serves.
//
// webUI is the adapter's Web UI service, or nil for a provider with no
// compose-shaped artifact. packagePayload is the mechanism a native
// package uses instead, or an empty selection when there is none.
func SelectUIBundle(webUI *Service, packagePayload UIBundleSelection, profileFallback string) UIBundleSelection {
	if webUI == nil {
		return packagePayload
	}

	if dir := strings.TrimSpace(webUI.Environment["UI_DIR"]); dir != "" {
		return UIBundleSelection{
			Mechanism: UIBundleImageRoot,
			Provider:  path.Base(dir),
			Detail:    fmt.Sprintf("service %q sets UI_DIR=%s", webUI.Name, dir),
		}
	}

	root := strings.TrimSpace(webUI.Environment["UI_ROOT"])
	if root == "" {
		return UIBundleSelection{
			Mechanism: UIBundleEmbedded,
			Detail:    fmt.Sprintf("service %q sets neither UI_DIR nor UI_ROOT, so it serves the bundle compiled into the binary", webUI.Name),
		}
	}

	profile := profileFallback
	if m := serveUIProfileRe.FindStringSubmatch(strings.Join(webUI.Command, " ")); m != nil {
		profile = m[1]
	}
	if profile == "" {
		return UIBundleSelection{
			Mechanism: UIBundleImageRoot,
			Detail:    fmt.Sprintf("service %q sets UI_ROOT=%s and names no profile, so there is nothing to select a bundle with and serve-ui refuses to start", webUI.Name, root),
		}
	}
	return UIBundleSelection{
		Mechanism: UIBundleImageRoot,
		Provider:  profile,
		Detail:    fmt.Sprintf("service %q sets UI_ROOT=%s and selects profile %q, so it serves %s", webUI.Name, root, profile, path.Join(root, profile)),
	}
}

// spkUIBundleRe reads the provider a Synology-style native package's
// bundle is built for out of its layout declaration.
var spkUIBundleRe = regexp.MustCompile(`UIBundlePlatform\s*=\s*"([a-z0-9-]+)"`)

// PackagePayloadUIBundle reads whether a native package carries its own
// bundle and points its Web UI host at it.
//
// layoutFile and startScript are repository-relative. Both are read as
// source: distribution/ is its own module and may not import apps/, and a
// constant plus a shell script are declarations, which is exactly what
// reading across that boundary is for.
func PackagePayloadUIBundle(layoutFile, startScript string) UIBundleSelection {
	layout, err := os.ReadFile(Path(layoutFile))
	if err != nil {
		return UIBundleSelection{Mechanism: UIBundleNone, Detail: fmt.Sprintf("cannot read %s: %v", layoutFile, err)}
	}
	m := spkUIBundleRe.FindSubmatch(layout)
	if m == nil {
		return UIBundleSelection{Mechanism: UIBundleNone, Detail: fmt.Sprintf("%s declares no UIBundlePlatform, so the package does not carry a bridge of its own", layoutFile)}
	}

	start, err := os.ReadFile(Path(startScript))
	if err != nil {
		return UIBundleSelection{Mechanism: UIBundleNone, Detail: fmt.Sprintf("cannot read %s: %v", startScript, err)}
	}
	if !strings.Contains(string(start), "--ui-dir") {
		return UIBundleSelection{
			Mechanism: UIBundleNone,
			Detail:    fmt.Sprintf("%s carries a %s bundle and %s never passes --ui-dir, so the installed package serves the bundle compiled into the binary instead", layoutFile, m[1], startScript),
		}
	}
	return UIBundleSelection{
		Mechanism: UIBundlePackagePayload,
		Provider:  string(m[1]),
		Detail:    fmt.Sprintf("the package carries a %s bundle and %s serves it with --ui-dir", m[1], startScript),
	}
}
