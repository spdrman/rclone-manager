package backend

import (
	"strings"
	"testing"
)

// TestValidateInstance is the table over every rule in issue #665
// section 1.6, positive and negative, for both bundled manifests.
func TestValidateInstance(t *testing.T) {
	reg := mustLoadOne(t, "s3.json", validS3Manifest)
	localReg := mustLoadOne(t, "local_volume.json", validLocalVolumeManifest)

	cases := []struct {
		name    string
		reg     *Registry
		backend string
		values  map[string]string
		wantErr bool
	}{
		{"a fully valid s3 instance", reg, "s3", map[string]string{
			"bucket": "my-bucket", "credentials": "cred-ref-1",
		}, false},
		{"required bucket missing", reg, "s3", map[string]string{
			"credentials": "cred-ref-1",
		}, true},
		{"required credential missing", reg, "s3", map[string]string{
			"bucket": "my-bucket",
		}, true},
		{"credential value carries a slash", reg, "s3", map[string]string{
			"bucket": "my-bucket", "credentials": "../etc/passwd",
		}, true},
		{"credential value carries a backslash", reg, "s3", map[string]string{
			"bucket": "my-bucket", "credentials": `a\b`,
		}, true},
		{"a fully valid local_volume instance", localReg, "local_volume", map[string]string{
			"path": "/mnt/backups",
		}, false},
		{"path not absolute", localReg, "local_volume", map[string]string{
			"path": "relative/path",
		}, true},
		{"path with .. segment", localReg, "local_volume", map[string]string{
			"path": "/mnt/../etc",
		}, true},
		{"path with . segment", localReg, "local_volume", map[string]string{
			"path": "/mnt/./backups",
		}, true},
		{"an unknown backend", reg, "azureblob", map[string]string{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := c.reg.ValidateInstance(c.backend, "storage_mediums[0]", c.values)
			if c.wantErr && len(errs) == 0 {
				t.Fatal("expected at least one problem, got none")
			}
			if !c.wantErr && len(errs) != 0 {
				t.Fatalf("expected no problems, got: %v", errs)
			}
		})
	}

	t.Run("enum value not declared", func(t *testing.T) {
		errs := reg.ValidateInstance("s3", "storage_mediums[0]", map[string]string{
			"bucket": "b", "credentials": "c", "storage_class": "NOT_A_CLASS",
		})
		if len(errs) == 0 {
			t.Fatal("expected a problem for an undeclared enum value, got none")
		}
	})

	t.Run("enum value declared", func(t *testing.T) {
		s3WithClasses := mustLoadOne(t, "s3.json", validS3Manifest)
		errs := s3WithClasses.ValidateInstance("s3", "storage_mediums[0]", map[string]string{
			"bucket": "b", "credentials": "c",
		})
		if len(errs) != 0 {
			t.Fatalf("expected no problems, got: %v", errs)
		}
	})

	t.Run("bool field", func(t *testing.T) {
		boolReg := mustLoadOne(t, "b.json", strings.Replace(validLocalVolumeManifest,
			`{"id": "path", "label": "Directory", "kind": "path", "required": true}`,
			`{"id": "path", "label": "Directory", "kind": "path", "required": true},
    {"id": "flag", "label": "Flag", "kind": "bool", "required": false}`, 1))
		if errs := boolReg.ValidateInstance("local_volume", "p", map[string]string{"path": "/x", "flag": "true"}); len(errs) != 0 {
			t.Errorf("bool=true should pass: %v", errs)
		}
		if errs := boolReg.ValidateInstance("local_volume", "p", map[string]string{"path": "/x", "flag": "false"}); len(errs) != 0 {
			t.Errorf("bool=false should pass: %v", errs)
		}
		if errs := boolReg.ValidateInstance("local_volume", "p", map[string]string{"path": "/x", "flag": "yes"}); len(errs) == 0 {
			t.Error("bool=yes should be refused")
		}
	})

	t.Run("string pattern", func(t *testing.T) {
		shipped, err := Bundled()
		if err != nil {
			t.Fatalf("Bundled(): %v", err)
		}
		if errs := shipped.ValidateInstance("s3", "p", map[string]string{"bucket": "has/slash", "credentials": "c"}); len(errs) == 0 {
			t.Error("a bucket carrying a slash should be refused by the pattern ^[^/]+$")
		}
	})

	t.Run("url field", func(t *testing.T) {
		s3Reg := mustLoadOne(t, "s3.json", strings.Replace(validS3Manifest,
			`{"id": "bucket", "label": "Bucket", "kind": "string", "required": true},`,
			`{"id": "bucket", "label": "Bucket", "kind": "string", "required": true},
    {"id": "endpoint", "label": "Endpoint", "kind": "url", "required": false},`, 1))
		good := s3Reg.ValidateInstance("s3", "p", map[string]string{"bucket": "b", "credentials": "c", "endpoint": "https://minio.example.com"})
		if len(good) != 0 {
			t.Errorf("a plain https URL should pass: %v", good)
		}
		for _, bad := range []string{"not a url", "ftp://x.example.com", "https://user:pass@x.example.com"} {
			if errs := s3Reg.ValidateInstance("s3", "p", map[string]string{"bucket": "b", "credentials": "c", "endpoint": bad}); len(errs) == 0 {
				t.Errorf("endpoint %q should be refused", bad)
			}
		}
	})

	t.Run("key_prefix field", func(t *testing.T) {
		shipped, err := Bundled()
		if err != nil {
			t.Fatalf("Bundled(): %v", err)
		}
		for _, bad := range []string{"/leading", "trailing/", "a//b", "a/../b", "a/./b"} {
			if errs := shipped.ValidateInstance("s3", "p", map[string]string{"bucket": "b", "credentials": "c", "prefix": bad}); len(errs) == 0 {
				t.Errorf("prefix %q should be refused", bad)
			}
		}
		if errs := shipped.ValidateInstance("s3", "p", map[string]string{"bucket": "b", "credentials": "c", "prefix": "clean/prefix"}); len(errs) != 0 {
			t.Errorf("a clean prefix should pass: %v", errs)
		}
	})
}

