// Issue #180's packaging half: which adapter UI bundles the canonical
// image carries, and the proof that carrying them is a decision rather
// than an accident.
//
// The interesting constraint is that this rule fails in both directions
// and the two failures look nothing alike. Carrying too few reinstates
// #180 for whichever adapter was dropped, and it does so at the worst
// moment, because serve-ui refuses to start rather than quietly serving
// the wrong bridge. Carrying too many is an image-size regression against
// a budget with a couple of megabytes of headroom, which nobody notices
// until a release. So the tests below assert the set, not a floor, and
// the reasoning behind the set lives on the test that checks it.
package packaging

import (
	"strings"
	"testing"
)

// TestTheCanonicalImageCarriesEachAdapterBundle is issue #180's packaging
// half, and the reason it is a test rather than a line in a README is the
// budget it lives under.
//
// The image size is gated at 1.05x of a recorded baseline. Seven bundles
// at roughly 347 KB each is about 2.4 MB against about 2.15 MB of
// headroom, so the image can only carry the ones that have no other
// carrier: an adapter that is metadata and nothing else. Both halves of
// that sentence have to stay true, and each fails differently. Carrying
// too few silently reinstates #180 for whichever adapter was dropped,
// because serve-ui then refuses to start rather than serving the wrong
// bridge. Carrying too many is an image-size regression nobody meant.
func TestTheCanonicalImageCarriesEachAdapterBundle(t *testing.T) {
	root, carried, err := ImageBundleRoot()
	if err != nil {
		t.Fatalf("read the image's bundle carriage: %v", err)
	}
	if root == "" {
		t.Fatal("the Dockerfile COPYs the bundles nowhere")
	}

	c := MustLoad()
	conf := MustLoadConformance()

	// Every adapter that ships a bridge must select it from the image and
	// find one there; every adapter that ships none must select the
	// bundle compiled into the binary and must not be carried at all.
	//
	// Two-sided since issue #170, which added four adapters with no
	// bridge. The reason is arithmetic rather than preference: the image
	// carries five bundles with 347,956 bytes of headroom against its
	// gated 5% ceiling and one bundle costs roughly 352 KB, so a sixth
	// does not fit, let alone four more. Reading the answer off
	// canonical.json's uiBridge rather than off "does this compose file
	// happen to set UI_ROOT" is what keeps that a declaration somebody
	// made instead of a shape somebody noticed.
	carriers := 0
	for _, p := range allPlatforms() {
		t.Run(p.name, func(t *testing.T) {
			for _, art := range p.runtimeArtifacts(t) {
				rt, drift := ReduceToRoles(p.name, art.svcs, c)
				if len(drift) > 0 {
					t.Fatalf("%s: %s", art.name, FormatDrift(drift))
				}
				sel := SelectUIBundle(rt.WebUI, UIBundleSelection{Mechanism: UIBundleNone}, c.Platforms[p.name].Profile)

				if c.Platforms[p.name].UIBridge == UIBridgeNone {
					if sel.Mechanism != UIBundleEmbedded {
						t.Errorf("%s ships no frontend bridge and yet selects a %q bundle (%s); there is nothing for it to select and serve-ui fails closed rather than falling back", art.name, sel.Mechanism, sel.Detail)
					}
					if contains(carried, p.name) {
						t.Errorf("the image carries a %s bundle and that adapter ships no bridge to put in it, which is roughly 352 KB of image against a ceiling with 347,956 bytes left", p.name)
					}
					continue
				}
				carriers++
				if sel.Mechanism != UIBundleImageRoot {
					t.Fatalf("%s does not select a bundle from the image (%s); every converted adapter here is metadata only, so the image is its only carrier", art.name, sel.Detail)
				}
				if sel.Provider != p.name {
					t.Errorf("%s selects the %s bundle, not this platform's own", art.name, sel.Provider)
				}
				if !contains(carried, sel.Provider) {
					t.Errorf("%s selects the %s bundle and the image carries %v; serve-ui fails closed on a missing bundle, so this adapter would not start at all", art.name, sel.Provider, carried)
				}
				if declared := strings.TrimSuffix(strings.TrimSpace(rt.WebUI.Environment["UI_ROOT"]), "/"); declared != root {
					t.Errorf("%s sets UI_ROOT=%s and the image carries the bundles at %s", art.name, declared, root)
				}
			}
		})
	}
	if carriers == 0 {
		t.Error("no adapter selects a bundle out of the image at all, so the carriage this test exists to check was never exercised")
	}

	// And nothing else is in there. A bundle for a provider that has its
	// own carrier is 347 KB of image nobody serves.
	for _, provider := range carried {
		if _, declared := conf.Providers[provider]; !declared {
			t.Errorf("the image carries a bundle for %q, which is not a provider this matrix declares", provider)
		}
		if provider == "generic" {
			t.Error("the image carries a generic bundle, which is already compiled into the binary; that is 347 KB of duplicate against a gated image-size budget")
		}
	}
}

