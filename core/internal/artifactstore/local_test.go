package artifactstore_test

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/artifactstore"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/internal/model"
)

// lifecycleDir is where TestLifecycleUsesOnlyTheSharedFormulaFromThisPackage
// looks. Relative to this package's own directory, which is where `go test`
// runs a package's tests from.
const lifecycleDir = "../lifecycle"

func testSetID(t *testing.T) model.BackupSetID {
	t.Helper()
	id, err := model.NewBackupSetID("src", "set")
	if err != nil {
		t.Fatalf("NewBackupSetID: %v", err)
	}
	return id
}

func testArtifact(t *testing.T, name string) model.ArtifactID {
	t.Helper()
	a, err := model.NewArtifactID(testSetID(t), name)
	if err != nil {
		t.Fatalf("NewArtifactID(%q): %v", name, err)
	}
	return a
}

// TestLocalLocatorIsTheLiteralPathEveryDeploymentAlreadyHas pins the
// formula to strings written out here, not to another implementation of
// the same formula.
//
// The first version of this test compared LocalLocator against
// lifecycle.FinalArtifactPath, which stopped meaning anything the moment
// lifecycle's finalPath started delegating here: both sides became the
// same function, so rewriting the join as filepath.Join(dir,
// "SILENTLY_RELOCATED", name) kept it green while relocating every
// artifact on every existing deployment. A tautology cannot detect
// drift.
//
// The literals below are the real pin. Asserting lifecycle's exported
// path against the same literals is what still ties the two ends
// together, because now each end is pinned to the answer rather than to
// the other end.
//
// It asks the STORE rather than the free function since #390, because
// LocalLocator is gone: the conversion #334 deferred is done, and this
// test's subject is now the answer a Local gives, which is the answer
// every existing deployment's artifacts are actually sitting at.
func TestLocalLocatorIsTheLiteralPathEveryDeploymentAlreadyHas(t *testing.T) {
	cases := []struct{ dir, name, want string }{
		{"/data/backups", "backup.dump", "/data/backups/backup.dump"},
		{"/data/backups", "backup.dump.zst", "/data/backups/backup.dump.zst"},
		{"/data/backups", "a b.tar", "/data/backups/a b.tar"},
		{"/data/backups", "2026-09-01T00-00-00Z.sql", "/data/backups/2026-09-01T00-00-00Z.sql"},
		{"/mnt/tank/backup-manager/backups", "backup.dump", "/mnt/tank/backup-manager/backups/backup.dump"},
		{"/data/backups/", "backup.dump", "/data/backups/backup.dump"},
		{"relative/dir", "backup.dump", "relative/dir/backup.dump"},
	}

	for _, tc := range cases {
		artifact := testArtifact(t, tc.name)
		store, err := artifactstore.NewLocal(tc.dir)
		if err != nil {
			t.Fatalf("NewLocal(%q): %v", tc.dir, err)
		}
		if got, err := store.Locator(artifact); err != nil {
			t.Errorf("Locator(%q, %q): %v", tc.dir, tc.name, err)
		} else if got != tc.want {
			t.Errorf("Locator(%q, %q) = %q, want %q", tc.dir, tc.name, got, tc.want)
		}
		got, err := lifecycle.FinalArtifactPath(tc.dir, artifact)
		if err != nil {
			t.Errorf("lifecycle.FinalArtifactPath(%q, %q): %v", tc.dir, tc.name, err)
		} else if got != tc.want {
			t.Errorf("lifecycle.FinalArtifactPath(%q, %q) = %q, want %q", tc.dir, tc.name, got, tc.want)
		}
	}
}

// TestLocalLocatorMatchesTheRootItWasBuiltWith proves a Local answers for
// the root it was constructed with, and not for some other one.
//
// It used to compare the Store method against the free function
// LocalLocator, and that comparison died with the free function in #390:
// the two ends became one, which is the tautology this file's other test
// already warns about. The literal is the pin now, for the same reason it
// is the pin there.
func TestLocalLocatorMatchesTheRootItWasBuiltWith(t *testing.T) {
	const root = "/data/backups"
	artifact := testArtifact(t, "backup.dump")

	store, err := artifactstore.NewLocal(root)
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	got, err := store.Locator(artifact)
	if err != nil {
		t.Fatalf("Locator: %v", err)
	}
	if want := "/data/backups/backup.dump"; got != want {
		t.Errorf("Locator = %q, want %q", got, want)
	}
}

// TestNewLocalRefusesAnEmptyRoot keeps a missing local_path from silently
// resolving to a bare basename in the process's working directory, which
// is the shape of mistake that writes an artifact somewhere nobody is
// backing up. Refusing at construction rather than at the first Locator
// call means a store that exists is a store that knows where it is.
func TestNewLocalRefusesAnEmptyRoot(t *testing.T) {
	if _, err := artifactstore.NewLocal(""); err == nil {
		t.Fatal("expected a refusal for an empty root, got nil")
	}
}