// TestValidateInstanceRefusesAnUnknownField is a value key no field
// declares. The message never quotes the unknown key back (see
// ValidateInstance's own docblock, "No message ever echoes an
// operator-supplied VALUE" - issue #665's C5), so this checks that it
// is refused and that the message still tells an operator which
// backend and which fields ARE declared, not that it names the typo.
func TestValidateInstanceRefusesAnUnknownField(t *testing.T) {
	reg := mustLoadOne(t, "s3.json", validS3Manifest)
	errs := reg.ValidateInstance("s3", "storage_mediums[0]", map[string]string{
		"bucket": "b", "credentials": "c", "made_up_key": "x",
	})
	if len(errs) == 0 {
		t.Fatal("a value key no field declares was accepted")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Error(), "s3") && strings.Contains(e.Error(), "bucket") {
			found = true
		}
		if strings.Contains(e.Error(), "made_up_key") {
			t.Errorf("the unrecognized key was echoed back, which C5 says must never happen: %v", e)
		}
	}
	if !found {
		t.Errorf("no error names the backend and its declared fields: %v", errs)
	}
}

// TestValidateInstanceNeverSubstitutesUnsetMeans is #294's rule: an
// unset optional field produces no error, and nothing here ever writes
// UnsetMeans into the values map the caller handed in.
func TestValidateInstanceNeverSubstitutesUnsetMeans(t *testing.T) {
	shipped, err := Bundled()
	if err != nil {
		t.Fatalf("Bundled(): %v", err)
	}
	values := map[string]string{"bucket": "b", "credentials": "c"}
	before := len(values)
	errs := shipped.ValidateInstance("s3", "storage_mediums[0]", values)
	if len(errs) != 0 {
		t.Fatalf("an s3 instance with every optional field unset should validate: %v", errs)
	}
	if len(values) != before {
		t.Fatalf("ValidateInstance wrote into the caller's values map: %v", values)
	}
	if _, ok := values["storage_class"]; ok {
		t.Error("storage_class was substituted with unset_means, which #294 says must never happen")
	}
}

// TestValidateInstanceReportsEveryProblem: three bad fields produce
// three errors, not one.
func TestValidateInstanceReportsEveryProblem(t *testing.T) {
	shipped, err := Bundled()
	if err != nil {
		t.Fatalf("Bundled(): %v", err)
	}
	errs := shipped.ValidateInstance("s3", "storage_mediums[0]", map[string]string{
		"bucket":        "has/a/slash",
		"credentials":   `bad\ref`,
		"storage_class": "NOT_A_REAL_CLASS",
	})
	if len(errs) < 3 {
		t.Fatalf("expected at least 3 problems (bucket, credentials, storage_class), got %d: %v", len(errs), errs)
	}
}
