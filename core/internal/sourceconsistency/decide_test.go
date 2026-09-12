// The re-read decision, and the single fact this whole phase turns on:
// kopia v0.23.1 decides an existing file's content can be reused from
// metadata alone, so something above it has to decide when metadata is not
// enough. These tests pin both halves - what the engine would do, and what
// this package does instead - because the gap between them IS the design.

package sourceconsistency

import (
	"fmt"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/model"
)

var now = time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)

// kopiaMetadataReuse reports what the pinned engine would decide on its own:
// whether kopia v0.23.1 would carry this path's content forward from the
// previous snapshot without reading the file.
//
// It is the engine's rule, written here so that the gap between it and
// Decide is executable rather than a paragraph in an ADR. The rule is
// `metadataEquals` in `snapshot/upload/upload.go` at v0.23.1 (lines
// 694-720): the modification time compared with time.Time.Equal at full
// resolution, the mode, the owner, and the size - reached only for an entry
// found under the same name in a previous snapshot's directory
// (`findCachedEntry`, 722-754). No content, no identifier, and no re-hash
// unless `ForceHashPercentage` is raised from its zero default.
//
// It lives in the test file rather than beside Decide deliberately. It is a
// model of somebody else's code, asserted against and never called in a
// run, and exporting it put a function on this package's API surface that
// no caller should ever consult: a caller reaching for "what would kopia
// do" is a caller about to make the decision this package was built to take
// away from them. The finding it states stays executable; it is just no
// longer something production code can depend on.
func kopiaMetadataReuse(prev, cur Entry) bool {
	return prev.Path == cur.Path &&
		prev.Size == cur.Size &&
		prev.ModTimeNanos == cur.ModTimeNanos &&
		prev.Mode == cur.Mode &&
		prev.Owner == cur.Owner
}

// entry is the catalogue row shape these tests build by hand.
func entry(path string, size int64, mtime int64, digest string) Entry {
	return Entry{
		Path:         path,
		Size:         size,
		ModTimeNanos: mtime,
		Mode:         0o600,
		Owner:        "1000:1000",
		Digest:       digest,
		DigestAlg:    DigestAlgorithm,
		VerifiedAt:   now.Add(-time.Hour),
	}
}

// The exact rule kopia v0.23.1 applies, written out so it can be tested
// rather than asserted in prose. It is name, mode, owner, modification time
// and size (snapshot/upload/upload.go:694-720), and nothing else: no
// content, no identifier, no re-hash unless ForceHashPercentage was raised
// from its zero default (upload.go:756-767).
//
// The first case is the one that matters: identical metadata, different
// content, REUSED. That is a changed file the engine would carry forward
// from the previous snapshot unread.
func TestTheKopiaReuseRuleIsMetadataOnlyByConstruction(t *testing.T) {
	prev := entry("report.csv", 4096, 1_757_000_000_000_000_000, "aaaa")
	same := prev
	same.Digest = "bbbb" // different content, nothing else moved

	if !kopiaMetadataReuse(prev, same) {
		t.Fatal("kopiaMetadataReuse refused a pair kopia's metadataEquals accepts; the model of the engine is wrong, not the engine")
	}

	for name, mutate := range map[string]func(e *Entry){
		"size":              func(e *Entry) { e.Size++ },
		"modification time": func(e *Entry) { e.ModTimeNanos++ },
		"mode":              func(e *Entry) { e.Mode = 0o644 },
		"owner":             func(e *Entry) { e.Owner = "0:0" },
		"name":              func(e *Entry) { e.Path = "report.csv.1" },
	} {
		cur := prev
		mutate(&cur)
		if kopiaMetadataReuse(prev, cur) {
			t.Errorf("a changed %s was still reused", name)
		}
	}
}

// One nanosecond of difference is a cache miss, because the comparison is
// time.Time.Equal at full resolution. This is worth pinning because it is
// the half of kopia's heuristic that is STRONGER than the folklore: on a
// filesystem with nanosecond timestamps an ordinary in-place write is seen.
// What is not seen is a write whose timestamp was restored, and no
// resolution helps with that.
func TestKopiaReuseSeesANanosecondButNotARestoredTimestamp(t *testing.T) {
	prev := entry("a", 10, 1_757_000_000_000_000_000, "aaaa")

	oneNano := prev
	oneNano.ModTimeNanos++
	if kopiaMetadataReuse(prev, oneNano) {
		t.Error("a one-nanosecond difference was reused")
	}

	restored := prev
	restored.Digest = "bbbb"
	if !kopiaMetadataReuse(prev, restored) {
		t.Error("a restored timestamp with changed content was not reused; this test's premise is that it is")
	}
}