// TestZeroLocalRefusesToLocate covers the one way to get a rootless store
// past NewLocal: a composite literal. It is still refused, so the
// precondition holds however the value was built.
func TestZeroLocalRefusesToLocate(t *testing.T) {
	if _, err := (artifactstore.Local{}).Locator(testArtifact(t, "backup.dump")); err == nil {
		t.Fatal("expected the zero Local to refuse to locate anything, got nil")
	}
}

func TestLocalStatReportsSizeAndAbsence(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "present.dump")
	if err := os.WriteFile(present, []byte("twelve bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := artifactstore.Local{}.Stat(context.Background(), present)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if st.Size == nil || *st.Size != 12 {
		t.Errorf("Size = %v, want 12", st.Size)
	}
	if st.ModTime == nil {
		t.Error("ModTime is nil, want the file's modification time")
	}

	_, err = artifactstore.Local{}.Stat(context.Background(), filepath.Join(dir, "absent.dump"))
	if !errors.Is(err, artifactstore.ErrNotPresent) {
		t.Errorf("Stat on an absent file = %v, want ErrNotPresent", err)
	}
}

// TestLocalStatRefusesASymlink covers the same anomaly the prune path
// treats as disqualifying: Commit never produces a symlink at a final
// name, so one found there describes a file this store did not place.
func TestLocalStatRefusesASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.dump")
	if err := os.WriteFile(target, []byte("real"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.dump")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable here: %v", err)
	}

	if _, err := (artifactstore.Local{}).Stat(context.Background(), link); err == nil {
		t.Fatal("expected a refusal for a symlink at an artifact's final path, got nil")
	}
}

func TestLocalOpenReadsTheBytesAndReportsAbsence(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.dump")
	if err := os.WriteFile(p, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}

	rc, err := artifactstore.Local{}.Open(context.Background(), p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload" {
		t.Errorf("read %q, want %q", got, "payload")
	}

	if _, err := (artifactstore.Local{}).Open(context.Background(), filepath.Join(dir, "absent")); !errors.Is(err, artifactstore.ErrNotPresent) {
		t.Errorf("Open on an absent file = %v, want ErrNotPresent", err)
	}
}

// TestLocalPutIsAtomicallyNamed proves a reader never sees a partially
// written artifact under its final name, which is the property a mover
// would depend on when it confirms a destination copy before removing the
// origin.
func TestLocalPutIsAtomicallyNamed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "put.dump")

	if err := (artifactstore.Local{}).Put(context.Background(), p, bytes.NewReader([]byte("written"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "written" {
		t.Errorf("content = %q, want %q", got, "written")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("Put left %d entries behind (%v), want only the artifact", len(entries), names)
	}
}

// TestLocalPutRefusesToOverwriteAnExistingArtifact is the one operation
// in this package that could destroy an artifact's bytes, so it does not
// have the destructive spelling.
//
// The package doc argues at length that the dangerous ordering has to
// require adding a method rather than calling the existing ones in the
// wrong sequence. A Put that renames over whatever is already there would
// have broken that argument on the write side, where a rename-over is not
// recoverable: the mover's step 2 is a confirmation, and a destination
// that already holds something different is a case a person has to decide
// about, not one a library gets to settle silently.
func TestLocalPutRefusesToOverwriteAnExistingArtifact(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "put.dump")
	if err := os.WriteFile(p, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := artifactstore.Local{}.Put(context.Background(), p, bytes.NewReader([]byte("replacement")))
	if !errors.Is(err, artifactstore.ErrAlreadyPresent) {
		t.Fatalf("Put over an existing artifact = %v, want ErrAlreadyPresent", err)
	}

	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Errorf("Put replaced the bytes at an occupied locator: content = %q, want %q", got, "original")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("a refused Put left %d entries behind, want only the original artifact", len(entries))
	}
}

// TestLocalPutFsyncsTheDirectoryEntryItCreated proves Put discharges the
// durability half of the ordering the package doc promises, not just the
// atomicity half.
//
// Without the directory fsync, a crash between the origin's Remove
// reaching disk and the destination's new directory entry reaching disk
// leaves ZERO copies, which is the one outcome the doc says is
// impossible. commit.go's FR-14 treatment is the long version of why a
// directory is a separate inode with its own writeback state.
//
// The test arranges a directory that can be written to and linked into
// but not opened: write and execute, no read. Put can create its temp
// file and link it into place, and then cannot fsync the directory, so a
// Put that reports success there is a Put that skipped the fsync.
func TestLocalPutFsyncsTheDirectoryEntryItCreated(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which ignores the directory permissions this test relies on")
	}

	dir := filepath.Join(t.TempDir(), "write-only")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// t.TempDir's own cleanup has to be able to read this back.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}

	err := artifactstore.Local{}.Put(context.Background(), filepath.Join(dir, "a.dump"), bytes.NewReader([]byte("x")))
	if err == nil {
		t.Fatal("Put reported success on a directory it cannot fsync, so the new directory entry is not durable")
	}
}

