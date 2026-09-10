package backend

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

// TestEveryBundledManifestParses is what makes a malformed bundled
// manifest unshippable: this is the test that runs on every build,
// against the exact files that ship. The runtime refusal in
// service.Open/openService is the belt; this is the braces.
func TestEveryBundledManifestParses(t *testing.T) {
	reg, err := Bundled()
	if err != nil {
		t.Fatalf("Bundled(): %v", err)
	}
	if reg.Len() == 0 {
		t.Fatal("the bundled registry declares no backend at all")
	}
}

// TestTheBundledSetIsExactly pins the shipped ids (issue #665 section
// 4.1): a third manifest is a reviewed diff that has to change this
// list, not a file that starts being read because it appeared in a
// directory.
func TestTheBundledSetIsExactly(t *testing.T) {
	reg, err := Bundled()
	if err != nil {
		t.Fatalf("Bundled(): %v", err)
	}
	want := []string{"local_volume", "s3"}
	if got := reg.IDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("bundled backend ids = %v, want %v", got, want)
	}
}

// mapFS builds an fstest.MapFS out of name -> JSON text, for Load
// (exported only for tests; see doc.go).
func mapFS(files map[string]string) fstest.MapFS {
	out := make(fstest.MapFS, len(files))
	for name, content := range files {
		out[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return out
}

// validLocalVolumeManifest and validS3Manifest are minimal, otherwise-
// legal manifests a test can mutate one field of at a time. Copied from
// the shipped files rather than re-derived, so a test failure here means
// the RULE broke, not that this fixture drifted from what ships.
const validLocalVolumeManifest = `{
  "id": "local_volume",
  "label": "Local volume",
  "summary": "A directory on a disk.",
  "role": "local_volume",
  "rclone_backend": "local",
  "fields": [
    {"id": "path", "label": "Directory", "kind": "path", "required": true}
  ],
  "probe": {
    "steps": [
      {"step": "credentials", "run": false, "reason": "no credential"},
      {"step": "reach", "run": true},
      {"step": "deliverable", "run": true},
      {"step": "write", "run": true},
      {"step": "read_back", "run": true},
      {"step": "storage_class", "run": false, "reason": "no classes"},
      {"step": "verification", "run": true},
      {"step": "delete", "run": true}
    ]
  }
}`

const validS3Manifest = `{
  "id": "s3",
  "label": "S3",
  "summary": "An object store.",
  "role": "object_store",
  "rclone_backend": "s3",
  "fields": [
    {"id": "bucket", "label": "Bucket", "kind": "string", "required": true},
    {"id": "credentials", "label": "Access key", "kind": "credential", "required": true}
  ],
  "probe": {
    "steps": [
      {"step": "credentials", "run": true},
      {"step": "reach", "run": true},
      {"step": "deliverable", "run": true},
      {"step": "write", "run": true},
      {"step": "read_back", "run": true},
      {"step": "storage_class", "run": true},
      {"step": "verification", "run": true},
      {"step": "delete", "run": true}
    ]
  }
}`

func mustLoadOne(t *testing.T, name, content string) *Registry {
	t.Helper()
	reg, err := Load(mapFS(map[string]string{name: content}))
	if err != nil {
		t.Fatalf("Load(%s) unexpectedly refused a manifest meant to be valid: %v", name, err)
	}
	return reg
}

// TestAnUnknownFieldInAManifestIsAParseError mirrors config.Load's
// KnownFields(true): a manifest carrying a field this build does not
// understand is refused rather than half-understood.
func TestAnUnknownFieldInAManifestIsAParseError(t *testing.T) {
	bad := strings.Replace(validLocalVolumeManifest, `"id": "local_volume",`, `"id": "local_volume", "made_up_field": true,`, 1)
	_, err := Load(mapFS(map[string]string{"a.json": bad}))
	if err == nil {
		t.Fatal("a manifest with an unknown top-level field was accepted")
	}
	if !strings.Contains(err.Error(), "a.json") {
		t.Errorf("error does not name the file: %v", err)
	}
}

// TestAManifestDeclaringAnUnknownRcloneBackendIsRefused is #665's named
// acceptance criterion.
func TestAManifestDeclaringAnUnknownRcloneBackendIsRefused(t *testing.T) {
	bad := strings.Replace(validS3Manifest, `"rclone_backend": "s3"`, `"rclone_backend": "azureblob"`, 1)
	_, err := Load(mapFS(map[string]string{"s3.json": bad}))
	if err == nil {
		t.Fatal("a manifest naming an unsupported rclone backend was accepted")
	}
	if !errors.Is(err, ErrMalformedManifest) {
		t.Errorf("error does not wrap ErrMalformedManifest: %v", err)
	}
	if !strings.Contains(err.Error(), "s3.json") || !strings.Contains(err.Error(), "azureblob") {
		t.Errorf("error does not name the file and the rejected backend: %v", err)
	}
	for name := range SupportedRcloneBackends {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not name the accepted backend %q: %v", name, err)
		}
	}
}

func TestAManifestDeclaringAnUnknownRoleIsRefused(t *testing.T) {
	bad := strings.Replace(validS3Manifest, `"role": "object_store"`, `"role": "database"`, 1)
	_, err := Load(mapFS(map[string]string{"s3.json": bad}))
	if err == nil {
		t.Fatal("a manifest naming an unknown role was accepted")
	}
	if !strings.Contains(err.Error(), "database") {
		t.Errorf("error does not name the rejected role: %v", err)
	}
}

func TestAManifestCannotClaimTheReservedLocalId(t *testing.T) {
	bad := strings.Replace(validLocalVolumeManifest, `"id": "local_volume",`, `"id": "local",`, 1)
	_, err := Load(mapFS(map[string]string{"local.json": bad}))
	if err == nil {
		t.Fatal("a manifest claiming id \"local\" was accepted")
	}
	if !strings.Contains(err.Error(), "local") || !strings.Contains(err.Error(), "MediumLocal") {
		t.Errorf("error does not explain the reservation: %v", err)
	}
}

// TestAMalformedManifestIsRefusedRatherThanSkipped: one good manifest and
// one bad one in the same registry produce a nil registry, not the good
// one alone.
func TestAMalformedManifestIsRefusedRatherThanSkipped(t *testing.T) {
	bad := strings.Replace(validS3Manifest, `"rclone_backend": "s3"`, `"rclone_backend": "azureblob"`, 1)
	reg, err := Load(mapFS(map[string]string{
		"a_local.json": validLocalVolumeManifest,
		"b_bad.json":   bad,
	}))
	if err == nil {
		t.Fatal("a registry containing one malformed manifest was accepted")
	}
	if reg != nil {
		t.Fatalf("a malformed registry was returned instead of nil: %v", reg)
	}
}

func TestALoadReportsEveryProblemNotTheFirst(t *testing.T) {
	bad1 := strings.Replace(validS3Manifest, `"rclone_backend": "s3"`, `"rclone_backend": "azureblob"`, 1)
	bad2 := strings.Replace(validLocalVolumeManifest, `"role": "local_volume"`, `"role": "database"`, 1)
	bad3 := strings.Replace(validLocalVolumeManifest, `"id": "local_volume",`, `"id": "local",`, 1)
	_, err := Load(mapFS(map[string]string{
		"one.json":   bad1,
		"two.json":   bad2,
		"three.json": bad3,
	}))
	if err == nil {
		t.Fatal("three malformed manifests were accepted")
	}
	for _, want := range []string{"one.json", "two.json", "three.json"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s, so it stopped at fewer than three problems: %v", want, err)
		}
	}
}

