// What the feed COSTS the work it is following (EPIC G review).
//
// liveactivity.go's RecordEvent promises, in its own doc, that following
// the work is never a reason the work is slower: "exactly one lock, one
// append and a return". The buffer behind it was not a ring. Once full,
// the append grew a 200-element slice and copied it, and then the body
// allocated a second 200-element array and copied it again, so every
// event a cycle emitted paid two allocations and 73 KB, inside the
// mutex, on whichever goroutine the cycle had reached the event on. The
// read path had the same shape one level up: it filtered a bucket into a
// fresh slice sized for the WHOLE bucket and then copied that into
// another, for every bucket in the reading, under the same lock, at the
// one-second cadence two surfaces poll during a cycle.
//
// So the cases here are measurements rather than behaviour. The
// behaviour is already pinned by liveactivity_test.go and
// liveactivitysplit_test.go, and none of it may move: what these hold is
// that the cost of it stays flat.
package service

import (
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/spdrman/backupd/core/internal/obs"
)

// bytesPerRun is the average heap allocated by one call to f.
//
// TotalAlloc counts every byte ever allocated and never goes down, so it
// measures what a call ASKED FOR rather than what happened to survive a
// GC, which is the number that matters for a path that runs inside a
// mutex on a working goroutine. testing.AllocsPerRun answers the other
// half (how many allocations) and neither answers both.
func bytesPerRun(iterations int, f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < iterations; i++ {
		f()
	}
	runtime.ReadMemStats(&after)
	return (after.TotalAlloc - before.TotalAlloc) / uint64(iterations)
}

// sampleEvent is one already-built line, so what a measurement below sees
// is the buffer's own cost and not the cost of composing an event.
func sampleEvent(seq int64) LiveActivityEvent {
	return LiveActivityEvent{
		Sequence: seq,
		At:       time.Unix(0, seq),
		Level:    "info",
		Event:    obs.EventLifecycleTransition,
		Scope:    LiveActivityScopeSet,
		Message:  "lifecycle transition",
	}
}

// fullRing is a ring at capacity, which is the state a daemon that has
// been up for more than a few seconds is permanently in and therefore the
// only state worth measuring.
func fullRing() *liveActivityRing {
	r := newLiveActivityRing(liveActivityBufferSize)
	for i := 1; i <= liveActivityBufferSize; i++ {
		r.add(sampleEvent(int64(i)))
	}
	return r
}

// TestLiveActivityRing_AddIntoAFullRingAllocatesNothing is the claim the
// name "ring" makes. A fixed buffer that displaces its oldest entry does
// one index write; anything else is a buffer that reallocates itself
// under a different name.
func TestLiveActivityRing_AddIntoAFullRingAllocatesNothing(t *testing.T) {
	r := fullRing()
	seq := int64(liveActivityBufferSize)

	allocs := testing.AllocsPerRun(200, func() {
		seq++
		r.add(sampleEvent(seq))
	})

	if allocs != 0 {
		t.Errorf("adding one event to a full %d-entry ring allocates %.1f times. RecordEvent runs on the cycle's own goroutine inside the feed's mutex, so every allocation here is time the work being followed is not doing the work",
			liveActivityBufferSize, allocs)
	}
}

// The same claim in bytes, over enough adds that a buffer which copies
// itself cannot hide inside the noise. Ten thousand events into one full
// 200-entry ring copied 703 MB before this.
func TestLiveActivityRing_TenThousandAddsCopyNothing(t *testing.T) {
	r := fullRing()
	seq := int64(liveActivityBufferSize)

	perAdd := bytesPerRun(10000, func() {
		seq++
		r.add(sampleEvent(seq))
	})

	if perAdd > 16 {
		t.Errorf("one add into a full ring allocates %d bytes, so 10000 of them move %d MB through the heap. A ring writes one slot",
			perAdd, perAdd*10000/(1<<20))
	}
}

// A ring is still a bounded, oldest-first tail: the measurements above
// are worth nothing if what comes back changed. This holds the three
// facts the feed's honesty rests on across a wrap: what is held, in what
// order, and which sequence was the last one thrown away.
func TestLiveActivityRing_HoldsTheNewestCapEventsOldestFirst(t *testing.T) {
	r := newLiveActivityRing(4)
	for i := 1; i <= 10; i++ {
		r.add(sampleEvent(int64(i)))
	}

	if got := r.len(); got != 4 {
		t.Fatalf("a 4-entry ring holds %d events after 10 adds", got)
	}
	for i := 0; i < 4; i++ {
		want := int64(7 + i)
		if got := r.at(i).Sequence; got != want {
			t.Errorf("the ring's %d'th oldest event is sequence %d, want %d", i, got, want)
		}
	}
	if r.evicted != 6 {
		t.Errorf("the ring says it last discarded sequence %d; six events fell out, the newest of them was 6, and a client's cursor is compared against exactly this number",
			r.evicted)
	}
}

