package kopia_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/backupengine"
	"github.com/backupdproject/backupd/core/internal/backupengine/kopia"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/recovery"
	"github.com/backupdproject/backupd/core/internal/secretref"
)

// testDomain is the repository domain every local test opens. It goes
// through the model's own constructor rather than being cast, because that
// is what decides an id is safe to name a directory with, and a test that
// bypassed it would be testing a path the product cannot produce.
func testDomain(t *testing.T, id string) model.RepositoryDomainID {
	t.Helper()

	domain, err := model.NewRepositoryDomainID(id)
	if err != nil {
		t.Fatalf("NewRepositoryDomainID(%q): %v", id, err)
	}

	return domain
}

// passphraseRef writes the test passphrase to a 0600 file and returns the
// reference to it.
//
// There is no way to hand this adapter a literal passphrase, and that is
// the point rather than an inconvenience: if a test could, so could
// production wiring, and the whole custody argument would rest on nobody
// taking the shortcut.
func passphraseRef(t *testing.T, dir string) secretref.Ref {
	t.Helper()

	path := filepath.Join(dir, "passphrase")
	if err := os.WriteFile(path, []byte(testPassphrase+"\n"), 0o600); err != nil {
		t.Fatalf("writing the passphrase file: %v", err)
	}

	return secretref.Ref{File: path}
}

// localLocation is a complete local repository location under root.
func localLocation(t *testing.T, root, domain string) backupengine.RepositoryLocation {
	t.Helper()

	secrets := t.TempDir()

	return backupengine.RepositoryLocation{
		Kind:       backupengine.LocationLocal,
		Domain:     testDomain(t, domain),
		Root:       root,
		StateDir:   filepath.Join(t.TempDir(), "state"),
		Passphrase: passphraseRef(t, secrets),
	}
}

// repoDir is where a local location's blobs actually are, resolved through
// the product's own function rather than composed a second time in the
// tests.
func repoDir(t *testing.T, loc backupengine.RepositoryLocation) string {
	t.Helper()

	dir, err := backupengine.ReservedLocalDir(loc.Root, loc.Domain)
	if err != nil {
		t.Fatalf("ReservedLocalDir: %v", err)
	}

	return dir
}

// TestLocalRepositoryRoundTrip is the lifecycle this issue is about, in
// order: create, open, report, close.
//
// The sequence is the claim. Each of these is only meaningful given the
// one before it, and the numbers are checked against what was actually
// stored rather than against constants, because a stats implementation
// that returns plausible zeroes passes every assertion written the other
// way round.
func TestLocalRepositoryRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	srcDir := filepath.Join(root, "source")
	writeSourceTree(t, srcDir)

	loc := localLocation(t, root, "production")
	eng := kopia.New()

	if err := eng.CreateRepository(ctx, loc); err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}

	// The repository is where the reserved namespace says, not where the
	// caller pointed: loc.Root is the backup root.
	if _, err := os.Stat(filepath.Join(repoDir(t, loc), "kopia.repository.f")); err != nil {
		t.Fatalf("the repository format blob is not under the reserved namespace at %s: %v", repoDir(t, loc), err)
	}

	rep, err := eng.OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	// A fresh repository is reachable, holds nothing, and has nothing to
	// warn about. All three are separate claims.
	health, err := rep.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}

	if !health.Reachable {
		t.Errorf("Health says a repository this process just created and opened is unreachable")
	}

	if len(health.Warnings) != 0 {
		t.Errorf("Health warns about a fresh repository on this machine's own clock: %+v", health.Warnings)
	}

	empty, err := rep.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	if empty.Snapshots != 0 || empty.Sources != 0 {
		t.Errorf("Stats on a fresh repository = %+v, want no sources and no snapshots", empty)
	}

	if empty.Blobs == 0 || empty.PhysicalBytes == 0 {
		t.Errorf("Stats on a fresh repository reports %d blobs and %d bytes; an initialized repository holds at least its format blob",
			empty.Blobs, empty.PhysicalBytes)
	}

	src := backupengine.Source{Host: "repo-host", User: "repo-user", Path: srcDir}

	snap, err := rep.Snapshot(ctx, backupengine.SnapshotRequest{Source: src})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	full, err := rep.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats after a snapshot: %v", err)
	}

	if full.Sources != 1 || full.Snapshots != 1 {
		t.Errorf("Stats after one snapshot of one source = %d source(s), %d snapshot(s); want 1 and 1", full.Sources, full.Snapshots)
	}

	if full.PhysicalBytes <= empty.PhysicalBytes {
		t.Errorf("Stats reports %d physical bytes after storing %d logical bytes, up from %d; a snapshot that cost nothing was not stored",
			full.PhysicalBytes, snap.Bytes, empty.PhysicalBytes)
	}

	if err := rep.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Close is documented as safe twice, and a caller that has to remember
	// whether it already closed is a caller that will get it wrong in a
	// deferred function.
	if err := rep.Close(ctx); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestRepositoryReopensByStableIdAfterRestart is the restart requirement,
