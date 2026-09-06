package rclone

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/operations"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file tests a pair of operations that cannot be tested against the
// thing they act on, and the way it works around that is the point.
//
// rclone's s3 `restore` command is addressed by a REMOTE and restores
// every archived object it walks under it. The adapter roots its Fs at the
// bucket, so the confinement (NoTraverse plus a files-from filter) is the
// only thing between one restore request and a per-object retrieval charge
// for every backup the deployment holds, accepted by the provider before
// anything here could notice and not cancellable afterwards. That is a
// bill, not a slow test, so it has to be proved rather than reasoned
// about.
//
// MinIO cannot emulate a Glacier restore, so proving it against a real
// endpoint is not available. What IS available is the enumeration itself:
// the confinement is implemented in rclone's own walk layer, so running
// operations.ListFn over a real local Fs under the same confined context
// answers exactly the question that costs money, "which objects would that
// command have acted on", against a directory holding three. The local
// backend is a stand-in for the walk and for nothing else; no case here
// claims a directory can be restored.
//
// The other half is that neither command FAILS the way a Go caller
// expects. `restore` returns nil and a per-object list whose Status field
// carries the refusal, so a caller checking only err believes a restore
// started and then waits hours for it. checkRestoreAccepted is tested as a
// free function for that reason: the three answers that are not a success
// are answers a real endpoint gives rarely and expensively.

// localFsWith builds a real rclone local-backend Fs over a directory
// holding names, so the tests below run the same walk code the s3 restore
// command runs rather than a re-implementation of it.
//
// The local backend is used as a STAND-IN for the enumeration, and only
// for the enumeration. Nothing here claims a local directory can be
// restored; what it can do is answer "which objects would that command
// have acted on", which is the question that costs money to get wrong.
func localFsWith(t *testing.T, names ...string) (fs.Fs, string) {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		full := filepath.Join(dir, filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte("bytes of "+n), 0o600); err != nil {
			t.Fatalf("writing %s: %v", full, err)
		}
	}
	f, err := fs.NewFs(context.Background(), dir)
	if err != nil {
		t.Fatalf("building a local Fs over %s: %v", dir, err)
	}
	t.Cleanup(func() { shutdownFs(context.Background(), f) })
	return f, dir
}

// visitedUnder runs the exact enumeration rclone's s3 `restore` command
// runs (operations.ListFn over the Fs) and reports what it visited.
func visitedUnder(t *testing.T, ctx context.Context, f fs.Fs) []string {
	t.Helper()
	var seen []string
	if err := operations.ListFn(ctx, f, func(o fs.Object) {
		seen = append(seen, o.Remote())
	}); err != nil {
		t.Fatalf("enumerating: %v", err)
	}
	sort.Strings(seen)
	return seen
}

// TestARestoreVisitsExactlyTheObjectItNames is the money test in this
// file, and the reason confinedContext exists at all.
//
// rclone's s3 `restore` is addressed by a REMOTE and restores every
// archived object it walks under it. This adapter's Fs is rooted at the
// medium's BUCKET. So the difference between a confined context and an
// unconfined one is the difference between restoring one backup and
// paying a per-object retrieval charge on every backup the deployment
// holds, accepted by the provider before anything here could intervene.
//
// The directory deliberately holds three objects and a nested one, so a
// confinement that only half worked (a prefix match, a top-level-only
// filter) fails here rather than passing and being discovered on a bill.
func TestARestoreVisitsExactlyTheObjectItNames(t *testing.T) {
	f, _ := localFsWith(t, "alpha.tar.gz", "beta.tar.gz", "gamma.tar.gz", "old/beta.tar.gz")

	confined, err := confinedContext(context.Background(), "beta.tar.gz")
	if err != nil {
		t.Fatalf("confinedContext: %v", err)
	}

	got := visitedUnder(t, confined, f)
	want := []string{"beta.tar.gz"}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("a confined restore would have acted on %v, want exactly %v", got, want)
	}
}

