package source_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/internal/backend"
	"github.com/backupdproject/backupd/core/internal/backupengine/source"
	"github.com/backupdproject/backupd/core/internal/model"
	"github.com/backupdproject/backupd/core/internal/sourceconsistency"
	"github.com/backupdproject/backupd/core/internal/transport"
)

func newAdapter(t *testing.T, deps source.Deps, opts source.Options) *source.Adapter {
	t.Helper()

	a, err := source.New(deps, opts)
	if err != nil {
		t.Fatalf("source.New: %v", err)
	}

	return a
}

func depsFor(f *fakeSource) source.Deps {
	return source.Deps{Streamer: f, Stater: f, Enumerator: f, Links: f}
}

func liveOpts() source.Options {
	return source.Options{Mode: model.ModeLiveBestEffort, Preset: model.PresetConservative}
}

// The whole path in the shape a caller uses it: a source with objects on
// it, one call, every object's bytes in the sink under its own path and
// nothing invented on the way.
func TestBackupStreamsEveryObjectUnderItsOwnPath(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("runs/2026/db.dump", patternBytes(3<<20, 1), 1_700_000_000)
	f.put("runs/2026/notes.txt", []byte("hello"), 1_700_000_001)
	f.put("index.json", []byte(`{"ok":true}`), 1_700_000_002)

	sink := newRecordingSink()
	a := newAdapter(t, depsFor(f), liveOpts())

	rep, err := a.Backup(context.Background(), source.Request{Source: fakeTransportSource(), Sink: sink})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	if !rep.Complete() {
		t.Fatalf("run is incomplete: %+v", rep)
	}

	if rep.Stored != 3 || rep.Entries != 3 {
		t.Fatalf("stored %d of %d entries, want 3 of 3: %+v", rep.Stored, rep.Entries, rep)
	}

	want := []string{"index.json", "runs/2026/db.dump", "runs/2026/notes.txt"}
	if got := sink.paths(); !equalStrings(got, want) {
		t.Fatalf("sink holds %v, want %v", got, want)
	}

	body, _ := sink.content("runs/2026/db.dump")
	if !bytes.Equal(body, patternBytes(3<<20, 1)) {
		t.Fatalf("the stored object is not the source's bytes (%d stored, %d on the source)", len(body), 3<<20)
	}

	if rep.Bytes != int64(3<<20+5+11) {
		t.Errorf("the report counts %d bytes; the source holds %d", rep.Bytes, 3<<20+5+11)
	}

	if rep.Attempts != 3 {
		t.Errorf("three clean objects cost %d attempts, want 3: retrying a run that did not fail is the retry storm this adapter exists to bound", rep.Attempts)
	}
}

// The source is never written to, moved or deleted. It is asserted two
// ways because one of them is stronger: the Deps the adapter is given
// carry no mutating method at all, so there is nothing it COULD call, and
// the fake source's objects are still there afterwards, which is what an
// operator would check.
func TestBackupNeverRemovesAnythingFromTheSource(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	for i := range 20 {
		f.put(fmt.Sprintf("runs/%02d.bin", i), patternBytes(4096, uint32(i+1)), 1_700_000_000)
	}

	before := len(f.objects)

	sink := newRecordingSink()
	a := newAdapter(t, depsFor(f), liveOpts())

	if _, err := a.Backup(context.Background(), source.Request{Source: fakeTransportSource(), Sink: sink}); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	if got := len(f.objects); got != before {
		t.Fatalf("the source held %d objects before the backup and %d after it", before, got)
	}

	for i := range 20 {
		p := fmt.Sprintf("runs/%02d.bin", i)
		o, ok := f.get(p)
		if !ok {
			t.Fatalf("%s is gone from the source", p)
		}
		if !bytes.Equal(o.data, patternBytes(4096, uint32(i+1))) {
			t.Fatalf("%s was modified by the backup", p)
		}
	}

	// The structural half, which is the stronger of the two: the
	// adapter cannot remove anything from a source because nothing it
	// is given has a method that could. Asserted over the METHOD SETS
	// of the interfaces in source.Deps rather than over this fake, so
	// widening one of them - adding a DeleteRemote, a Put, a Move -
	// fails here and not in production.
	for _, iface := range []reflect.Type{
		reflect.TypeFor[source.Streamer](),
		reflect.TypeFor[source.Stater](),
		reflect.TypeFor[source.LinkReader](),
		reflect.TypeFor[transport.Enumerator](),
	} {
		for i := range iface.NumMethod() {
			name := iface.Method(i).Name
			for _, mutating := range []string{"Delete", "Remove", "Put", "Move", "Write", "Rename", "Mkdir", "Copy", "Truncate"} {
				if strings.Contains(name, mutating) {
					t.Errorf("%s.%s can change the source; the adapter is given only read capabilities", iface, name)
				}
			}
		}
	}
}