// TestLocalRemoveIsIdempotent: the caller's intent is that these bytes
// not be in this store, and that is already true of a file that is gone.
func TestLocalRemoveIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "gone.dump")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := (artifactstore.Local{}).Remove(context.Background(), p); err != nil {
		t.Fatalf("first Remove: %v", err)
	}
	if err := (artifactstore.Local{}).Remove(context.Background(), p); err != nil {
		t.Fatalf("second Remove on an absent file: %v, want nil", err)
	}
}

// TestSeamOffersNoMoveMethod is a design assertion, not a behavioural
// one.
//
// The seam deliberately offers Put, Stat, Open and Remove and no Move, so
// that the put-then-confirm-then-remove ordering a mover needs lives in
// one auditable place rather than being re-decided inside every adapter.
// A single Move call makes the dangerous ordering (remove before the
// destination copy is confirmed) expressible in one line, and that is the
// one shape that can leave zero copies of a backup.
//
// The first version of this test asserted against one exact signature,
// Move(context.Context, string, string) error, on any(Local{}). It
// therefore missed every other spelling, including the most likely real
// one: an artifact-addressed Move, which is the shape this package's own
// doc says operations should have. It also inspected Local's method set
// rather than Store's, so a Move on the interface itself would have
// slipped through in the other direction.
//
// So this matches on the method NAME alone, which is what the decision is
// actually about, and it checks the interface and the implementation,
// because a Move nobody can reach through Store is still a Move anyone
// holding a Local can call. If this is failing because someone added one,
// read the package doc before deleting the test.
func TestSeamOffersNoMoveMethod(t *testing.T) {
	types := []reflect.Type{
		reflect.TypeOf((*artifactstore.Store)(nil)).Elem(),
		reflect.TypeOf(artifactstore.Local{}),
		reflect.TypeOf(&artifactstore.Local{}),
	}

	for _, typ := range types {
		if typ.NumMethod() == 0 {
			t.Fatalf("%s reports no methods at all, so this gate would pass vacuously", typ)
		}
		for i := 0; i < typ.NumMethod(); i++ {
			m := typ.Method(i)
			if m.Name == "Move" {
				t.Errorf("%s gained a Move method (%s); see this package's doc for why the seam offers put, confirm and remove separately", typ, m.Type)
			}
		}
	}
}

// TestLifecycleUsesOnlyTheSharedFormulaFromThisPackage guards the other
// half of the same decision, the half a missing method cannot guard.
//
// Local.Put's own comment says the FR-12 commit path must keep writing
// its own .partial and hard-linking it, because Put does not reproduce
// that path's crash-safety obligations and must not be quietly swapped
// for it. An absent Move takes someone writing code to defeat; a present
// Put takes someone calling it. So lifecycle is allowed exactly one
// symbol from this package, and anything else is a failure that names
// itself.
//
// That one symbol was LocalLocator, the free function taking a directory.
// Since #390 it is NewLocal, because lifecycle now builds a store and asks
// it, which is the conversion #334 deferred. The guard is unchanged in
// substance: NewLocal and the Locator method it returns are pure path
// computation that touch nothing, exactly as the free function was, and
// Put and Remove remain unreachable from lifecycle.
//
// Locator itself is not in this list and does not need to be: it is a
// method on a value, so it appears in the AST as a selector on a local
// variable rather than on the package, and there is no way to reach it
// without first calling something that IS in this list.
func TestLifecycleUsesOnlyTheSharedFormulaFromThisPackage(t *testing.T) {
	const storePath = "github.com/spdrman/rclone-manager/core/internal/artifactstore"
	allowed := map[string]bool{"NewLocal": true}

	entries, err := os.ReadDir(lifecycleDir)
	if err != nil {
		t.Fatalf("reading %s: %v", lifecycleDir, err)
	}

	scanned, referencing := 0, 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++

		file := filepath.Join(lifecycleDir, name)
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}

		local := importedAs(parsed, storePath)
		if local == "" {
			continue
		}
		referencing++

		ast.Inspect(parsed, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok || ident.Name != local {
				return true
			}
			if !allowed[sel.Sel.Name] {
				t.Errorf("%s references artifactstore.%s; lifecycle may use only NewLocal (and the Locator method it returns). Put in particular does not reproduce FR-12's crash-safety obligations, so the commit path must keep writing its own .partial and hard-linking it", file, sel.Sel.Name)
			}
			return true
		})
	}

	if scanned == 0 {
		t.Fatalf("found no non-test Go files under %s, so this gate would pass vacuously", lifecycleDir)
	}
	if referencing == 0 {
		t.Fatalf("no file under %s imports %s any more, so this gate no longer proves anything; delete it or point it somewhere real", lifecycleDir, storePath)
	}
}

// importedAs reports the identifier a file uses for importPath, or "" if
// the file does not import it. It handles a renamed import, because a
// guard that only recognises the default name is a guard one alias
// defeats.
func importedAs(file *ast.File, importPath string) string {
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != importPath {
			continue
		}
		if spec.Name != nil {
			return spec.Name.Name
		}
		return importPath[strings.LastIndex(importPath, "/")+1:]
	}
	return ""
}