// and it is tested by taking away everything a restart does not keep.
//
// The connection config and the index cache are local state this process
// wrote; a new process may find them stale, or on another machine, or
// deleted by an operator clearing disk space. What survives is the backup
// root and the repository domain id an operator declared, so those are the
// only two things the second open is given.
func TestRepositoryReopensByStableIdAfterRestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	srcDir := filepath.Join(root, "source")
	writeSourceTree(t, srcDir)

	loc := localLocation(t, root, "production")

	if err := kopia.New().CreateRepository(ctx, loc); err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}

	first, err := kopia.New().OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	src := backupengine.Source{Host: "repo-host", User: "repo-user", Path: srcDir}

	snap, err := first.Snapshot(ctx, backupengine.SnapshotRequest{Source: src})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if err := first.Close(ctx); err != nil {
		t.Fatalf("closing before the restart: %v", err)
	}

	// The restart: every byte of local state about this repository is gone.
	if err := os.RemoveAll(loc.StateDir); err != nil {
		t.Fatalf("clearing the state directory: %v", err)
	}

	second, err := kopia.New().OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("reopening by id with no local state: %v", err)
	}

	t.Cleanup(func() {
		if err := second.Close(context.Background()); err != nil {
			t.Errorf("closing after the restart: %v", err)
		}
	})

	found, err := second.LookupSnapshot(ctx, snap.ID)
	if err != nil {
		t.Fatalf("LookupSnapshot(%s) after a restart: %v", snap.ID, err)
	}

	if found.ID != snap.ID || found.Bytes != snap.Bytes {
		t.Errorf("the snapshot came back as %+v, want the id and size stored before the restart (%s, %d bytes)",
			found, snap.ID, snap.Bytes)
	}
}

// TestLookupSnapshotRefusesAnUnknownId is the other half of the lookup
// contract: the catalog holds ids for snapshots retention may already have
// deleted, and "not there" has to be distinguishable from "the repository
// is broken".
func TestLookupSnapshotRefusesAnUnknownId(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	rep, _, snap := singleSnapshotRepository(t, 1<<20)

	if err := rep.DeleteSnapshot(ctx, snap.ID); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}

	if _, err := rep.LookupSnapshot(ctx, snap.ID); !errors.Is(err, backupengine.ErrSnapshotNotFound) {
		t.Errorf("LookupSnapshot of a deleted snapshot = %v, want ErrSnapshotNotFound", err)
	}
}