// Mutation during read, ordered rather than raced: the file is rewritten
// at a known byte of its own read, and what the run reports about it is
// deterministic.
func TestAnObjectRewrittenMidReadIsRetriedAndTheTornCopyIsDiscarded(t *testing.T) {
	t.Parallel()

	original := patternBytes(256<<10, 7)
	replacement := patternBytes(256<<10, 9)

	f := newFakeSource()
	f.put("busy.bin", original, 1_700_000_000)

	var once sync.Once

	f.duringReadAfter = 64 << 10
	f.duringRead = func(s *fakeSource) {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			// Same length, different bytes, a later timestamp: the
			// mutation a size comparison alone would miss.
			s.objects["busy.bin"] = &fakeObject{
				data:    replacement,
				modTime: 1_700_000_050,
				kind:    transport.EntryKindRegular,
			}
		})
	}

	sink := newRecordingSink()
	a := newAdapter(t, depsFor(f), liveOpts())

	rep, err := a.Backup(context.Background(), source.Request{Source: fakeTransportSource(), Sink: sink})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	if !rep.Complete() {
		t.Fatalf("the object settled on the second read, so the run is complete: %+v", rep)
	}

	if rep.Retried != 1 {
		t.Fatalf("report says %d retried, want 1: %+v", rep.Retried, rep)
	}

	if sink.discardCount() != 1 {
		t.Fatalf("the torn copy was discarded %d times, want 1: a stream is read once, so the tear is only visible after the bytes are already stored", sink.discardCount())
	}

	body, ok := sink.content("busy.bin")
	if !ok {
		t.Fatal("nothing was stored for busy.bin")
	}
	if !bytes.Equal(body, replacement) {
		t.Fatal("the stored bytes are not the settled content; a retry that banked the torn read is the failure this test exists for")
	}

	if !reasonsContain(rep.Reasons, "settled after 2 reads") {
		t.Errorf("the report does not say the source moved: %v", rep.Reasons)
	}
}

// A source that never holds still is reported incomplete, within a bound,
// and nothing it produced is left behind as a restore point.
func TestAnObjectThatNeverHoldsStillMakesTheRunIncomplete(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("always-moving.bin", patternBytes(64<<10, 3), 1_700_000_000)

	var mutations int

	f.duringReadAfter = 1 << 10
	f.duringRead = func(s *fakeSource) {
		s.mu.Lock()
		defer s.mu.Unlock()
		mutations++
		s.objects["always-moving.bin"] = &fakeObject{
			data:    patternBytes(64<<10, uint32(100+mutations)),
			modTime: int64(1_700_000_000 + mutations),
			kind:    transport.EntryKindRegular,
		}
	}

	sink := newRecordingSink()
	a := newAdapter(t, depsFor(f), source.Options{
		Mode:        model.ModeLiveBestEffort,
		Preset:      model.PresetConservative,
		MaxAttempts: 3,
	})

	rep, err := a.Backup(context.Background(), source.Request{Source: fakeTransportSource(), Sink: sink})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	if rep.Complete() {
		t.Fatal("a file that was torn on every attempt was reported as a complete run")
	}

	if rep.Incomplete != 1 || rep.Stored != 0 {
		t.Fatalf("want one incomplete and nothing stored, got %+v", rep)
	}

	if rep.Attempts != 3 {
		t.Fatalf("the retry bound is 3 and the run performed %d attempts", rep.Attempts)
	}

	if _, ok := sink.content("always-moving.bin"); ok {
		t.Fatal("a torn read was left in the sink as a restore point")
	}

	if sink.discardCount() != 3 {
		t.Fatalf("three torn attempts produced %d discards", sink.discardCount())
	}

	if !reasonsContain(rep.Reasons, "still moving after 3 reads") {
		t.Errorf("the report does not name the file that would not settle: %v", rep.Reasons)
	}
}

// When the torn copy cannot be removed, the run says so and names the
// artifact. Silence here would leave a restore point nobody knows is
// torn, which is worse than the tear.
func TestATornCopyThatCannotBeDiscardedIsNamedInTheReport(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("busy.bin", patternBytes(32<<10, 5), 1_700_000_000)
	f.duringReadAfter = 1 << 10
	f.duringRead = func(s *fakeSource) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.objects["busy.bin"] = &fakeObject{
			data: patternBytes(32<<10, 6), modTime: 1_700_000_099, kind: transport.EntryKindRegular,
		}
	}

	sink := newRecordingSink()
	sink.discardErr = errors.New("the repository is read-only")

	a := newAdapter(t, depsFor(f), liveOpts())

	rep, err := a.Backup(context.Background(), source.Request{Source: fakeTransportSource(), Sink: sink})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	if rep.Complete() {
		t.Fatal("a run holding an unremovable torn copy called itself complete")
	}

	if !reasonsContain(rep.Reasons, "could not be removed") {
		t.Fatalf("the report does not name the torn artifact somebody has to deal with: %v", rep.Reasons)
	}
}

// A sink whose byte count disagrees with what the stream produced cannot
// be used to prove anything, and is not treated as proof.
func TestASinkThatMisreportsItsByteCountProvesNothing(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("x.bin", patternBytes(8<<10, 11), 1_700_000_000)

	sink := newRecordingSink()
	sink.underreport = true

	a := newAdapter(t, depsFor(f), liveOpts())

	rep, err := a.Backup(context.Background(), source.Request{Source: fakeTransportSource(), Sink: sink})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	if rep.Complete() || rep.Stored != 0 {
		t.Fatalf("a mismatch between the stream's length and the sink's claim was accepted as a verified capture: %+v", rep)
	}
}