func TestDuplicateBackendIdsAreRefused(t *testing.T) {
	second := strings.Replace(validS3Manifest, `"rclone_backend": "s3"`, `"rclone_backend": "local"`, 1)
	second = strings.Replace(second, `"role": "object_store"`, `"role": "local_volume"`, 1)
	second = strings.Replace(second, `"kind": "credential"`, `"kind": "string"`, 1)
	_, err := Load(mapFS(map[string]string{
		"a.json": validS3Manifest,
		"b.json": second,
	}))
	if err == nil {
		t.Fatal("two manifests declaring the same id (\"s3\") were accepted")
	}
	if !strings.Contains(err.Error(), "s3") {
		t.Errorf("error does not name the duplicated id: %v", err)
	}
}

func TestDuplicateFieldIdsWithinOneManifestAreRefused(t *testing.T) {
	bad := strings.Replace(validS3Manifest,
		`{"id": "bucket", "label": "Bucket", "kind": "string", "required": true},`,
		`{"id": "bucket", "label": "Bucket", "kind": "string", "required": true},
    {"id": "bucket", "label": "Bucket again", "kind": "string", "required": false},`, 1)
	_, err := Load(mapFS(map[string]string{"s3.json": bad}))
	if err == nil {
		t.Fatal("a manifest declaring the same field id twice was accepted")
	}
	if !strings.Contains(err.Error(), "bucket") {
		t.Errorf("error does not name the duplicated field id: %v", err)
	}
}