// TestClockSkewIsAWarningNotARefusal is the clock-health requirement.
//
// The skew is injected through the adapter's clock rather than by changing
// the machine's time, and the assertion is deliberately two-sided: the
// warning fires, AND the repository is still fully usable, because the
// wrong fix for a clock problem is a product that stops backing up when
// NTP is unreachable.
func TestClockSkewIsAWarningNotARefusal(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	srcDir := filepath.Join(root, "source")
	writeSourceTree(t, srcDir)

	loc := localLocation(t, root, "production")

	// Two hours fast: far outside the tolerance, and the direction that
	// causes the worse failure, because a clock that is ahead makes
	// another process's live maintenance lock look expired.
	skewed := kopia.New(kopia.WithClock(func() time.Time { return time.Now().Add(2 * time.Hour) }))

	if err := skewed.CreateRepository(ctx, loc); err != nil {
		t.Fatalf("CreateRepository under a skewed clock: %v", err)
	}

	rep, err := skewed.OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("OpenRepository under a skewed clock: %v", err)
	}

	t.Cleanup(func() {
		if err := rep.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	health, err := rep.Health(ctx)
	if err != nil {
		t.Fatalf("Health under a skewed clock returned an error; skew is a warning: %v", err)
	}

	if !health.Reachable {
		t.Errorf("Health reports a skewed clock as unreachable storage")
	}

	var found *backupengine.HealthWarning

	for i, w := range health.Warnings {
		if w.Kind == backupengine.HealthWarningClockSkew {
			found = &health.Warnings[i]
		}
	}

	if found == nil {
		t.Fatalf("Health under a two-hour clock skew reported %+v; want a %s warning",
			health.Warnings, backupengine.HealthWarningClockSkew)
	}

	// The detail is what an operator reads, so it has to say what was
	// measured rather than only that something was.
	if !strings.Contains(found.Detail, "2h") {
		t.Errorf("the clock-skew warning does not say how far out the clock is: %q", found.Detail)
	}

	// And the repository still works, which is the half that keeps this a
	// warning.
	src := backupengine.Source{Host: "repo-host", User: "repo-user", Path: srcDir}
	if _, err := rep.Snapshot(ctx, backupengine.SnapshotRequest{Source: src}); err != nil {
		t.Errorf("Snapshot refused to run under a clock-skew warning: %v", err)
	}
}

// TestReservedNamespaceIsInvisibleToArtifactManagement is the rule in
// backupengine/reserved.go proved against a real repository holding real
// pack and index blobs.
//
// The load-bearing assertion is the walk: EVERY file under the backup root
// is either one this test put there as an artifact, or inside the reserved
// namespace. That is what makes the predicate artifact management consults
// sufficient rather than merely present -- a future change that wrote a
// cache file, a lock or a log beside the artifacts would fail here, which
// is the only place it would fail before a prune deleted it.
func TestReservedNamespaceIsInvisibleToArtifactManagement(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()

	// An ordinary backup set's local directory, with one artifact and the
	// sidecar manifest catalog rebuild reads.
	setDir := filepath.Join(root, "production", "postgres-primary")
	if err := os.MkdirAll(setDir, 0o750); err != nil {
		t.Fatalf("creating the backup set directory: %v", err)
	}

	const artifactBody = "an artifact nobody should confuse with a pack file"

	artifact := filepath.Join(setDir, "dump.tar.gz")
	mustWrite(t, artifact, []byte(artifactBody))

	now := time.Now().UTC()
	if err := recovery.WriteManifest(setDir, recovery.Manifest{
		FormatVersion:      recovery.CurrentFormatVersion,
		Source:             "production",
		BackupSet:          "postgres-primary",
		ArtifactName:       "dump.tar.gz",
		RemotePath:         "/srv/dumps/dump.tar.gz",
		ReceivedTimestamp:  now,
		RetentionTimestamp: now,
		SizeBytes:          int64(len(artifactBody)),
		Checksum:           strings.Repeat("a", 64),
		ChecksumAlgorithm:  "sha256",
	}); err != nil {
		t.Fatalf("writing the sidecar manifest: %v", err)
	}

	// A real repository in the same backup root, with content in it, so
	// the walk below has pack and index blobs to find.
	srcDir := filepath.Join(t.TempDir(), "source")
	writeSourceTree(t, srcDir)

	loc := localLocation(t, root, "production")
	// Deliberately the DERIVED state directory, so the config file and the
	// index cache are part of what this test has to account for.
	loc.StateDir = ""

	eng := kopia.New()

	if err := eng.CreateRepository(ctx, loc); err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}

	rep, err := eng.OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	if _, err := rep.Snapshot(ctx, backupengine.SnapshotRequest{
		Source: backupengine.Source{Host: "repo-host", User: "repo-user", Path: srcDir},
	}); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if err := rep.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The catalog-rebuild reader, pointed at the set's own directory, sees
	// exactly the one artifact and nothing the engine wrote.
	manifests, errs := recovery.ScanManifests(setDir)
	if len(errs) != 0 {
		t.Fatalf("ScanManifests reported %d error(s): %v", len(errs), errs)
	}

	if len(manifests) != 1 || manifests[0].ArtifactName != "dump.tar.gz" {
		t.Fatalf("ScanManifests found %d manifest(s) %v; want exactly the one artifact", len(manifests), manifests)
	}

	// And every byte under the backup root is accounted for: an artifact,
	// its sidecar, or reserved.
	var strays []string

	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if d.IsDir() {
			return nil
		}

		if backupengine.LocalPathIsReserved(root, path) {
			return nil
		}

		if strings.HasPrefix(path, setDir+string(filepath.Separator)) {
			return nil
		}

		strays = append(strays, path)

		return nil
	}); err != nil {
		t.Fatalf("walking the backup root: %v", err)
	}

	if len(strays) != 0 {
		t.Errorf("the engine left %d file(s) under the backup root that artifact management would treat as artifacts: %v",
			len(strays), strays)
	}

	// The predicate is not vacuously true either: the artifact this test
	// planted must NOT be reserved, or "reserved" would just mean
	// "everything".
	if backupengine.LocalPathIsReserved(root, artifact) {
		t.Errorf("LocalPathIsReserved says an artifact at %s is repository internals", artifact)
	}

	// And a path that merely starts with the same characters is outside.
	if backupengine.LocalPathIsReserved(root, filepath.Join(root, ".backupdata", "x")) {
		t.Errorf("LocalPathIsReserved matches on a string prefix rather than a path boundary")
	}
}