// A path that cannot be made safe is refused, reported, and never dialed.
// The last clause is the one that matters: the refusal has to happen
// before anything opens the name.
func TestAnUnsafePathIsRefusedWithoutBeingOpened(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("runs/ok.bin", []byte("fine"), 1_700_000_000)
	f.put("../../etc/shadow", []byte("root:x:0:0"), 1_700_000_000)
	f.put("nul\x00name", []byte("x"), 1_700_000_000)

	sink := newRecordingSink()
	a := newAdapter(t, depsFor(f), liveOpts())

	rep, err := a.Backup(context.Background(), source.Request{Source: fakeTransportSource(), Sink: sink})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	if rep.Refused != 2 {
		t.Fatalf("refused %d entries, want 2: %+v", rep.Refused, rep)
	}

	if rep.Complete() {
		t.Fatal("a run that could not use two of the operator's paths called itself complete")
	}

	if got := sink.paths(); !equalStrings(got, []string{"runs/ok.bin"}) {
		t.Fatalf("sink holds %v; only the safe path may be stored", got)
	}

	if f.opens.Load() != 1 {
		t.Fatalf("the source was opened %d times for one safe object: an unsafe name must be refused before it is dialed", f.opens.Load())
	}
}

// Symlinks under the default policy: reported, counted, never followed,
// nothing stored.
func TestSymlinksAreSkippedAndNeverFollowedByDefault(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("real.txt", []byte("content"), 1_700_000_000)
	f.putKind("link-to-real", transport.EntryKindSymlink, "real.txt")
	f.putKind("link-escaping", transport.EntryKindSymlink, "../../etc/shadow")

	sink := newRecordingSink()
	a := newAdapter(t, depsFor(f), liveOpts())

	rep, err := a.Backup(context.Background(), source.Request{Source: fakeTransportSource(), Sink: sink})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	if rep.SkippedSymlink != 2 {
		t.Fatalf("skipped %d symlinks, want 2: %+v", rep.SkippedSymlink, rep)
	}

	if got := sink.paths(); !equalStrings(got, []string{"real.txt"}) {
		t.Fatalf("sink holds %v, want only the regular file", got)
	}

	if !rep.Complete() {
		t.Fatalf("skipping a symlink by policy is not a hole in the run: %+v", rep)
	}
}

// Preserve stores the link, and refuses one whose target leaves the root.
// The backend has to declare that it stores links at all: none of the
// three this build ships does, which is why the profile is supplied.
func TestSymlinkPreserveStoresTheTargetAndRefusesAnEscape(t *testing.T) {
	t.Parallel()

	caps := localLikeCapabilities()
	caps.SymlinkSemantics = backend.SymlinksStored

	f := newFakeSource()
	f.putKind("links/inside", transport.EntryKindSymlink, "../data/real.txt")
	f.putKind("links/outside", transport.EntryKindSymlink, "../../etc/shadow")
	f.putKind("links/absolute", transport.EntryKindSymlink, "/etc/shadow")
	f.put("data/real.txt", []byte("content"), 1_700_000_000)

	deps := depsFor(f)
	deps.Profiles = capabilityProfile("link_store", caps)

	sink := newRecordingSink()
	a := newAdapter(t, deps, source.Options{
		Mode:     model.ModeLiveBestEffort,
		Preset:   model.PresetConservative,
		Symlinks: source.SymlinkPreserve,
	})

	rep, err := a.Backup(context.Background(), source.Request{Source: fakeTransportSource(), Sink: sink})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	stored, ok := sink.content("links/inside")
	if !ok {
		t.Fatal("the link inside the root was not stored")
	}
	if string(stored) != "../data/real.txt" {
		t.Fatalf("the stored link is %q, want its target; a link IS its target", stored)
	}

	if _, ok := sink.content("links/outside"); ok {
		t.Fatal("a link pointing outside the backup root was stored")
	}
	if _, ok := sink.content("links/absolute"); ok {
		t.Fatal("a link with an absolute target was stored")
	}

	if rep.Refused != 2 {
		t.Fatalf("refused %d escaping links, want 2: %+v", rep.Refused, rep)
	}
}

// Preserve on a backend that does not store links is a configuration
// refusal at the start of the run, not a silent degrade to "ignore".
func TestSymlinkPreserveIsRefusedOnABackendThatSkipsLinks(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("x", []byte("y"), 1)

	a := newAdapter(t, depsFor(f), source.Options{
		Mode:     model.ModeLiveBestEffort,
		Preset:   model.PresetConservative,
		Symlinks: source.SymlinkPreserve,
	})

	_, err := a.Backup(context.Background(), source.Request{Source: fakeTransportSource(), Sink: newRecordingSink()})
	if err == nil {
		t.Fatal("preserve on local_volume, whose matrix says symlink_semantics \"skip\", was accepted")
	}
	if !strings.Contains(err.Error(), "symlink_semantics") {
		t.Fatalf("the refusal does not name the capability it rests on: %v", err)
	}
}

// Sockets, fifos and device nodes: counted, reported, never opened. The
// "never opened" is the load-bearing half, because a read of a fifo with
// no writer does not return.
func TestSpecialFilesAreReportedAndNeverOpened(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("real.txt", []byte("content"), 1_700_000_000)
	f.putKind("run/docker.sock", transport.EntryKindOther, "")
	f.putKind("var/pipe", transport.EntryKindOther, "")

	sink := newRecordingSink()
	a := newAdapter(t, depsFor(f), liveOpts())

	rep, err := a.Backup(context.Background(), source.Request{Source: fakeTransportSource(), Sink: sink})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	if rep.SkippedSpecial != 2 {
		t.Fatalf("skipped %d special files, want 2: %+v", rep.SkippedSpecial, rep)
	}

	if f.opens.Load() != 1 {
		t.Fatalf("the source was opened %d times; only the one regular file may be opened", f.opens.Load())
	}
}