func TestAFieldPatternThatDoesNotCompileIsRefused(t *testing.T) {
	bad := strings.Replace(validS3Manifest,
		`{"id": "bucket", "label": "Bucket", "kind": "string", "required": true},`,
		`{"id": "bucket", "label": "Bucket", "kind": "string", "required": true, "pattern": "(unclosed"},`, 1)
	_, err := Load(mapFS(map[string]string{"s3.json": bad}))
	if err == nil {
		t.Fatal("a field with a pattern that does not compile was accepted")
	}
	if !strings.Contains(err.Error(), "bucket") {
		t.Errorf("error does not name the offending field: %v", err)
	}
}

func TestAnEnumFieldMustCarryValuesAndAValuesFieldMustBeAnEnum(t *testing.T) {
	t.Run("enum with no values", func(t *testing.T) {
		bad := strings.Replace(validS3Manifest,
			`{"id": "bucket", "label": "Bucket", "kind": "string", "required": true},`,
			`{"id": "kind_field", "label": "K", "kind": "enum", "required": false},`, 1)
		if _, err := Load(mapFS(map[string]string{"s3.json": bad})); err == nil {
			t.Fatal("an enum field with no values was accepted")
		}
	})
	t.Run("values on a non-enum field", func(t *testing.T) {
		bad := strings.Replace(validS3Manifest,
			`{"id": "bucket", "label": "Bucket", "kind": "string", "required": true},`,
			`{"id": "bucket", "label": "Bucket", "kind": "string", "required": true, "values": [{"value": "x", "label": "X"}]},`, 1)
		if _, err := Load(mapFS(map[string]string{"s3.json": bad})); err == nil {
			t.Fatal("a non-enum field declaring values was accepted")
		}
	})
}

func TestUnsetMeansMustBeOneOfTheDeclaredValues(t *testing.T) {
	bad := strings.Replace(validS3Manifest,
		`{"id": "bucket", "label": "Bucket", "kind": "string", "required": true},`,
		`{"id": "mode", "label": "Mode", "kind": "enum", "required": false, "unset_means": "nope", "values": [{"value": "a", "label": "A"}]},`, 1)
	_, err := Load(mapFS(map[string]string{"s3.json": bad}))
	if err == nil {
		t.Fatal("unset_means naming a value the field does not declare was accepted")
	}
}

func TestACredentialFieldDeclaresNothingButThatItIsOne(t *testing.T) {
	t.Run("pattern", func(t *testing.T) {
		bad := strings.Replace(validS3Manifest,
			`{"id": "credentials", "label": "Access key", "kind": "credential", "required": true}`,
			`{"id": "credentials", "label": "Access key", "kind": "credential", "required": true, "pattern": "^x$"}`, 1)
		if _, err := Load(mapFS(map[string]string{"s3.json": bad})); err == nil {
			t.Fatal("a credential field carrying a pattern was accepted")
		}
	})
	t.Run("values", func(t *testing.T) {
		bad := strings.Replace(validS3Manifest,
			`{"id": "credentials", "label": "Access key", "kind": "credential", "required": true}`,
			`{"id": "credentials", "label": "Access key", "kind": "credential", "required": true, "values": [{"value": "x", "label": "X"}]}`, 1)
		if _, err := Load(mapFS(map[string]string{"s3.json": bad})); err == nil {
			t.Fatal("a credential field carrying values was accepted")
		}
	})
	t.Run("unset_means", func(t *testing.T) {
		bad := strings.Replace(validS3Manifest,
			`{"id": "credentials", "label": "Access key", "kind": "credential", "required": true}`,
			`{"id": "credentials", "label": "Access key", "kind": "credential", "required": true, "unset_means": "x"}`, 1)
		if _, err := Load(mapFS(map[string]string{"s3.json": bad})); err == nil {
			t.Fatal("a credential field carrying unset_means was accepted")
		}
	})
	t.Run("more than one credential field", func(t *testing.T) {
		bad := strings.Replace(validS3Manifest,
			`{"id": "credentials", "label": "Access key", "kind": "credential", "required": true}`,
			`{"id": "credentials", "label": "Access key", "kind": "credential", "required": true},
    {"id": "credentials2", "label": "Second key", "kind": "credential", "required": false}`, 1)
		if _, err := Load(mapFS(map[string]string{"s3.json": bad})); err == nil {
			t.Fatal("a manifest declaring two credential fields was accepted")
		}
	})
}