// The property the acceptance criterion calls "weak-metadata sources have a
// correctness-preserving policy": on the pair kopia would reuse, a weak
// source under the conservative preset reads and verifies. Not sometimes,
// not after an interval - on every such pair, because the policy that
// applies to it does not permit metadata to skip content at all.
func TestOnThePairKopiaWouldReuseAWeakSourceStillReads(t *testing.T) {
	prev := entry("report.csv", 4096, 1_757_000_000_000_000_000, "aaaa")
	cur := prev
	cur.Digest = "" // a fresh listing has metadata and no content yet

	if !kopiaMetadataReuse(prev, cur) {
		t.Fatal("the premise of this test is a pair the engine would reuse")
	}

	pol := model.VerificationPolicyFor(model.TrustWeak, model.PresetConservative)
	got := Decide(prev, cur, pol, now)
	if got.Action != ActionReadAndVerify {
		t.Fatalf("action = %q (%s), want %q", got.Action, got.Reason, ActionReadAndVerify)
	}
}

// Unknown trust is the same answer for both presets, which is the whole
// reason unknown is a separate class from weak: there is no preset under
// which a source whose metadata semantics were never established may skip
// reading content.
func TestUnknownTrustAlwaysReads(t *testing.T) {
	prev := entry("a", 10, 1_757_000_000_000_000_000, "aaaa")
	cur := prev

	for _, preset := range []model.MetadataTrustPreset{model.PresetTrustMetadata, model.PresetConservative} {
		pol := model.VerificationPolicyFor(model.TrustUnknown, preset)
		if got := Decide(prev, cur, pol, now); got.Action != ActionReadAndVerify {
			t.Errorf("preset %q: action = %q (%s), want %q", preset, got.Action, got.Reason, ActionReadAndVerify)
		}
	}
}

// A strong source under the strong preset is the one configuration that
// reuses on metadata, and it does so on the strength of the generation
// identifier rather than the metadata: a matching identifier reuses, a
// differing one reads, and a MISSING one reads. The last is not a detail.
// An s3 object uploaded in multiple parts has an ETag that is not a content
// hash, and a bucket without versioning has no version id, so "the policy
// says remote hash and there is no remote hash for this object" is a real
// per-object state and it has to fall to the cautious side.
func TestRemoteHashPolicyRestsOnEvidenceAndReadsWhenThereIsNone(t *testing.T) {
	pol := model.VerificationPolicyFor(model.TrustStrong, model.PresetTrustMetadata)
	prev := entry("obj", 4096, 1_757_000_000_000_000_000, "aaaa")
	prev.GenerationID = "v1"

	matching := prev
	if got := Decide(prev, matching, pol, now); got.Action != ActionReuse {
		t.Errorf("a matching generation identifier: action = %q (%s), want %q", got.Action, got.Reason, ActionReuse)
	}

	differing := prev
	differing.GenerationID = "v2"
	if got := Decide(prev, differing, pol, now); got.Action != ActionReadAndVerify {
		t.Errorf("a differing generation identifier: action = %q (%s), want %q", got.Action, got.Reason, ActionReadAndVerify)
	}

	absent := prev
	absent.GenerationID = ""
	if got := Decide(prev, absent, pol, now); got.Action != ActionReadAndVerify {
		t.Errorf("no generation identifier on the current listing: action = %q (%s), want %q", got.Action, got.Reason, ActionReadAndVerify)
	}

	neverHadOne := prev
	neverHadOne.GenerationID = ""
	stored := prev
	stored.GenerationID = ""
	if got := Decide(stored, neverHadOne, pol, now); got.Action != ActionReadAndVerify {
		t.Errorf("neither side has one: action = %q (%s), want %q", got.Action, got.Reason, ActionReadAndVerify)
	}
}

// A remote hash is the other strong signal, and it is compared only when
// both sides used the same algorithm. Two digests from different algorithms
// are not a comparison, and treating a mismatch between them as a change
// would re-read the whole source on the day a backend's default changed.
func TestRemoteHashComparisonRequiresTheSameAlgorithm(t *testing.T) {
	pol := model.VerificationPolicyFor(model.TrustStrong, model.PresetTrustMetadata)

	prev := entry("obj", 4096, 1_757_000_000_000_000_000, "aaaa")
	prev.RemoteHash, prev.RemoteHashAlg = "d41d8", "md5"

	same := prev
	if got := Decide(prev, same, pol, now); got.Action != ActionReuse {
		t.Errorf("matching md5: action = %q (%s), want %q", got.Action, got.Reason, ActionReuse)
	}

	changed := prev
	changed.RemoteHash = "9e107"
	if got := Decide(prev, changed, pol, now); got.Action != ActionReadAndVerify {
		t.Errorf("changed md5: action = %q (%s), want %q", got.Action, got.Reason, ActionReadAndVerify)
	}

	otherAlg := prev
	otherAlg.RemoteHashAlg = "sha256"
	if got := Decide(prev, otherAlg, pol, now); got.Action != ActionReadAndVerify {
		t.Errorf("incomparable algorithms: action = %q (%s), want %q", got.Action, got.Reason, ActionReadAndVerify)
	}
}