// An entry the source did not classify is not read as "regular". Unknown
// is an absence of an answer, and the answer that costs an open is the
// one this adapter may not assume.
func TestAnUnclassifiedEntryIsNotTreatedAsAFile(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.putKind("mystery", transport.EntryKindUnknown, "")

	sink := newRecordingSink()
	a := newAdapter(t, depsFor(f), liveOpts())

	rep, err := a.Backup(context.Background(), source.Request{Source: fakeTransportSource(), Sink: sink})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	if f.opens.Load() != 0 {
		t.Fatal("an entry the source would not classify was opened anyway")
	}
	if rep.SkippedSpecial != 1 {
		t.Fatalf("an unclassified entry was not accounted for: %+v", rep)
	}
}

// Exclusions fold case on a backend that does not declare itself
// case-sensitive, because an exclusion that matches too little backs up
// the directory of secrets an operator explicitly named.
func TestExclusionsFoldCaseWhereTheBackendWillNotSayItIsCaseSensitive(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("Secrets/key.pem", []byte("-----BEGIN"), 1_700_000_000)
	f.put("secrets/other.pem", []byte("-----BEGIN"), 1_700_000_000)
	f.put("public/readme.md", []byte("hello"), 1_700_000_000)

	src := fakeTransportSource()
	src.ExcludePaths = []string{"secrets"}

	sink := newRecordingSink()
	a := newAdapter(t, depsFor(f), liveOpts())

	rep, err := a.Backup(context.Background(), source.Request{Source: src, Sink: sink})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	if got := sink.paths(); !equalStrings(got, []string{"public/readme.md"}) {
		t.Fatalf("sink holds %v; an exclusion an operator spelled in one case must not be bypassed by the other", got)
	}
	if rep.SkippedExcluded != 2 {
		t.Fatalf("excluded %d entries, want 2: %+v", rep.SkippedExcluded, rep)
	}
}

// On a backend that DOES declare case sensitivity, two names differing
// only in case are two objects, and only the one that was excluded is.
func TestExclusionsAreExactWhereTheBackendDeclaresCaseSensitivity(t *testing.T) {
	t.Parallel()

	caps := localLikeCapabilities()
	caps.CaseSensitivity = backend.CaseSensitive

	f := newFakeSource()
	f.put("Secrets/key.pem", []byte("a"), 1_700_000_000)
	f.put("secrets/other.pem", []byte("b"), 1_700_000_000)

	src := fakeTransportSource()
	src.ExcludePaths = []string{"secrets"}

	deps := depsFor(f)
	deps.Profiles = capabilityProfile("case_sensitive_backend", caps)

	sink := newRecordingSink()
	a := newAdapter(t, deps, liveOpts())

	if _, err := a.Backup(context.Background(), source.Request{Source: src, Sink: sink}); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	if got := sink.paths(); !equalStrings(got, []string{"Secrets/key.pem"}) {
		t.Fatalf("sink holds %v, want only the differently-cased path the exclusion does not name", got)
	}
}

// A backend that cannot stream is refused, and the refusal says so rather
// than falling back to a staging copy.
func TestABackendThatCannotStreamIsRefused(t *testing.T) {
	t.Parallel()

	caps := localLikeCapabilities()
	caps.StreamingOpen = false

	f := newFakeSource()
	deps := depsFor(f)
	deps.Profiles = capabilityProfile("no_stream", caps)

	a := newAdapter(t, deps, liveOpts())

	_, err := a.Backup(context.Background(), source.Request{Source: fakeTransportSource(), Sink: newRecordingSink()})
	if !errors.Is(err, source.ErrUnstreamable) {
		t.Fatalf("Backup on an unstreamable backend returned %v, want ErrUnstreamable", err)
	}
}

// A backend that cannot be listed in bounded memory is refused BEFORE
// anything is dialed, and can still be backed up from a path list. This
// is the SFTP shape stated as a property.
func TestAnUnboundedBackendIsRefusedForAWalkAndAcceptedForAPathList(t *testing.T) {
	t.Parallel()

	caps := localLikeCapabilities()
	caps.BoundedListing = false

	f := newFakeSource()
	f.put("runs/db.dump", patternBytes(4096, 21), 1_700_000_000)

	deps := depsFor(f)
	deps.Profiles = capabilityProfile("unbounded_listing", caps)

	sink := newRecordingSink()
	a := newAdapter(t, deps, liveOpts())

	_, err := a.Backup(context.Background(), source.Request{Source: fakeTransportSource(), Sink: sink})
	if !errors.Is(err, backend.ErrUnboundedListing) {
		t.Fatalf("Backup on an unlistable backend returned %v, want ErrUnboundedListing", err)
	}

	if f.opens.Load() != 0 || f.stats.Load() != 0 {
		t.Fatal("the refusal arrived after the source had been touched; it has to arrive before")
	}

	rep, err := a.BackupPaths(context.Background(), source.Request{Source: fakeTransportSource(), Sink: sink}, []string{"runs/db.dump"})
	if err != nil {
		t.Fatalf("BackupPaths: %v", err)
	}
	if rep.Stored != 1 || !rep.Complete() {
		t.Fatalf("a path list on an unlistable backend did not work: %+v", rep)
	}
}

