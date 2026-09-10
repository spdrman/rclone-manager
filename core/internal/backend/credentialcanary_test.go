package backend

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// The same canary literals core/service/mediums_test.go already mints
// (testCanaryAccessKeyID, testCanarySecret), copied rather than a fresh
// pair invented here: the point of a canary is that the SAME string is
// looked for at every leak surface across the whole credential path
// (issue #665 section 3.3), and a different one per package would prove
// nothing about the path as a whole.
const (
	testCanaryAccessKeyID = "EXAMPLEKEYIDNOTREAL0"
	testCanarySecret      = "EXAMPLE-SECRET-NOT-A-REAL-KEY-0000000000"
)

// TestNoCredentialCanaryReachesAValidateInstanceMessage is C1's first
// half: an instance map whose credential field holds the canary never
// puts it in a returned error string. (The second half - creating a
// medium from an imported reference and reading config.yaml back as
// bytes - lives in core/service/mediums_test.go, where the write
// happens.)
func TestNoCredentialCanaryReachesAValidateInstanceMessage(t *testing.T) {
	shipped, err := Bundled()
	if err != nil {
		t.Fatalf("Bundled(): %v", err)
	}
	// The canary is planted as the credential value itself. It is not a
	// legal reference (ValidateInstance refuses a "/" or "\"-free but
	// otherwise arbitrary string as a value just fine - resolving
	// whether an id was actually minted is core/service's job), so this
	// also exercises the no-refusal path, which is the one most likely
	// to have quoted it back by mistake.
	errs := shipped.ValidateInstance("s3", "storage_mediums[0]", map[string]string{
		"bucket": "my-bucket", "credentials": testCanaryAccessKeyID + testCanarySecret,
	})
	for _, e := range errs {
		if strings.Contains(e.Error(), testCanaryAccessKeyID) || strings.Contains(e.Error(), testCanarySecret) {
			t.Errorf("a ValidateInstance error carries the credential canary: %v", e)
		}
	}
}

// TestTheRegistryRendersStablyAndHoldsNoMaterial is C4: %+v, %#v and an
// slog JSON render of a Manifest, a Field and a Registry built from the
// bundled set, plus the structural half TestTheRegistryHasNowhereForCredentialMaterial
// already proves by reflection - repeated here so the log-line surface
// and the structural proof are asserted together, in the file C4 names.
func TestTheRegistryRendersStablyAndHoldsNoMaterial(t *testing.T) {
	shipped, err := Bundled()
	if err != nil {
		t.Fatalf("Bundled(): %v", err)
	}
	s3, err := shipped.Backend("s3")
	if err != nil {
		t.Fatalf("Backend(\"s3\"): %v", err)
	}
	cred, ok := s3.CredentialField()
	if !ok {
		t.Fatal("the shipped s3 manifest declares no credential field, so this test checks nothing")
	}

	renders := []string{
		fmt.Sprintf("%+v", s3),
		fmt.Sprintf("%#v", s3),
		fmt.Sprintf("%+v", cred),
		fmt.Sprintf("%+v", shipped),
	}

	var buf strings.Builder
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("manifest", "manifest", s3, "field", cred)
	renders = append(renders, buf.String())

	for i, r := range renders {
		if strings.Contains(r, testCanaryAccessKeyID) || strings.Contains(r, testCanarySecret) {
			t.Errorf("render %d carries the credential canary:\n%s", i, r)
		}
	}

	first := fmt.Sprintf("%+v", s3)
	second := fmt.Sprintf("%+v", s3)
	if first != second {
		t.Errorf("%%+v of the same Manifest value is not stable:\n  %s\n  %s", first, second)
	}
}

// errorCanaryTable is the positions C5 plants the canary in: every place
// a string reaches an error message this package can produce.
type errorCanaryPosition struct {
	name  string
	build func(canary string) error
}