// TestAnUnconfinedRestoreWouldTakeTheWholeBucket is the positive control
// for the test above: it proves the fixture actually holds more than one
// object, so the assertion up there is about the confinement rather than
// about an empty directory.
//
// Without this, deleting the filter and ALSO breaking the fixture would
// leave both tests green, which is the shape of vacuous proof this
// repository keeps finding.
func TestAnUnconfinedRestoreWouldTakeTheWholeBucket(t *testing.T) {
	f, _ := localFsWith(t, "alpha.tar.gz", "beta.tar.gz", "gamma.tar.gz", "old/beta.tar.gz")

	got := visitedUnder(t, context.Background(), f)
	if len(got) != 4 {
		t.Fatalf("the unconfined walk visited %v; this fixture is meant to hold four objects, so the confinement test above would prove nothing", got)
	}
}

// TestAConfinedRestoreCannotBeSilentlySkipped pins the other silent
// failure on this path.
//
// rclone's restore loop calls operations.SkipDestructive, and when that
// answers true it SKIPS the object and leaves its per-object status as
// "OK". A caller reading that list is told a restore started that never
// did, and then waits hours for a state change that cannot arrive.
//
// The parent context here has --dry-run set on purpose. fs.AddConfig
// COPIES the parent's config, so a caller that had set it would carry it
// straight into the restore; confinedContext clearing it is what makes
// that unreachable rather than merely unlikely.
func TestAConfinedRestoreCannotBeSilentlySkipped(t *testing.T) {
	parent, parentConfig := fs.AddConfig(context.Background())
	parentConfig.DryRun = true
	parentConfig.Interactive = true

	confined, err := confinedContext(parent, "beta.tar.gz")
	if err != nil {
		t.Fatalf("confinedContext: %v", err)
	}

	if operations.SkipDestructive(confined, "beta.tar.gz", "restore") {
		t.Fatal("rclone would skip the restore and still report its status as OK, which is a success this product never had")
	}
	if got := fs.GetConfig(confined); !got.NoTraverse {
		t.Error("NoTraverse is off, so the restore finds its object by listing the bucket rather than by asking for it")
	}
}

// TestARestoreThatWasNotAcceptedIsNotReportedAsStarted covers the second
// way this pair fails quietly: rclone's `restore` returns a nil error and
// puts the refusal in the per-object Status field.
//
// The empty case is the one worth staring at. rclone lists what it walked,
// so an empty answer means the filter matched nothing, which means there
// is no such object. Reporting that as a started restore leaves an
// operation row running against a job nobody is doing.
func TestARestoreThatWasNotAcceptedIsNotReportedAsStarted(t *testing.T) {
	for _, tc := range []struct {
		name     string
		out      any
		wantErr  bool
		wantKind transport.Category
	}{
		{
			name: "the provider accepted it",
			out:  []map[string]string{{"Status": "OK", "Remote": "beta.tar.gz"}},
		},
		{
			name:     "nothing was there to restore",
			out:      []map[string]string{},
			wantErr:  true,
			wantKind: transport.NotFound,
		},
		{
			name:     "the class does not need or allow a restore",
			out:      []map[string]string{{"Status": "Not GLACIER or DEEP_ARCHIVE or INTELLIGENT_TIERING storage class", "Remote": "beta.tar.gz"}},
			wantErr:  true,
			wantKind: transport.UnsupportedCapability,
		},
		{
			name: "the confinement did not hold",
			out: []map[string]string{
				{"Status": "OK", "Remote": "beta.tar.gz"},
				{"Status": "OK", "Remote": "gamma.tar.gz"},
			},
			wantErr:  true,
			wantKind: transport.Unclassified,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkRestoreAccepted(tc.out, "beta.tar.gz")
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("checkRestoreAccepted = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("checkRestoreAccepted reported a started restore, and nothing was started")
			}
			var te *transport.Error
			if !errors.As(err, &te) {
				t.Fatalf("checkRestoreAccepted returned %v, which is not a classified transport error", err)
			}
			if te.Category != tc.wantKind {
				t.Errorf("category = %v, want %v", te.Category, tc.wantKind)
			}
			if !errors.Is(err, errRestoreNotAccepted) {
				t.Errorf("%v does not identify itself as a refused restore", err)
			}
		})
	}
}