// A backend that reports nothing a read window could be checked against
// is refused, unless the operator declared a mode under which the source
// provably cannot move.
func TestABackendWithNoReadWindowEvidenceIsRefusedUnlessTheModeFreezesTheSource(t *testing.T) {
	t.Parallel()

	caps := localLikeCapabilities()
	caps.StableSize = false
	caps.MTimePrecision = backend.MTimeUnknown
	caps.GenerationIdentity = backend.GenerationUnknown

	f := newFakeSource()
	f.put("x.bin", []byte("data"), 0)

	deps := depsFor(f)
	deps.Profiles = capabilityProfile("blind_backend", caps)

	a := newAdapter(t, deps, liveOpts())

	_, err := a.Backup(context.Background(), source.Request{Source: fakeTransportSource(), Sink: newRecordingSink()})
	if !errors.Is(err, source.ErrUndetectableMutation) {
		t.Fatalf("a backend with no post-read evidence was accepted under a live mode: %v", err)
	}

	frozen := newAdapter(t, deps, source.Options{Mode: model.ModeExternalSnapshot, Preset: model.PresetConservative})

	rep, err := frozen.Backup(context.Background(), source.Request{Source: fakeTransportSource(), Sink: newRecordingSink()})
	if err != nil {
		t.Fatalf("the same backend under a point-in-time mode: %v", err)
	}
	if rep.Stored != 1 {
		t.Fatalf("nothing was stored under a mode that guarantees a point in time: %+v", rep)
	}

	if f.stats.Load() != 0 {
		t.Fatalf("a point-in-time mode paid for %d post-read stats; a frozen image cannot move, so there is nothing to find", f.stats.Load())
	}
}

// A backend that keeps no modification time this engine can read reports
// none, rather than reporting the epoch. Fabricating 1970 is worse than
// saying nothing, because a consumer cannot tell the difference.
func TestNoModificationTimeIsRecordedWhereTheBackendDeclaresNone(t *testing.T) {
	t.Parallel()

	caps := localLikeCapabilities()
	caps.MTimePrecision = backend.MTimeUnknown

	f := newFakeSource()
	f.put("x.bin", []byte("data"), 1_700_000_000)

	deps := depsFor(f)
	deps.Profiles = capabilityProfile("timeless", caps)

	var seen time.Time

	a := newAdapter(t, deps, source.Options{
		Mode:   model.ModeLiveBestEffort,
		Preset: model.PresetConservative,
	})

	sink := &modTimeSink{inner: newRecordingSink(), observe: func(t time.Time) { seen = t }}

	if _, err := a.Backup(context.Background(), source.Request{Source: fakeTransportSource(), Sink: sink}); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	if !seen.IsZero() {
		t.Fatalf("the object was given the modification time %s from a backend whose matrix says it keeps none", seen)
	}
}

// The generation identity gate: an object identifier is used as CONTENT
// identity only where the matrix says the backend versions content. A
// path-shaped identifier changing is not evidence of anything, and a
// versioned one changing is decisive.
func TestOnlyAVersionedBackendsIdentifierIsReadAsContentIdentity(t *testing.T) {
	t.Parallel()

	build := func(gen backend.GenerationIdentity) (*fakeSource, source.Report) {
		t.Helper()

		caps := localLikeCapabilities()
		caps.GenerationIdentity = gen

		f := newFakeSource()
		f.mu.Lock()
		f.objects["obj"] = &fakeObject{
			data: patternBytes(16<<10, 4), modTime: 1_700_000_000, id: "v1", kind: transport.EntryKindRegular,
		}
		f.mu.Unlock()

		// The identifier changes and NOTHING else does: same bytes, same
		// length, same timestamp. Only a backend whose matrix says the
		// identifier names content may read that as a change.
		f.duringReadAfter = 1 << 10
		f.duringRead = func(s *fakeSource) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.objects["obj"] = &fakeObject{
				data: patternBytes(16<<10, 4), modTime: 1_700_000_000, id: "v2", kind: transport.EntryKindRegular,
			}
		}

		deps := depsFor(f)
		deps.Profiles = capabilityProfile("gen_test", caps)

		rep, err := newAdapter(t, deps, liveOpts()).Backup(
			context.Background(), source.Request{Source: fakeTransportSource(), Sink: newRecordingSink()})
		if err != nil {
			t.Fatalf("Backup: %v", err)
		}

		return f, rep
	}

	if _, rep := build(backend.GenerationNone); rep.Retried != 0 || rep.Stored != 1 {
		t.Errorf("a backend declaring generation_identity \"none\" retried on a changed slot id: %+v", rep)
	}

	if _, rep := build(backend.GenerationVersioned); rep.Retried != 1 {
		t.Errorf("a backend declaring generation_identity \"versioned\" did not notice its identifier change: %+v", rep)
	}
}

// Concurrency is bounded by the option and the enumeration blocks on it,
// so a fast listing cannot build a queue of the whole source.
func TestConcurrencyIsBounded(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	for i := range 200 {
		f.put(fmt.Sprintf("f%03d.bin", i), patternBytes(64<<10, uint32(i+1)), 1_700_000_000)
	}

	sink := newRecordingSink()
	a := newAdapter(t, depsFor(f), source.Options{
		Mode:        model.ModeLiveBestEffort,
		Preset:      model.PresetConservative,
		Concurrency: 3,
	})

	rep, err := a.Backup(context.Background(), source.Request{Source: fakeTransportSource(), Sink: sink})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}

	if rep.Stored != 200 {
		t.Fatalf("stored %d of 200: %+v", rep.Stored, rep)
	}

	if peak := sink.peak.Load(); peak > 3 {
		t.Fatalf("%d objects were in flight at once with a concurrency of 3", peak)
	}
}