func TestTheProbeDeclaresEveryStepExactlyOnceInOrder(t *testing.T) {
	cases := map[string]string{
		"missing a step": strings.Replace(validLocalVolumeManifest, `{"step": "delete", "run": true}`, ``, 1),
		"unknown step": strings.Replace(validLocalVolumeManifest,
			`{"step": "delete", "run": true}`, `{"step": "teleport", "run": true}`, 1),
		"reordered": strings.Replace(validLocalVolumeManifest,
			`{"step": "reach", "run": true},
      {"step": "deliverable", "run": true},`,
			`{"step": "deliverable", "run": true},
      {"step": "reach", "run": true},`, 1),
		"run true with a reason": strings.Replace(validLocalVolumeManifest,
			`{"step": "reach", "run": true}`, `{"step": "reach", "run": true, "reason": "should not be here"}`, 1),
		"run false with no reason": strings.Replace(validLocalVolumeManifest,
			`{"step": "credentials", "run": false, "reason": "no credential"}`, `{"step": "credentials", "run": false}`, 1),
	}
	for name, manifest := range cases {
		t.Run(name, func(t *testing.T) {
			if manifest == validLocalVolumeManifest {
				t.Fatal("fixture mutation was a no-op, so this case tests nothing")
			}
			if _, err := Load(mapFS(map[string]string{"local.json": manifest})); err == nil {
				t.Fatalf("%s: manifest was accepted", name)
			}
		})
	}
	t.Run("repeated step", func(t *testing.T) {
		bad := strings.Replace(validLocalVolumeManifest, `{"step": "delete", "run": true}`, `{"step": "reach", "run": true}`, 1)
		if _, err := Load(mapFS(map[string]string{"local.json": bad})); err == nil {
			t.Fatal("a probe with a repeated step name (and one fewer distinct step) was accepted")
		}
	})
}

// TestTheRegistryHasNowhereForCredentialMaterial is medium_test.go's
// shape ("the struct has nowhere for a value to hide") applied to this
// package: a reflect walk over every exported type here, refusing a
// []byte field or a field whose name reads as holding material.
func TestTheRegistryHasNowhereForCredentialMaterial(t *testing.T) {
	suspectNames := []string{"secret", "password", "token", "key", "material", "bytes"}
	check := func(t *testing.T, typ reflect.Type) {
		if typ.Kind() != reflect.Struct {
			return
		}
		for i := range typ.NumField() {
			f := typ.Field(i)
			if f.Type.Kind() == reflect.Slice && f.Type.Elem().Kind() == reflect.Uint8 {
				t.Errorf("%s.%s is a []byte field: exactly the shape a credential leaks into", typ.Name(), f.Name)
			}
			lower := strings.ToLower(f.Name)
			for _, s := range suspectNames {
				if strings.Contains(lower, s) {
					t.Errorf("%s.%s's name reads as if it could hold credential material", typ.Name(), f.Name)
				}
			}
		}
	}
	for _, typ := range []reflect.Type{
		reflect.TypeOf(Manifest{}),
		reflect.TypeOf(Field{}),
		reflect.TypeOf(Probe{}),
		reflect.TypeOf(ProbeStep{}),
		reflect.TypeOf(EnumValue{}),
		reflect.TypeOf(Registry{}),
	} {
		check(t, typ)
	}
}