// Metadata that moved is decisive in the other direction under every
// policy: nothing reuses a row whose size or timestamp disagrees with the
// listing, however strong the source.
func TestMovedMetadataForcesAReadUnderEveryPolicy(t *testing.T) {
	prev := entry("a", 10, 1_757_000_000_000_000_000, "aaaa")
	prev.GenerationID = "v1"

	for _, class := range []model.TrustClass{model.TrustStrong, model.TrustWeak, model.TrustUnknown} {
		for _, preset := range []model.MetadataTrustPreset{model.PresetTrustMetadata, model.PresetConservative} {
			pol := model.VerificationPolicyFor(class, preset)
			for name, mutate := range map[string]func(e *Entry){
				"size":  func(e *Entry) { e.Size = 11 },
				"mtime": func(e *Entry) { e.ModTimeNanos++ },
			} {
				cur := prev
				mutate(&cur)
				if got := Decide(prev, cur, pol, now); got.Action != ActionReadAndVerify {
					t.Errorf("%s/%s changed %s: action = %q (%s), want %q", class, preset, name, got.Action, got.Reason, ActionReadAndVerify)
				}
			}
		}
	}
}

// A path with no previous row is read. Obvious, and worth a case because the
// zero Entry compares equal to itself on every field a reuse decision looks
// at, so a comparison written without this check reuses a file it has never
// seen and stores whatever digest the zero value carried.
func TestAPathWithNoPreviousRowIsRead(t *testing.T) {
	pol := model.VerificationPolicyFor(model.TrustStrong, model.PresetTrustMetadata)
	cur := entry("new", 10, 1_757_000_000_000_000_000, "")
	cur.GenerationID = "v1"

	if got := Decide(Entry{}, cur, pol, now); got.Action != ActionReadAndVerify {
		t.Fatalf("action = %q (%s), want %q", got.Action, got.Reason, ActionReadAndVerify)
	}
}

// The periodic policy's promise is the interval: a row verified longer ago
// than that is read even though nothing about it moved. The boundary is
// asserted on both sides, because "at" and "after" are the difference
// between a full re-read every interval and one that never quite happens.
func TestPeriodicPolicyReadsOnceTheIntervalHasElapsed(t *testing.T) {
	pol := model.VerificationPolicyFor(model.TrustStrong, model.PresetConservative)
	if pol.Mode != model.VerifyPeriodic {
		t.Fatalf("policy mode = %q, want %q", pol.Mode, model.VerifyPeriodic)
	}

	prev := entry("obj", 10, 1_757_000_000_000_000_000, "aaaa")
	prev.GenerationID = "v1"
	cur := prev

	prev.VerifiedAt = now.Add(-pol.ReverifyInterval + time.Minute)
	if got := Decide(prev, cur, pol, now); got.Action != ActionReuse {
		t.Errorf("inside the interval: action = %q (%s), want %q", got.Action, got.Reason, ActionReuse)
	}

	prev.VerifiedAt = now.Add(-pol.ReverifyInterval)
	if got := Decide(prev, cur, pol, now); got.Action != ActionReadAndVerify {
		t.Errorf("exactly at the interval: action = %q (%s), want %q", got.Action, got.Reason, ActionReadAndVerify)
	}

	prev.VerifiedAt = time.Time{}
	if got := Decide(prev, cur, pol, now); got.Action != ActionReadAndVerify {
		t.Errorf("never verified: action = %q (%s), want %q", got.Action, got.Reason, ActionReadAndVerify)
	}
}

// Sampling has to be deterministic for the same path, or a re-read becomes a
// coin flip an operator cannot reason about and a run's cost swings for no
// stated reason. It is a hash of the path, not a random draw.
func TestSamplingIsDeterministicPerPath(t *testing.T) {
	for _, path := range []string{"a", "deep/nested/path.bin", "unicode-ï.txt"} {
		first := sampled(path, 0.05)
		for range 8 {
			if sampled(path, 0.05) != first {
				t.Fatalf("sampled(%q) is not deterministic", path)
			}
		}
	}

	if !sampled("anything", 1) {
		t.Error("a fraction of 1 did not select a path")
	}
	if sampled("anything", 0) {
		t.Error("a fraction of 0 selected a path")
	}
}