// Cancellation: the run stops, every reader is closed, and no goroutine
// outlives the call. The goroutine count is compared before and after
// because a worker pool that is not waited for leaks one per run, which
// is invisible until a daemon has run a few thousand backups.
func TestCancellationStopsTheRunAndLeavesNothingRunning(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	for i := range 500 {
		f.put(fmt.Sprintf("f%03d.bin", i), patternBytes(128<<10, uint32(i+1)), 1_700_000_000)
	}

	ctx, cancel := context.WithCancel(context.Background())

	var stored atomic.Int64

	sink := &cancellingSink{
		inner: newRecordingSink(),
		after: func(n int64) {
			if n == 5 {
				cancel()
			}
			stored.Store(n)
		},
	}

	before := runtime.NumGoroutine()

	a := newAdapter(t, depsFor(f), source.Options{
		Mode:        model.ModeLiveBestEffort,
		Preset:      model.PresetConservative,
		Concurrency: 4,
	})

	_, err := a.Backup(ctx, source.Request{Source: fakeTransportSource(), Sink: sink})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled backup returned %v, want context.Canceled", err)
	}

	if stored.Load() >= 500 {
		t.Fatal("the run read the whole source after being cancelled")
	}

	// The workers are joined before Backup returns, so the count is back
	// where it started as soon as the call does. A short settle covers
	// the runtime's own bookkeeping, not the adapter's.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("%d goroutines before the cancelled run and %d after it", before, after)
	}

	if live := f.inturn.Load(); live != 0 {
		t.Fatalf("%d source readers are still open after a cancelled run", live)
	}
}

// Backup does not return while a worker is still reading, cancelled or
// not.
//
// This is the leak check stated so that it cannot pass by accident. A
// goroutine count taken after a settling loop is a weak signal: workers
// draining a closed channel exit within microseconds, so a run that
// forgot to join them still looks clean a moment later. Here the sink
// BLOCKS, so a worker is provably still inside Store when the run is
// cancelled, and a Backup that returned without joining would return with
// that worker still holding a source reader - which is exactly the
// per-run leak that is invisible until a daemon has performed a few
// thousand backups.
func TestBackupDoesNotReturnWhileAWorkerIsStillReading(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	for i := range 50 {
		f.put(fmt.Sprintf("f%02d.bin", i), patternBytes(4096, uint32(i+1)), 1_700_000_000)
	}

	var (
		inFlight atomic.Int64
		entered  = make(chan struct{}, 50)
		release  = make(chan struct{})
	)

	sink := &blockingSink{
		inner: newRecordingSink(),
		enter: func() {
			inFlight.Add(1)
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
		},
		leave: func() { inFlight.Add(-1) },
	}

	ctx, cancel := context.WithCancel(context.Background())

	a := newAdapter(t, depsFor(f), source.Options{
		Mode:        model.ModeLiveBestEffort,
		Preset:      model.PresetConservative,
		Concurrency: 4,
	})

	done := make(chan struct{})

	go func() {
		defer close(done)

		_, _ = a.Backup(ctx, source.Request{Source: fakeTransportSource(), Sink: sink})
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no worker ever reached the sink")
	}

	cancel()
	close(release)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Backup never returned")
	}

	if n := inFlight.Load(); n != 0 {
		t.Fatalf("Backup returned with %d workers still inside the sink", n)
	}

	if live := f.inturn.Load(); live != 0 {
		t.Fatalf("Backup returned with %d source readers still open", live)
	}
}

// A read blocked on a source that never returns bytes is unblocked by
// cancellation, because the adapter closes the reader from the outside. A
// context is not something a blocked read consults.
func TestACancelledRunUnblocksAReadThatIsStuck(t *testing.T) {
	t.Parallel()

	blocked := make(chan struct{})
	released := make(chan struct{})

	f := newFakeSource()
	f.put("stuck.bin", []byte("x"), 1_700_000_000)

	deps := depsFor(f)
	deps.Streamer = streamerFunc(func(context.Context, transport.Source, string) (io.ReadCloser, error) {
		return &blockingReader{blocked: blocked, released: released}, nil
	})

	ctx, cancel := context.WithCancel(context.Background())

	a := newAdapter(t, deps, source.Options{
		Mode:        model.ModeLiveBestEffort,
		Preset:      model.PresetConservative,
		Concurrency: 1,
	})

	done := make(chan error, 1)

	go func() {
		_, err := a.Backup(ctx, source.Request{Source: fakeTransportSource(), Sink: newRecordingSink()})
		done <- err
	}()

	select {
	case <-blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("the read never started")
	}

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run did not return after cancellation: a read blocked on a source is only reachable by closing it from the outside")
	}

	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("the blocked reader was never closed")
	}
}