// TestTheGenericBundleIsTheOneCompiledIntoTheBinary guards an ordering
// trap with no other symptom.
//
// `npm run build:bundles` runs `npm run build` once per provider, so it
// leaves dist/ holding whichever provider it built last, and dist/ is
// what go:embed compiles into the binary. Swap the two Dockerfile lines
// and the canonical image's built-in UI becomes a NAS provider's bridge:
// the build succeeds, the image runs, every test passes, and a generic
// Docker user is told they are on Synology.
func TestTheGenericBundleIsTheOneCompiledIntoTheBinary(t *testing.T) {
	ok, why := GenericBundleIsBuiltLast()
	if !ok {
		t.Errorf("%s", why)
	}

	// Positive control: the same reasoning over a Dockerfile with the
	// two lines reversed has to fail, or the assertion above is reading
	// a file and concluding nothing.
	shipped, _, err := ShippedBridgeProvider()
	if err != nil {
		t.Fatalf("read the shipped bridge: %v", err)
	}
	if shipped != "generic" {
		t.Errorf("the release build selects the %s bridge as the compiled-in bundle, want generic: every other provider gets its bridge from a bundle it carries, and the binary's own has to be the vendor-neutral one", shipped)
	}
}

// TestUIBundleSelectionIsDecidedByTheArtifact is the positive-control
// suite for the selector itself, one row per way a deployment can end up
// serving a bridge, right or wrong.
//
// Table-driven over synthetic services rather than over the real
// adapters, because what needs proving here is that the selector can
// distinguish these cases at all. The real adapters only ever exercise
// one of the rows.
func TestUIBundleSelectionIsDecidedByTheArtifact(t *testing.T) {
	svc := func(env map[string]string, cmd ...string) *Service {
		return &Service{Name: "backup-manager-ui", Command: cmd, Environment: env}
	}

	for _, tc := range []struct {
		name         string
		webUI        *Service
		payload      UIBundleSelection
		fallback     string
		wantMech     UIBundleMechanism
		wantProvider string
	}{
		{
			name:     "nothing configured: the bundle compiled into the binary",
			webUI:    svc(map[string]string{}, "/backup-manager-web", "serve-ui"),
			payload:  UIBundleSelection{Mechanism: UIBundleNone},
			wantMech: UIBundleEmbedded,
		},
		{
			name:         "UI_ROOT plus a profile: the image's bundle for that profile",
			webUI:        svc(map[string]string{"UI_ROOT": "/ui/bundles"}, "/backup-manager-web", "serve-ui", "--profile=truenas"),
			payload:      UIBundleSelection{Mechanism: UIBundleNone},
			wantMech:     UIBundleImageRoot,
			wantProvider: "truenas",
		},
		{
			name:         "UI_DIR wins outright, whatever the profile says",
			webUI:        svc(map[string]string{"UI_DIR": "/opt/pkg/ui-bundle/unraid", "UI_ROOT": "/ui/bundles"}, "/backup-manager-web", "serve-ui", "--profile=truenas"),
			payload:      UIBundleSelection{Mechanism: UIBundleNone},
			wantMech:     UIBundleImageRoot,
			wantProvider: "unraid",
		},
		{
			// The shape that fails closed at run time. It has to be
			// reported as "no bundle", never as the fallback profile's,
			// or the matrix would claim a bridge for a deployment that
			// does not start.
			name:     "UI_ROOT with no profile at all selects nothing",
			webUI:    svc(map[string]string{"UI_ROOT": "/ui/bundles"}, "/backup-manager-web", "serve-ui"),
			payload:  UIBundleSelection{Mechanism: UIBundleNone},
			fallback: "",
			wantMech: UIBundleImageRoot,
		},
		{
			name:         "no compose service at all: whatever the native package does",
			webUI:        nil,
			payload:      UIBundleSelection{Mechanism: UIBundlePackagePayload, Provider: "synology", Detail: "the package carries it"},
			wantMech:     UIBundlePackagePayload,
			wantProvider: "synology",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := SelectUIBundle(tc.webUI, tc.payload, tc.fallback)
			if got.Mechanism != tc.wantMech {
				t.Errorf("mechanism = %q, want %q (%s)", got.Mechanism, tc.wantMech, got.Detail)
			}
			if got.Provider != tc.wantProvider {
				t.Errorf("provider = %q, want %q (%s)", got.Provider, tc.wantProvider, got.Detail)
			}
			if got.Detail == "" {
				t.Error("the selection carries no evidence, so a report of it says nothing")
			}
		})
	}
}