// TestApprovedDomainAdmitsSeveralBackupSets is the co-tenancy requirement,
// and it is the composition of two halves that are each already tested
// elsewhere: the model decides whether sharing is allowed, and the adapter
// stores what it is given.
//
// What is proved here is that those two halves agree -- that two sets a
// shared domain admits really do end up in one repository, deduplicating
// against each other, and that an isolated domain's refusal is not
// something the storage layer can route around.
func TestApprovedDomainAdmitsSeveralBackupSets(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()

	shared := model.RepositoryDomain{
		ID:          testDomain(t, "production"),
		Description: "everything that may share one key",
		Isolation:   model.RepositoryShared,
	}

	first := repositoryRef(t, shared.ID, "production", "postgres-primary")
	second := repositoryRef(t, shared.ID, "production", "postgres-replica")

	if err := shared.MayShare(first, second); err != nil {
		t.Fatalf("a shared domain refused two sets: %v", err)
	}

	loc := localLocation(t, root, shared.ID.String())
	eng := kopia.New()

	if err := eng.CreateRepository(ctx, loc); err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}

	rep, err := eng.OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	t.Cleanup(func() {
		if err := rep.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	// Two sets whose trees are byte-identical, which is the case
	// co-tenancy exists for: two hosts running the same software, holding
	// much of the same data. writeSourceTree fills its files with random
	// bytes, so the second tree is a COPY of the first rather than a
	// second call, or there would be nothing to deduplicate and this test
	// would be asserting that random data compresses.
	dirA := filepath.Join(t.TempDir(), "a")
	dirB := filepath.Join(t.TempDir(), "b")
	writeSourceTree(t, dirA)
	copyTree(t, dirA, dirB)

	sourceA := backupengine.Source{Host: "host-a", User: "backupd", Path: dirA}
	sourceB := backupengine.Source{Host: "host-b", User: "backupd", Path: dirB}

	if _, err := rep.Snapshot(ctx, backupengine.SnapshotRequest{Source: sourceA}); err != nil {
		t.Fatalf("Snapshot of the first set: %v", err)
	}

	afterFirst, err := rep.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	if _, err := rep.Snapshot(ctx, backupengine.SnapshotRequest{Source: sourceB}); err != nil {
		t.Fatalf("Snapshot of the second set: %v", err)
	}

	afterSecond, err := rep.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	if afterSecond.Sources != 2 || afterSecond.Snapshots != 2 {
		t.Errorf("after two sets snapshotted into one repository: %d source(s), %d snapshot(s); want 2 and 2",
			afterSecond.Sources, afterSecond.Snapshots)
	}

	// The second set's identical content is stored once. The margin is
	// generous because a second snapshot always writes new metadata; what
	// it must not do is write the content again.
	grew := afterSecond.PhysicalBytes - afterFirst.PhysicalBytes
	if grew > afterFirst.PhysicalBytes/2 {
		t.Errorf("the second set added %d bytes to a repository of %d; identical content in a shared domain is not being deduplicated",
			grew, afterFirst.PhysicalBytes)
	}

	// An isolated domain refuses the same pair, and it does so in the
	// model, before any storage exists to route around.
	isolated := shared
	isolated.Isolation = model.RepositoryIsolated

	if err := isolated.MayShare(first, second); !errors.Is(err, model.ErrIsolationViolated) {
		t.Errorf("an isolated domain admitted two sets: %v", err)
	}
}

// repositoryRef builds one backup set's reference to a domain.
func repositoryRef(t *testing.T, domain model.RepositoryDomainID, source, set string) model.RepositoryRef {
	t.Helper()

	id, err := model.NewBackupSetID(source, set)
	if err != nil {
		t.Fatalf("NewBackupSetID(%q, %q): %v", source, set, err)
	}

	return model.RepositoryRef{Domain: domain, Set: id}
}

// TestLocationRefusalsNameWhatIsMissing covers the refusals that happen
// before any storage is touched.
//
// Each row is a distinct operator mistake with a distinct fix, which is
// why they are asserted on the message rather than only on "an error
// happened": a location refused for the wrong stated reason sends somebody
// to edit the wrong line of their configuration.
func TestLocationRefusalsNameWhatIsMissing(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	valid := localLocation(t, root, "production")

	noDomain := valid
	noDomain.Domain = ""

	noRoot := valid
	noRoot.Root = ""

	relativeRoot := valid
	relativeRoot.Root = "relative/backup-root"

	noPassphrase := valid
	noPassphrase.Passphrase = secretref.Ref{}

	twoSources := valid
	twoSources.Passphrase = secretref.Ref{File: "/tmp/one", Env: "TWO"}

	unknownKind := valid
	unknownKind.Kind = "sftp"

	bucketWithAPath := valid
	bucketWithAPath.Kind = backupengine.LocationS3
	bucketWithAPath.S3 = backupengine.S3Storage{
		Bucket:      "nas-backups/repository",
		Region:      "us-east-1",
		Credentials: secretref.Ref{File: "/tmp/creds"},
	}

	endpointWithAPath := valid
	endpointWithAPath.Kind = backupengine.LocationS3
	endpointWithAPath.S3 = backupengine.S3Storage{
		Bucket:      "nas-backups",
		Endpoint:    "https://minio.example:9000/nas-backups",
		Credentials: secretref.Ref{File: "/tmp/creds"},
	}

	bucketURLAsEndpoint := valid
	bucketURLAsEndpoint.Kind = backupengine.LocationS3
	bucketURLAsEndpoint.S3 = backupengine.S3Storage{
		Bucket:      "nas-backups",
		Endpoint:    "s3://nas-backups",
		Credentials: secretref.Ref{File: "/tmp/creds"},
	}

	s3WithNoState := valid
	s3WithNoState.Kind = backupengine.LocationS3
	s3WithNoState.StateDir = ""
	s3WithNoState.S3 = backupengine.S3Storage{
		Bucket:      "nas-backups",
		Region:      "us-east-1",
		Credentials: secretref.Ref{File: "/tmp/creds"},
	}

	for _, tc := range []struct {
		name string
		loc  backupengine.RepositoryLocation
		says string
	}{
		{"no domain", noDomain, "repository domain"},
		{"no backup root", noRoot, "backup root"},
		{"a relative backup root", relativeRoot, "relative"},
		{"no passphrase source", noPassphrase, "passphrase"},
		{"two passphrase sources", twoSources, "more than one secret source"},
		{"an unsupported kind", unknownKind, `unsupported repository kind "sftp"`},
		{"a bucket with a path in it", bucketWithAPath, "path separator"},
		{"an endpoint with a path in it", endpointWithAPath, "bucket goes in the bucket field"},
		{"a bucket URL as the endpoint", bucketURLAsEndpoint, `scheme "s3"`},
		{"an s3 location with no state directory", s3WithNoState, "state directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := kopia.New().CreateRepository(ctx, tc.loc)
			if err == nil {
				t.Fatalf("CreateRepository accepted %+v", tc.loc)
			}

			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the refusal does not say %q: %v", tc.says, err)
			}
		})
	}
}