// Bounded memory over an object far larger than any budget: the peak heap
// is a function of the buffer, not of the object.
func TestALargeObjectStreamsWithoutBeingHeld(t *testing.T) {
	// Deliberately NOT parallel. runtime.MemStats is process-wide, so a
	// sibling test allocating at the same moment is indistinguishable
	// here from the code under test allocating. Go runs the
	// non-parallel tests to completion before resuming the paused
	// parallel ones, which is the isolation this measurement needs.
	if testing.Short() {
		t.Skip("multi-gigabyte streaming; -short skips it")
	}

	const size = 2 << 30 // 2 GiB

	deps := source.Deps{
		Streamer: streamerFunc(func(context.Context, transport.Source, string) (io.ReadCloser, error) {
			return &generatedReader{remaining: size}, nil
		}),
		Stater: staterFunc(func(context.Context, transport.Source, string) (transport.RemoteArtifact, error) {
			return transport.RemoteArtifact{
				Path: "huge.bin", Size: size, ModTime: 1_700_000_000, Kind: transport.EntryKindRegular,
			}, nil
		}),
	}

	sink := &countingSink{}
	a := newAdapter(t, deps, source.Options{
		Mode:        model.ModeLiveBestEffort,
		Preset:      model.PresetConservative,
		Concurrency: 1,
	})

	runtime.GC()

	var start runtime.MemStats

	runtime.ReadMemStats(&start)

	rep, err := a.BackupPaths(context.Background(), source.Request{Source: fakeTransportSource(), Sink: sink}, []string{"huge.bin"})
	if err != nil {
		t.Fatalf("BackupPaths: %v", err)
	}

	runtime.GC()

	var end runtime.MemStats

	runtime.ReadMemStats(&end)

	if rep.Bytes != size {
		t.Fatalf("streamed %d bytes of a %d-byte object", rep.Bytes, int64(size))
	}

	// Two measurements, because they catch two different mistakes.
	//
	// TotalAlloc is everything allocated during the pass, so a staging
	// copy, an io.ReadAll or an append-per-chunk shows up here even if
	// the collector reclaimed it before the test looked. Streaming 2 GiB
	// through a fixed buffer allocates a few megabytes; a budget of 256
	// MiB is an order of magnitude under the object and still far above
	// anything an honest implementation does, including the race
	// detector's overhead.
	//
	// HeapAlloc after a collection is what is still HELD, which is what
	// catches a reader that keeps every chunk it produced.
	const (
		allocBudget = 256 << 20
		heldBudget  = 64 << 20
	)

	if total := int64(end.TotalAlloc - start.TotalAlloc); total > allocBudget {
		t.Fatalf("streaming %d bytes allocated %d in total; the object is being copied rather than streamed", int64(size), total)
	}

	if live := int64(end.HeapAlloc); live > heldBudget {
		t.Fatalf("the heap still holds %d bytes after streaming a %d-byte object", live, int64(size))
	}
}

// A vanished file is ordinary and is not an unreadable one: a backup
// source is a live directory somebody else is writing to.
func TestAFileDeletedBetweenTheListingAndTheReadIsReportedAsVanished(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("here.txt", []byte("content"), 1_700_000_000)

	sink := newRecordingSink()
	a := newAdapter(t, depsFor(f), liveOpts())

	rep, err := a.BackupPaths(context.Background(),
		source.Request{Source: fakeTransportSource(), Sink: sink},
		[]string{"here.txt", "gone.txt"})
	if err != nil {
		t.Fatalf("BackupPaths: %v", err)
	}

	if rep.Vanished != 1 {
		t.Fatalf("report says %d vanished, want 1: %+v", rep.Vanished, rep)
	}
	if rep.Unreadable != 0 {
		t.Fatalf("a deleted file was reported as unreadable: %+v", rep)
	}
}

// The report samples reasons rather than accumulating one per file: a
// source with a million unreadable files must cost a report the size of a
// report.
func TestTheReportSamplesReasonsRatherThanHoldingOnePerFile(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	paths := make([]string, 0, 500)

	for i := range 500 {
		paths = append(paths, fmt.Sprintf("missing-%03d.bin", i))
	}

	sink := newRecordingSink()
	a := newAdapter(t, depsFor(f), liveOpts())

	rep, err := a.BackupPaths(context.Background(), source.Request{Source: fakeTransportSource(), Sink: sink}, paths)
	if err != nil {
		t.Fatalf("BackupPaths: %v", err)
	}

	if len(rep.Reasons) > 64 {
		t.Fatalf("the report carries %d sentences; it is capped at 64", len(rep.Reasons))
	}
	if rep.DroppedReasons != int64(500-len(rep.Reasons)) {
		t.Fatalf("the report dropped %d reasons out of %d, which does not add up", rep.DroppedReasons, 500)
	}
	if rep.Vanished != 500 {
		t.Fatalf("the counters were sampled too: %+v", rep)
	}
}

// Construction refuses a wiring mistake instead of discovering it on the
// thousandth file.
func TestNewRefusesAnUnusableConfiguration(t *testing.T) {
	t.Parallel()

	if _, err := source.New(source.Deps{}, source.Options{}); !errors.Is(err, source.ErrNoStreamer) {
		t.Errorf("an adapter with no streamer was built: %v", err)
	}

	f := newFakeSource()

	if _, err := source.New(source.Deps{Streamer: f}, source.Options{Concurrency: -1}); err == nil {
		t.Error("a negative concurrency was accepted")
	}

	if _, err := source.New(source.Deps{Streamer: f}, source.Options{MaxAttempts: -1}); err == nil {
		t.Error("a negative attempt bound was accepted")
	}

	if _, err := source.New(source.Deps{Streamer: f}, source.Options{Symlinks: "follow"}); err == nil {
		t.Error("\"follow\" was accepted as a symlink policy; following is not a policy this product has")
	}

	if _, err := source.New(source.Deps{Streamer: f}, source.Options{Symlinks: source.SymlinkPreserve}); !errors.Is(err, source.ErrNoLinks) {
		t.Errorf("preserve was accepted with no way to read a link target: %v", err)
	}

	a := newAdapter(t, source.Deps{Streamer: f, Enumerator: f}, liveOpts())
	if _, err := a.Backup(context.Background(), source.Request{Source: fakeTransportSource()}); !errors.Is(err, source.ErrNoSink) {
		t.Errorf("a backup with nowhere to put the bytes was accepted: %v", err)
	}

	if _, err := a.Backup(context.Background(), source.Request{Source: fakeTransportSource(), Sink: newRecordingSink()}); !errors.Is(err, source.ErrNoStater) {
		t.Errorf("a live-mode backup with no way to re-stat was accepted: %v", err)
	}
}