// A ring that has not filled yet reports only what it holds, and reports
// having discarded nothing.
func TestLiveActivityRing_APartlyFilledRingDiscardsNothing(t *testing.T) {
	r := newLiveActivityRing(4)
	r.add(sampleEvent(1))
	r.add(sampleEvent(2))

	if got := r.len(); got != 2 {
		t.Fatalf("the ring holds %d of the 2 events added", got)
	}
	if r.at(0).Sequence != 1 || r.at(1).Sequence != 2 {
		t.Errorf("the ring reports %d then %d", r.at(0).Sequence, r.at(1).Sequence)
	}
	if r.evicted != 0 {
		t.Errorf("a ring that has never overflowed says it discarded sequence %d", r.evicted)
	}
}

// TestLiveActivity_SequencesWithinABucketIncreaseStrictly is what makes
// the read path's binary search legal.
//
// tailOf finds the first event past a cursor with sort.Search, which is
// correct only while sequences inside one bucket are sorted. They are,
// because l.seq++ and the add into the bucket happen under the same
// mutex, so no two events can be numbered out of the order they were
// filed in. That is a property of the locking rather than of anything
// declared, which is exactly the kind of thing a later change breaks
// silently, so it is asserted here against real concurrent writers
// rather than trusted.
func TestLiveActivity_SequencesWithinABucketIncreaseStrictly(t *testing.T) {
	rec := newLiveActivity(configuredSets("alpha/nightly"))

	var writers sync.WaitGroup
	for w := 0; w < 8; w++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := 0; i < 200; i++ {
				recordSetEvent(rec, "alpha/nightly", obs.EventLifecycleTransition)
			}
		}()
	}
	writers.Wait()

	got := rec.snapshot("alpha/nightly", 0, liveActivityMaxLimit)
	if len(got.Events) < 2 {
		t.Fatalf("the bucket holds %d events after 1600 concurrent adds", len(got.Events))
	}
	for i := 1; i < len(got.Events); i++ {
		if got.Events[i].Sequence <= got.Events[i-1].Sequence {
			t.Fatalf("the bucket holds sequence %d at position %d and %d at %d. A bucket's sequences have to increase strictly, because tailOf binary-searches for a cursor inside one and an unsorted bucket makes that search return a wrong answer rather than a slow one",
				got.Events[i-1].Sequence, i-1, got.Events[i].Sequence, i)
		}
	}
}

// TestLiveActivity_ReadDoesNotPayForWhatItDoesNotReturn is the read path.
//
// A reading of twenty full buckets used to allocate 987 KB: every bucket
// was filtered into a slice sized for the whole bucket and then copied
// into a second one, so a limit of 50 still paid for 200 events per
// bucket, under the feed's mutex, at the one-second cadence a dashboard
// and the global terminal each poll at during a cycle. What a read costs
// should be a function of what it hands back.
func TestLiveActivity_ReadDoesNotPayForWhatItDoesNotReturn(t *testing.T) {
	const sets = 20
	ids := make([]string, 0, sets)
	for i := 0; i < sets; i++ {
		ids = append(ids, "alpha/set"+string(rune('a'+i)))
	}
	rec := newLiveActivity(configuredSets(ids...))
	for i := 0; i < liveActivityBufferSize; i++ {
		for _, id := range ids {
			recordSetEvent(rec, id, obs.EventLifecycleTransition)
		}
		recordDeploymentEvent(rec, obs.EventCycleStart)
	}

	perRead := bytesPerRun(200, func() {
		rec.read(ids, true, 0, liveActivityDefaultLimit)
	})

	// Twenty-one buckets handing back fifty events each is about 130 KB
	// of LiveActivityEvent, and this is comfortably above that and far
	// below the 987 KB a read cost when it sized itself by what it held
	// rather than by what it returns.
	if perRead > 250<<10 {
		t.Errorf("one read of %d full buckets at a limit of %d allocates %d KB, and it holds the feed's mutex for all of it",
			sets, liveActivityDefaultLimit, perRead>>10)
	}
}

// BenchmarkRecordEventFullRing is the hot path itself: obs.Sink, called
// on the cycle's own goroutine for every event the engine emits, and
// since the API action log (issue #599) on every non-GET request too.
func BenchmarkRecordEventFullRing(b *testing.B) {
	rec := newLiveActivity(configuredSets("alpha/nightly"))
	for i := 0; i < liveActivityBufferSize; i++ {
		recordSetEvent(rec, "alpha/nightly", obs.EventLifecycleTransition)
	}
	record := obs.Record{
		At: time.Now(), Level: obs.LevelInfo,
		Event: obs.EventLifecycleTransition, Message: "lifecycle transition",
		Fields: []obs.Field{{Key: "backup_set", Value: "alpha/nightly"}},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec.RecordEvent(record)
	}
}

// BenchmarkLiveActivityRead is the poll: twenty full buckets plus the
// deployment's, read as one moment under one lock, which is what both
// surfaces ask for once a second while a cycle is running.
func BenchmarkLiveActivityRead(b *testing.B) {
	const sets = 20
	ids := make([]string, 0, sets)
	for i := 0; i < sets; i++ {
		ids = append(ids, "alpha/set"+string(rune('a'+i)))
	}
	rec := newLiveActivity(configuredSets(ids...))
	for i := 0; i < liveActivityBufferSize; i++ {
		for _, id := range ids {
			recordSetEvent(rec, id, obs.EventLifecycleTransition)
		}
		recordDeploymentEvent(rec, obs.EventCycleStart)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec.read(ids, true, 0, liveActivityDefaultLimit)
	}
}