// TestRestoreStatusReadsTheProvidersOwnAnswer runs the mapping against the
// exact JSON shape rclone documents for restore-status, including the two
// readings that are not a boolean pair: no RestoreStatus at all, and an
// in-progress restore with no expiry yet.
//
// The wrong-object row is the adversarial half. A recursive listing puts
// "old/beta.tar.gz" in the same answer as "beta.tar.gz", and a match that
// was not exact would report one artifact's restore as another's.
func TestRestoreStatusReadsTheProvidersOwnAnswer(t *testing.T) {
	raw := []any{
		map[string]any{
			"Remote":        "old/beta.tar.gz",
			"VersionID":     nil,
			"RestoreStatus": map[string]any{"IsRestoreInProgress": false, "RestoreExpiryDate": "2099-09-06T12:29:19Z"},
			"StorageClass":  "DEEP_ARCHIVE",
		},
		map[string]any{
			"Remote":        "beta.tar.gz",
			"VersionID":     nil,
			"RestoreStatus": map[string]any{"IsRestoreInProgress": true, "RestoreExpiryDate": nil},
			"StorageClass":  "GLACIER",
		},
		map[string]any{
			"Remote":        "gamma.tar.gz",
			"VersionID":     nil,
			"RestoreStatus": nil,
			"StorageClass":  "DEEP_ARCHIVE",
		},
	}

	var entries []restoreStatusEntry
	if err := decodeCommandOutput(raw, &entries); err != nil {
		t.Fatalf("decoding rclone's own restore-status shape: %v", err)
	}

	running := restoreStateFor(entries, "beta.tar.gz")
	if running == nil {
		t.Fatal("the object rclone said was restoring came back as no restore at all")
	}
	if !running.InProgress {
		t.Error("a restore rclone reported as in progress read as finished")
	}
	if running.ExpiresAt != nil {
		t.Errorf("an expiry of %v was invented for a restore that has not finished", running.ExpiresAt)
	}

	finished := restoreStateFor(entries, "old/beta.tar.gz")
	if finished == nil || finished.InProgress {
		t.Fatalf("the finished restore read as %+v", finished)
	}
	if finished.ExpiresAt == nil {
		t.Fatal("the provider gave an expiry date and it was dropped")
	}
	if want := time.Date(2099, 9, 6, 12, 29, 19, 0, time.UTC); !finished.ExpiresAt.Equal(want) {
		t.Errorf("expiry = %v, want %v", finished.ExpiresAt, want)
	}

	if st := restoreStateFor(entries, "gamma.tar.gz"); st != nil {
		t.Errorf("an object with no restore status reported %+v, and nil is the only honest answer", st)
	}
	if st := restoreStateFor(entries, "delta.tar.gz"); st != nil {
		t.Errorf("an object nobody asked about reported %+v", st)
	}
}