// TestRepositorySurfacesNeverCarryTheSecret is the leak assertion against
// this adapter's own output surfaces, with a real passphrase and real
// credentials in play.
//
// The surfaces checked are the ones a leak would actually travel on: the
// errors this adapter returns, the location it keeps for the life of a
// handle, and the stats and health reports something will eventually
// render into an API response.
func TestRepositorySurfacesNeverCarryTheSecret(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	root := t.TempDir()
	loc := localLocation(t, root, "production")

	// An s3 location as well, because its credential resolution is the
	// path that has material in hand while building provider options.
	credsDir := t.TempDir()
	credsFile := filepath.Join(credsDir, "credentials")

	if err := os.WriteFile(credsFile, []byte(
		"[default]\naws_access_key_id = AKIALEAKCANARY00000\naws_secret_access_key = "+leakCanary+"\n"), 0o600); err != nil {
		t.Fatalf("writing the credentials file: %v", err)
	}

	s3loc := loc
	s3loc.Kind = backupengine.LocationS3
	s3loc.S3 = backupengine.S3Storage{
		// A port nothing is listening on, so the open fails while holding
		// resolved credentials, which is the moment a leak happens.
		Endpoint:    "http://127.0.0.1:1/",
		Region:      "us-east-1",
		Bucket:      "nas-backups",
		Credentials: secretref.Ref{File: credsFile},
	}

	eng := kopia.New()

	if err := eng.CreateRepository(ctx, loc); err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}

	rep, err := eng.OpenRepository(ctx, loc)
	if err != nil {
		t.Fatalf("OpenRepository: %v", err)
	}

	t.Cleanup(func() {
		if err := rep.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	health, err := rep.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}

	stats, err := rep.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}

	// A wrong passphrase, so the refusal is built with the resolved
	// material available.
	wrong := loc
	wrong.Passphrase = passphraseRef(t, t.TempDir())

	if err := os.WriteFile(wrong.Passphrase.File, []byte(leakCanary), 0o600); err != nil {
		t.Fatalf("writing the wrong passphrase: %v", err)
	}

	_, wrongErr := eng.OpenRepository(ctx, wrong)
	if wrongErr == nil {
		t.Fatalf("OpenRepository accepted the wrong passphrase")
	}

	s3OpenErr := eng.CreateRepository(ctx, s3loc)
	if s3OpenErr == nil {
		t.Fatalf("CreateRepository succeeded against a closed port")
	}

	for _, surface := range []struct {
		label string
		text  string
	}{
		{"the wrong-passphrase refusal", fmt.Sprintf("%v", wrongErr)},
		{"the s3 connection failure", fmt.Sprintf("%v", s3OpenErr)},
		{"the location, rendered", fmt.Sprintf("%+v", s3loc)},
		{"the health report", fmt.Sprintf("%+v", health)},
		{"the stats report", fmt.Sprintf("%+v", stats)},
	} {
		if strings.Contains(surface.text, leakCanary) {
			t.Errorf("%s carries the secret: %s", surface.label, surface.text)
		}

		if strings.Contains(surface.text, testPassphrase) {
			t.Errorf("%s carries the repository passphrase: %s", surface.label, surface.text)
		}
	}
}

// leakCanary is the material the leak assertions look for. It is one
// distinctive token so a partial or truncated leak is still found.
const leakCanary = "canary-secret-nobody-may-print-3f91"

// copyTree copies a source tree byte for byte, so two backup sets can be
// given identical content without depending on a generator producing the
// same random bytes twice.
func copyTree(t *testing.T, from, to string) {
	t.Helper()

	if err := filepath.WalkDir(from, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}

		dest := filepath.Join(to, rel)

		if d.IsDir() {
			return os.MkdirAll(dest, 0o750)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		return os.WriteFile(dest, data, 0o640)
	}); err != nil {
		t.Fatalf("copying %s to %s: %v", from, to, err)
	}
}