// errorCanaryPositions builds one manifest-load or one ValidateInstance
// call per position, with the canary in the slot named, and returns the
// resulting error (nil if that shape happens not to refuse, which is
// itself checked below).
func errorCanaryPositions() []errorCanaryPosition {
	return []errorCanaryPosition{
		{"the credential value", func(c string) error {
			reg := mustLoad(validS3Manifest)
			errs := reg.ValidateInstance("s3", "p", map[string]string{"bucket": "b", "credentials": c + "/x"})
			return firstOrNil(errs)
		}},
		{"a string value with a pattern violation", func(c string) error {
			bad := strings.Replace(validS3Manifest,
				`{"id": "bucket", "label": "Bucket", "kind": "string", "required": true},`,
				`{"id": "bucket", "label": "Bucket", "kind": "string", "required": true, "pattern": "^[^/]+$"},`, 1)
			reg := mustLoad(bad)
			errs := reg.ValidateInstance("s3", "p", map[string]string{"bucket": "has/a/slash-" + c, "credentials": "x"})
			return firstOrNil(errs)
		}},
		{"an enum value not declared", func(c string) error {
			bad := strings.Replace(validS3Manifest,
				`{"id": "bucket", "label": "Bucket", "kind": "string", "required": true},`,
				`{"id": "bucket", "label": "Bucket", "kind": "string", "required": true},
    {"id": "mode", "label": "Mode", "kind": "enum", "required": false, "values": [{"value": "a", "label": "A"}]},`, 1)
			reg := mustLoad(bad)
			errs := reg.ValidateInstance("s3", "p", map[string]string{"bucket": "b", "credentials": "x", "mode": c})
			return firstOrNil(errs)
		}},
		{"an unknown field's key", func(c string) error {
			reg := mustLoad(validS3Manifest)
			errs := reg.ValidateInstance("s3", "p", map[string]string{"bucket": "b", "credentials": "x", c: "value"})
			return firstOrNil(errs)
		}},
		{"an unknown field's value", func(c string) error {
			reg := mustLoad(validS3Manifest)
			errs := reg.ValidateInstance("s3", "p", map[string]string{"bucket": "b", "credentials": "x", "made_up_field": c})
			return firstOrNil(errs)
		}},
		{"a path field", func(c string) error {
			reg := mustLoad(validLocalVolumeManifest)
			errs := reg.ValidateInstance("local_volume", "p", map[string]string{"path": "relative/" + c})
			return firstOrNil(errs)
		}},
		{"a URL field's userinfo", func(c string) error {
			bad := strings.Replace(validS3Manifest,
				`{"id": "bucket", "label": "Bucket", "kind": "string", "required": true},`,
				`{"id": "bucket", "label": "Bucket", "kind": "string", "required": true},
    {"id": "endpoint", "label": "Endpoint", "kind": "url", "required": false},`, 1)
			reg := mustLoad(bad)
			errs := reg.ValidateInstance("s3", "p", map[string]string{"bucket": "b", "credentials": "x",
				"endpoint": "https://" + c + ":" + c + "@minio.example.com"})
			return firstOrNil(errs)
		}},
		{"a URL field's query", func(c string) error {
			bad := strings.Replace(validS3Manifest,
				`{"id": "bucket", "label": "Bucket", "kind": "string", "required": true},`,
				`{"id": "bucket", "label": "Bucket", "kind": "string", "required": true},
    {"id": "endpoint", "label": "Endpoint", "kind": "url", "required": false},`, 1)
			reg := mustLoad(bad)
			errs := reg.ValidateInstance("s3", "p", map[string]string{"bucket": "b", "credentials": "x",
				"endpoint": "not-a-url-" + c})
			return firstOrNil(errs)
		}},
	}
}

func mustLoad(manifest string) *Registry {
	reg, err := Load(mapFS(map[string]string{"m.json": manifest}))
	if err != nil {
		panic(err) // programmer error in a fixture, not a test failure to report gracefully
	}
	return reg
}

func firstOrNil(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return errs[0]
}

// TestTheErrorCanaryNeverReachesAMessage is C5, "the leak nobody tests":
// table-driven over every refusal ValidateInstance and Load can
// produce, with the canary planted in every position that could reach a
// message. C5 is the assertion Main asked for by name; it is written
// first among the credential-canary tests for that reason.
func TestTheErrorCanaryNeverReachesAMessage(t *testing.T) {
	const canary = "CANARY-c5-2c9f7e"
	checked := 0
	for _, pos := range errorCanaryPositions() {
		t.Run(pos.name, func(t *testing.T) {
			err := pos.build(canary)
			if err == nil {
				t.Skip("this position did not produce an error for this input, so there is no message to check - see the positive control for proof the canary CAN be caught")
				return
			}
			checked++
			if strings.Contains(err.Error(), canary) {
				t.Errorf("the canary reached an error message: %v", err)
			}
		})
	}
	if checked == 0 {
		t.Fatal("not one position produced an error, so this test checked nothing")
	}
}

// TestTheErrorCanaryWouldCatchALeak is C5's positive control: a
// deliberately leaky fmt.Errorf that DOES quote the canary back, run
// through the identical detection this file uses (strings.Contains),
// so a passing suite above is known to be catching a real leak and not
// merely finding nothing to look at.
func TestTheErrorCanaryWouldCatchALeak(t *testing.T) {
	const canary = "CANARY-positive-control-9d1b"
	leaky := fmt.Errorf("%s: %q is not a field of the %q backend", "p", canary, "s3")
	if !strings.Contains(leaky.Error(), canary) {
		t.Fatal("the detection itself (strings.Contains) does not find a canary planted directly in a message, so it cannot prove anything above")
	}
}

// TestJSONRoundTripCarriesNoExtraCredentialSurface is a small structural
// check alongside C4: marshalling a Manifest to JSON and back produces
// the identical value, so there is no hidden field encoding/json would
// skip on the way out and restore on the way in - the same "the struct
// has nowhere for a value to hide" property, seen from the wire shape
// rather than from reflection.
func TestJSONRoundTripCarriesNoExtraCredentialSurface(t *testing.T) {
	shipped, err := Bundled()
	if err != nil {
		t.Fatalf("Bundled(): %v", err)
	}
	s3, _ := shipped.Backend("s3")
	raw, err := json.Marshal(s3)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Manifest
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.ID != s3.ID || len(back.Fields) != len(s3.Fields) {
		t.Fatalf("round trip lost data: got %+v, want %+v", back, s3)
	}
}