// TestAMediumWithNoArchiveTierRefusesRatherThanPretending is FR-13 on this
// pair. A local directory has no asynchronous retrieval, and answering
// "accepted" would leave a caller waiting hours for something nothing is
// doing.
//
// # Why the medium points at a directory that is not there
//
// Because that is what makes this test about canRestore rather than about
// whatever the local backend happens to do next. An earlier version
// pointed at a real directory, and deleting the medium-type check left it
// green: the refusal simply arrived one step later, when the local Fs
// turned out not to implement rclone's Commander. Same category, same
// sentinel, no signal.
//
// With a directory that does not exist, the two paths give different
// answers. Refusing on the type never touches the filesystem, so the
// answer is still the capability refusal; going on to build an Fs makes
// the missing directory the first thing that fails. So this now pins the
// order as well as the outcome, which is the part that matters: the check
// that costs nothing has to come before the one that does work.
func TestAMediumWithNoArchiveTierRefusesRatherThanPretending(t *testing.T) {
	medium := transport.Medium{
		ID:     "fixture",
		Type:   transport.MediumTypeLocalDir,
		Bucket: filepath.Join(t.TempDir(), "no-such-directory"),
	}
	adapter := New()

	err := adapter.InitiateRestore(context.Background(), medium, "beta.tar.gz", 3)
	if err == nil {
		t.Fatal("a local directory reported that it had started a restore")
	}
	var te *transport.Error
	if !errors.As(err, &te) || te.Category != transport.UnsupportedCapability {
		t.Fatalf("InitiateRestore refused with %v, want an UnsupportedCapability refusal", err)
	}
	if !errors.Is(err, errRestoreUnsupported) {
		t.Errorf("%v does not say that this medium type has no archive tier", err)
	}

	// The read half answers rather than refusing, and it answers the one
	// thing that is true: nothing is restoring here. It must not be an
	// error, because a status read that fails on a medium with no archive
	// tier turns "there is nothing to say" into "something went wrong".
	st, err := adapter.RestoreStatus(context.Background(), medium, "beta.tar.gz")
	if err != nil {
		t.Fatalf("RestoreStatus on a medium with no archive tier: %v", err)
	}
	if st != nil {
		t.Errorf("RestoreStatus reported %+v about a plain directory", st)
	}
}

// TestRestoreScopeRootsWhereRestoreStatusCanBeAfforded pins the split
// between the Fs root and the remote.
//
// restore-status does not obey filters and is bounded only by the Fs root,
// so a root at the bucket is a full-bucket listing on every status poll.
// Under transport.MediumKey's <prefix>/<source>/<set>/<name> layout the
// object's directory is one backup set, which is the smallest thing that
// listing can be bounded to.
func TestRestoreScopeRootsWhereRestoreStatusCanBeAfforded(t *testing.T) {
	for _, tc := range []struct {
		bucket, key        string
		wantRoot, wantLeaf string
	}{
		{"backups", "prefix/nas-a/photos/2026-09-01.tar.gz", "backups/prefix/nas-a/photos", "2026-09-01.tar.gz"},
		{"backups", "nas-a/photos/2026-09-01.tar.gz", "backups/nas-a/photos", "2026-09-01.tar.gz"},
		{"backups", "loose.tar.gz", "backups", "loose.tar.gz"},
	} {
		root, leaf := restoreScope(tc.bucket, tc.key)
		if root != tc.wantRoot || leaf != tc.wantLeaf {
			t.Errorf("restoreScope(%q, %q) = (%q, %q), want (%q, %q)",
				tc.bucket, tc.key, root, leaf, tc.wantRoot, tc.wantLeaf)
		}
	}
}

// TestTheRemoteFollowsTheRootTheBackendActuallyChose covers the s3
// backend's own habit of moving a root out from under the caller.
//
// NewFs HEADs the last segment of the root it is given and, when that
// segment is an object or it cannot tell, hands back an Fs rooted at the
// PARENT instead. Comparing against the root that was ASKED for then
// matches a leaf name against a "set/leaf" remote, finds nothing, and
// reports "no restore in play" about an object that is restoring.
func TestTheRemoteFollowsTheRootTheBackendActuallyChose(t *testing.T) {
	key := "nas-a/photos/2026-09-01.tar.gz"
	for _, tc := range []struct {
		name, root, want string
	}{
		{"the root it asked for", "backups/nas-a/photos", "2026-09-01.tar.gz"},
		{"the backend moved it up a level", "backups/nas-a", "photos/2026-09-01.tar.gz"},
		{"the bucket root", "backups", key},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := remoteRelativeTo(tc.root, "backups", key); got != tc.want {
				t.Errorf("remoteRelativeTo(root=%q) = %q, want %q", tc.root, got, tc.want)
			}
		})
	}
}