// And it has to actually sample: a hash whose low bits are skewed would pass
// the determinism test above while selecting nothing or everything. The band
// is wide because this is a property check, not a chi-squared test.
func TestSamplingSelectsRoughlyTheStatedFraction(t *testing.T) {
	const paths = 2000
	hits := 0
	for i := range paths {
		if sampled(fmt.Sprintf("dir%d/file%d.dat", i%37, i), 0.05) {
			hits++
		}
	}

	if hits < paths/40 || hits > paths/10 {
		t.Fatalf("%d of %d paths selected at a fraction of 0.05; want roughly %d", hits, paths, paths/20)
	}
}

// The sampled policy's floor: a path the sample keeps missing is still read
// once the interval has elapsed. Without that, 95% of a weak source could
// go unverified indefinitely, which is the "skip forever" the policy matrix
// exists to prevent.
func TestSampledPolicyStillReadsEveryPathOnceTheIntervalElapses(t *testing.T) {
	pol := model.VerificationPolicyFor(model.TrustWeak, model.PresetTrustMetadata)

	var unsampled string
	for i := range 10000 {
		p := fmt.Sprintf("file%d", i)
		if !sampled(p, pol.SampleFraction) {
			unsampled = p
			break
		}
	}
	if unsampled == "" {
		t.Fatal("could not find a path the sample misses")
	}

	prev := entry(unsampled, 10, 1_757_000_000_000_000_000, "aaaa")
	cur := prev

	prev.VerifiedAt = now.Add(-pol.ReverifyInterval + time.Minute)
	if got := Decide(prev, cur, pol, now); got.Action != ActionReuse {
		t.Fatalf("unsampled and inside the interval: action = %q (%s), want %q", got.Action, got.Reason, ActionReuse)
	}

	prev.VerifiedAt = now.Add(-pol.ReverifyInterval - time.Minute)
	if got := Decide(prev, cur, pol, now); got.Action != ActionReadAndVerify {
		t.Fatalf("unsampled and past the interval: action = %q (%s), want %q", got.Action, got.Reason, ActionReadAndVerify)
	}
}

// A sampled path is read on its turn even inside the interval, which is what
// makes sampling a verification strategy rather than a slower timer.
func TestSampledPathsAreReadInsideTheInterval(t *testing.T) {
	pol := model.VerificationPolicyFor(model.TrustWeak, model.PresetTrustMetadata)

	var hit string
	for i := range 10000 {
		p := fmt.Sprintf("file%d", i)
		if sampled(p, pol.SampleFraction) {
			hit = p
			break
		}
	}
	if hit == "" {
		t.Fatal("could not find a path the sample selects")
	}

	prev := entry(hit, 10, 1_757_000_000_000_000_000, "aaaa")
	prev.VerifiedAt = now.Add(-time.Minute)
	cur := prev

	if got := Decide(prev, cur, pol, now); got.Action != ActionReadAndVerify {
		t.Fatalf("action = %q (%s), want %q", got.Action, got.Reason, ActionReadAndVerify)
	}
}

// A mode this package does not know about must read rather than reuse. The
// zero VerificationPolicy is one of those, and it is what a caller that
// forgot to derive a policy would pass.
func TestAnUnknownPolicyModeReads(t *testing.T) {
	prev := entry("a", 10, 1_757_000_000_000_000_000, "aaaa")
	cur := prev

	for _, pol := range []model.VerificationPolicy{
		{},
		{Mode: model.VerifyNever, ReverifyInterval: time.Hour},
		{Mode: model.VerificationMode("optimistic"), ReverifyInterval: time.Hour},
	} {
		if got := Decide(prev, cur, pol, now); got.Action != ActionReadAndVerify {
			t.Errorf("policy %#v: action = %q (%s), want %q", pol, got.Action, got.Reason, ActionReadAndVerify)
		}
	}
}

// Every decision says why, because the reason is what a run report shows an
// operator asking why their incremental backup re-read 400 GB.
func TestEveryDecisionCarriesAReason(t *testing.T) {
	prev := entry("a", 10, 1_757_000_000_000_000_000, "aaaa")
	prev.GenerationID = "v1"

	for _, class := range []model.TrustClass{model.TrustStrong, model.TrustWeak, model.TrustUnknown} {
		for _, preset := range []model.MetadataTrustPreset{model.PresetTrustMetadata, model.PresetConservative} {
			pol := model.VerificationPolicyFor(class, preset)
			if got := Decide(prev, prev, pol, now); got.Reason == "" {
				t.Errorf("%s/%s produced action %q with no reason", class, preset, got.Action)
			}
		}
	}
}