// TestThisPackageCannotOpenAFile scans this package's own non-test
// source files' import blocks and refuses "os": that is the package any
// disk-reading loader would have to go through, and its absence is what
// makes "an unblessed manifest exists" unreachable code rather than an
// unwritten feature. path/filepath is allowed by name (IsAbs/Clean, for
// ValidateInstance's KindPath rule): it opens nothing.
func TestThisPackageCannotOpenAFile(t *testing.T) {
	imports, err := packageImports(".")
	if err != nil {
		t.Fatalf("scanning imports: %v", err)
	}
	if len(imports) == 0 {
		t.Fatal("no source files were scanned, so this proves nothing")
	}
	for _, problem := range scanForDiskAccess(imports) {
		t.Error(problem)
	}
}

// TestTheImportScanWouldCatchOne is the positive control for the test
// above: without it, "no disk access" could be a scan that never runs
// rather than a real property. It plants an "os" import in an in-memory
// source file and asserts the SAME scanner function flags it, rather
// than re-deriving the rule a second way.
func TestTheImportScanWouldCatchOne(t *testing.T) {
	fset := token.NewFileSet()
	src := "package backend\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\nvar _ = fmt.Sprint\nvar _ = os.Args\n"
	file, err := parser.ParseFile(fset, "planted.go", src, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing planted source: %v", err)
	}
	imports := map[string][]string{"planted.go": importNames(file)}
	problems := scanForDiskAccess(imports)
	if len(problems) == 0 {
		t.Fatal("the planted \"os\" import was not flagged, so the scan above cannot actually catch one")
	}
}

// packageImports parses every non-test .go file directly under dir and
// returns, per file, the import paths it declares.
//
// parser.ParseFile per file found by filepath.Glob, not parser.ParseDir:
// ParseDir is deprecated since Go 1.25 (it does not consider build
// tags when associating files with a package, which does not matter
// for this scan - every file here is unconditionally part of the
// package - but the deprecation still fails golangci-lint's
// staticcheck).
func packageImports(dir string) (map[string][]string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	out := map[string][]string{}
	for _, path := range matches {
		name := filepath.Base(path)
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return nil, err
		}
		out[name] = importNames(file)
	}
	return out, nil
}

func importNames(file *ast.File) []string {
	var names []string
	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		names = append(names, path)
	}
	return names
}

// scanForDiskAccess is the rule TestThisPackageCannotOpenAFile and
// TestTheImportScanWouldCatchOne both exercise, as a pure function so
// the positive control drives the exact same code the real test does
// rather than a second copy of the rule: no non-test file in this
// package may import "os" (or "io/ioutil", its retired twin) or
// "net/http". path/filepath is explicitly allowed: it is used for
// IsAbs/Clean and opens nothing.
func scanForDiskAccess(imports map[string][]string) []string {
	forbidden := map[string]bool{"os": true, "io/ioutil": true, "net/http": true}
	var problems []string
	for file, names := range imports {
		for _, name := range names {
			if forbidden[name] {
				problems = append(problems, fmt.Sprintf(
					"%s imports %q, which lets this package reach outside the fs.FS Load is handed - see doc.go, \"Bundled only\"",
					file, name))
			}
		}
	}
	return problems
}

func TestFieldKindsAreExactly(t *testing.T) {
	want := map[FieldKind]bool{
		KindString: true, KindPath: true, KindURL: true, KindEnum: true,
		KindBool: true, KindCredential: true, KindKeyPrefix: true,
	}
	if !reflect.DeepEqual(validFieldKinds, want) {
		t.Fatalf("the closed field-kind set changed: got %v, want %v", validFieldKinds, want)
	}
}

// TestRetentionDoesNotImportThisPackage is FR-32's "a medium-supplied
// value must never reach a retention decision", made structural: a
// manifest is data this package validates and hands on as strings, and
// nothing in core/internal/retention may ever read one.
func TestRetentionDoesNotImportThisPackage(t *testing.T) {
	dir := filepath.Join("..", "retention")
	imports, err := packageImports(dir)
	if err != nil {
		t.Fatalf("parsing core/internal/retention: %v", err)
	}
	if len(imports) == 0 {
		t.Fatal("no source files were scanned, so this proves nothing")
	}
	for filename, names := range imports {
		for _, path := range names {
			if strings.HasSuffix(path, "/internal/backend") {
				t.Errorf("%s imports %s", filename, path)
			}
		}
	}
}
