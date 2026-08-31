package serve_test

import (
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/spdrman/rclone-manager/apps/common/webhost/serve"
)

// embedded stands in for the compile-time go:embed bundle every build of
// the canonical binary carries. The point of the whole mechanism is that
// this never has to change to serve a different bridge.
func embedded() fs.FS {
	return fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte(`<!doctype html><title>generic</title>`)},
		"assets/app.js": &fstest.MapFile{Data: []byte(`export const bridge = "generic";`)},
	}
}

func writeBundle(t *testing.T, dir, marker string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html><title>"+marker+"</title>"), 0o644); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte(`export const bridge = "`+marker+`";`), 0o644); err != nil {
		t.Fatalf("write app.js: %v", err)
	}
	return dir
}

func TestResolveUIBundlePrecedence(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeBundle(t, filepath.Join(root, "synology"), "synology")
	writeBundle(t, filepath.Join(root, "ugos"), "ugos")
	explicit := writeBundle(t, filepath.Join(t.TempDir(), "operator-supplied"), "operator")

	cases := []struct {
		name       string
		src        serve.UIBundleSource
		wantOrigin serve.UIBundleOrigin
		wantMarker string
	}{
		{
			name:       "no disk source at all falls back to the embedded bundle",
			src:        serve.UIBundleSource{Embedded: embedded()},
			wantOrigin: serve.UIBundleEmbedded,
			wantMarker: "generic",
		},
		{
			name:       "a profile-selected bundle under the bundle root wins over the embedded one",
			src:        serve.UIBundleSource{Embedded: embedded(), Root: root, Profile: "synology"},
			wantOrigin: serve.UIBundleProfileRoot,
			wantMarker: "synology",
		},
		{
			name:       "the same binary and the same root serve a different bridge for a different profile",
			src:        serve.UIBundleSource{Embedded: embedded(), Root: root, Profile: "ugos"},
			wantOrigin: serve.UIBundleProfileRoot,
			wantMarker: "ugos",
		},
		{
			name:       "an explicit --ui-dir wins over everything",
			src:        serve.UIBundleSource{Embedded: embedded(), Root: root, Profile: "synology", Dir: explicit},
			wantOrigin: serve.UIBundleExplicitDir,
			wantMarker: "operator",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bundle, err := serve.ResolveUIBundle(tc.src)
			if err != nil {
				t.Fatalf("ResolveUIBundle: %v", err)
			}
			if bundle.Origin != tc.wantOrigin {
				t.Errorf("Origin = %q, want %q", bundle.Origin, tc.wantOrigin)
			}
			index, err := fs.ReadFile(bundle.FS, "index.html")
			if err != nil {
				t.Fatalf("read index.html: %v", err)
			}
			if !strings.Contains(string(index), tc.wantMarker) {
				t.Errorf("served index.html = %q, want it to carry marker %q", index, tc.wantMarker)
			}
		})
	}
}

func TestResolveUIBundleFailsClosed(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeBundle(t, filepath.Join(root, "generic"), "generic")
	empty := t.TempDir()

	cases := []struct {
		name       string
		src        serve.UIBundleSource
		wantErr    error
		wantDetail string
	}{
		{
			name:       "a --ui-dir that does not exist is a hard failure, never a silent fall back to embedded",
			src:        serve.UIBundleSource{Embedded: embedded(), Dir: filepath.Join(root, "nope")},
			wantErr:    serve.ErrUIBundleUnusable,
			wantDetail: "nope",
		},
		{
			name:       "a --ui-dir with no index.html is a hard failure",
			src:        serve.UIBundleSource{Embedded: embedded(), Dir: empty},
			wantErr:    serve.ErrUIBundleUnusable,
			wantDetail: "index.html",
		},
		{
			name:       "a bundle root that has no directory for this profile is a hard failure naming both",
			src:        serve.UIBundleSource{Embedded: embedded(), Root: root, Profile: "unraid"},
			wantErr:    serve.ErrUIBundleUnusable,
			wantDetail: "unraid",
		},
		{
			name:       "a bundle root with no profile selected is a configuration error, not a guess",
			src:        serve.UIBundleSource{Embedded: embedded(), Root: root},
			wantErr:    serve.ErrUIBundleUnusable,
			wantDetail: "profile",
		},
		{
			name:       "no disk source and no embedded bundle is a hard failure, never an empty web root",
			src:        serve.UIBundleSource{},
			wantErr:    serve.ErrUIBundleUnusable,
			wantDetail: "embedded",
		},
		{
			name:       "a profile name that escapes the bundle root is refused",
			src:        serve.UIBundleSource{Embedded: embedded(), Root: root, Profile: "../../etc"},
			wantErr:    serve.ErrUIBundleUnusable,
			wantDetail: "..",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bundle, err := serve.ResolveUIBundle(tc.src)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ResolveUIBundle error = %v, want %v", err, tc.wantErr)
			}
			if bundle.FS != nil {
				t.Error("a failed resolution still handed back a filesystem to serve")
			}
			if tc.wantDetail != "" && !strings.Contains(err.Error(), tc.wantDetail) {
				t.Errorf("the refusal %q does not say why: it never mentions %q", err, tc.wantDetail)
			}
		})
	}
}

// TestNewUIServesTheResolvedBundle closes the loop: the resolution above
// is only worth anything if the HTTP surface actually serves what it
// picked.
func TestNewUIServesTheResolvedBundle(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeBundle(t, filepath.Join(root, "synology"), "synology")

	upstream, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}

	for _, tc := range []struct {
		name   string
		src    serve.UIBundleSource
		marker string
	}{
		{name: "embedded", src: serve.UIBundleSource{Embedded: embedded()}, marker: "generic"},
		{name: "profile-selected", src: serve.UIBundleSource{Embedded: embedded(), Root: root, Profile: "synology"}, marker: "synology"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bundle, err := serve.ResolveUIBundle(tc.src)
			if err != nil {
				t.Fatalf("ResolveUIBundle: %v", err)
			}
			h := serve.NewUI(serve.UIConfig{Upstream: upstream, StaticFS: bundle.FS})

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET / = %d, want 200", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tc.marker) {
				t.Errorf("GET / served %q, want the %q bundle", rec.Body.String(), tc.marker)
			}
		})
	}
}