// OnResult reports every entry, including the ones nothing was stored
// for, because a caller building a catalog needs the skips as much as the
// stores.
func TestOnResultSeesEveryEntry(t *testing.T) {
	t.Parallel()

	f := newFakeSource()
	f.put("a.txt", []byte("a"), 1_700_000_000)
	f.putKind("b.link", transport.EntryKindSymlink, "a.txt")
	f.putKind("c.sock", transport.EntryKindOther, "")

	var mu sync.Mutex

	seen := map[string]sourceconsistency.Outcome{}

	a := newAdapter(t, depsFor(f), source.Options{
		Mode:   model.ModeLiveBestEffort,
		Preset: model.PresetConservative,
		OnResult: func(r source.Result) {
			mu.Lock()
			defer mu.Unlock()
			seen[r.Path] = r.Outcome
		},
	})

	if _, err := a.Backup(context.Background(), source.Request{Source: fakeTransportSource(), Sink: newRecordingSink()}); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	want := map[string]sourceconsistency.Outcome{
		"a.txt":  sourceconsistency.OutcomeStable,
		"b.link": sourceconsistency.OutcomeNotAFile,
		"c.sock": sourceconsistency.OutcomeNotAFile,
	}

	mu.Lock()
	defer mu.Unlock()

	for p, w := range want {
		if seen[p] != w {
			t.Errorf("OnResult reported %q for %s, want %q", seen[p], p, w)
		}
	}
}

// --- helpers -------------------------------------------------------------

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

type streamerFunc func(context.Context, transport.Source, string) (io.ReadCloser, error)

func (s streamerFunc) OpenSourceStream(ctx context.Context, src transport.Source, p string) (io.ReadCloser, error) {
	return s(ctx, src, p)
}

type staterFunc func(context.Context, transport.Source, string) (transport.RemoteArtifact, error)

func (s staterFunc) StatSource(ctx context.Context, src transport.Source, p string) (transport.RemoteArtifact, error) {
	return s(ctx, src, p)
}

// blockingReader never produces a byte until it is closed, which is how a
// remote read that has stopped answering behaves.
type blockingReader struct {
	once     sync.Once
	blocked  chan struct{}
	released chan struct{}
	closed   chan struct{}
	initOnce sync.Once
}

func (b *blockingReader) init() {
	b.initOnce.Do(func() { b.closed = make(chan struct{}) })
}

func (b *blockingReader) Read([]byte) (int, error) {
	b.init()
	b.once.Do(func() { close(b.blocked) })
	<-b.closed

	return 0, errors.New("read torn down")
}

func (b *blockingReader) Close() error {
	b.init()
	select {
	case <-b.closed:
	default:
		close(b.closed)
		close(b.released)
	}

	return nil
}

// generatedReader produces bytes without holding them, so a
// bounded-memory test measures the code under test and not its fixture.
type generatedReader struct {
	remaining int64
	block     []byte
}

func (g *generatedReader) Read(p []byte) (int, error) {
	if g.remaining <= 0 {
		return 0, io.EOF
	}

	if g.block == nil {
		// One small block, copied. The generator must not be what a
		// bounded-memory test measures, and it must not be what it
		// spends its time in either.
		g.block = patternBytes(64<<10, 0x9E3779B9)
	}

	n := len(p)
	if int64(n) > g.remaining {
		n = int(g.remaining)
	}

	for filled := 0; filled < n; {
		filled += copy(p[filled:n], g.block)
	}

	g.remaining -= int64(n)

	return n, nil
}

func (g *generatedReader) Close() error { return nil }

// modTimeSink observes the modification time an object was prepared with.
type modTimeSink struct {
	inner   *recordingSink
	observe func(time.Time)
}

func (m *modTimeSink) Store(ctx context.Context, obj source.Object) (source.Stored, error) {
	m.observe(obj.ModTime)

	return m.inner.Store(ctx, obj)
}

func (m *modTimeSink) Discard(ctx context.Context, id string) error { return m.inner.Discard(ctx, id) }

// cancellingSink counts what it has stored so a test can cancel part way.
type cancellingSink struct {
	inner *recordingSink
	after func(int64)
	n     atomic.Int64
}

func (c *cancellingSink) Store(ctx context.Context, obj source.Object) (source.Stored, error) {
	out, err := c.inner.Store(ctx, obj)
	c.after(c.n.Add(1))

	return out, err
}

func (c *cancellingSink) Discard(ctx context.Context, id string) error {
	return c.inner.Discard(ctx, id)
}

// blockingSink holds a worker inside Store until it is released, which is
// how a test stands still in the middle of a run.
type blockingSink struct {
	inner *recordingSink
	enter func()
	leave func()
}

func (b *blockingSink) Store(ctx context.Context, obj source.Object) (source.Stored, error) {
	b.enter()
	defer b.leave()

	return b.inner.Store(ctx, obj)
}

func (b *blockingSink) Discard(ctx context.Context, id string) error { return b.inner.Discard(ctx, id) }