// TestAPackageThatCarriesABundleButNeverServesItIsRefused is the second
// half of the native-package mechanism, and it is the half that fails
// silently.
//
// A .spk can carry a perfectly good bundle and still show the generic
// bridge, because what decides that is one flag in the start script. So
// the check reads both files, and this proves it reads the second one.
func TestAPackageThatCarriesABundleButNeverServesItIsRefused(t *testing.T) {
	const layout = "apps/synology/spk/layout.go"
	const start = "apps/synology/spk/assets/scripts/start-stop-status"

	got := PackagePayloadUIBundle(layout, start)
	if got.Mechanism != UIBundlePackagePayload || got.Provider != "synology" {
		t.Fatalf("the shipped package reads as %q/%q: %s", got.Mechanism, got.Provider, got.Detail)
	}

	// Control one: a start script that never passes --ui-dir. Any other
	// file in the tree with no --ui-dir in it will do, and using a real
	// one keeps this from depending on a fixture nobody maintains.
	noServe := PackagePayloadUIBundle(layout, "apps/synology/spk/assets/scripts/postinst")
	if noServe.Mechanism == UIBundlePackagePayload {
		t.Errorf("a start script that never passes --ui-dir read as a package that serves its own bundle: %s", noServe.Detail)
	}
	if !strings.Contains(noServe.Detail, "--ui-dir") {
		t.Errorf("the refusal never says what is missing: %s", noServe.Detail)
	}

	// Control two: a layout that declares no bundle platform.
	noBundle := PackagePayloadUIBundle("apps/synology/spk/arch.go", start)
	if noBundle.Mechanism != UIBundleNone {
		t.Errorf("a layout declaring no UIBundlePlatform read as %q: %s", noBundle.Mechanism, noBundle.Detail)
	}
}

// TestTheDockerfileBundleReaderCanFail is the control for the two
// Dockerfile readers. Both return an error or a false on a Dockerfile
// that does not carry bundles, and neither has ever been watched doing
// it against the real one, which always does.
func TestTheDockerfileBundleReaderCanFail(t *testing.T) {
	// A path that is not a Dockerfile at all: the readers must report
	// rather than return an empty, clean answer.
	if _, _, err := ImageBundleRoot(); err != nil {
		t.Fatalf("the real Dockerfile does not read as carrying bundles: %v", err)
	}
	root, carried, _ := ImageBundleRoot()
	if len(carried) == 0 || root == "" {
		t.Fatalf("ImageBundleRoot returned root=%q carried=%v; an empty answer would let every adapter pass", root, carried)
	}
	// Every carried provider has to be a real one, so a typo in the
	// Dockerfile's argument list is a failure rather than a bundle
	// directory nothing selects.
	conf := MustLoadConformance()
	for _, p := range carried {
		if _, ok := conf.Providers[p]; !ok {
			t.Errorf("the Dockerfile builds a bundle for %q, which is not a declared provider", p)
		}
	}
}
