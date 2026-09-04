package conformance_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/spdrman/rclone-manager/core/internal/artifactstore"
	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/placement"
	"github.com/spdrman/rclone-manager/core/internal/state"
	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// This file is the continuous invariant watcher the phase 2 exit gate
// asks for, and the argument for why it is continuous rather than a
// sampler.
//
// # The claim
//
// FR-30's standing invariant is a predicate over two things: the durable
// journal, and which copies actually exist. Neither changes on its own.
// The journal changes when something writes it. A copy stops existing
// when something deletes it. So the complete set of events that can
// falsify the invariant is:
//
//	1. a journal write
//	2. a call that removes a copy
//
// and a check attached to every one of those is a check over the whole
// run, with no gap between observations for a breach to hide in. That is
// the same argument tests/movecrash makes, and it is why "continuously"
// in the gate line is satisfiable at all: a poller with any interval, no
// matter how tight, has a window it cannot see, and this has none.
//
// # Where this goes further than the crash suite's guard
//
// tests/movecrash guards the two delete calls and re-reads the journal at
// each. That catches a delete taken while the journal says the invariant
// does not hold, which is the ordering bug FR-30 is written against.
//
// It does not catch the other direction: a copy destroyed while the
// journal still describes it as ACTIVE and verified. At that instant the
// journal reads clean and the artifact has no readable copy at all, and
// every journal-only check in this repository would call that fine.
//
// So this watcher keeps a set of locators it has watched being destroyed,
// and subtracts them from the record before evaluating. A placement row
// pointing at bytes this run deleted is not a copy, and the watcher will
// not count one. The set is cleared for a locator when something writes
// to it again, which is what a move back to local does.
//
// # What it does NOT claim
//
// It does not watch a copy that something outside this process deletes,
// and it does not verify the bytes at every instant (that would be a
// download per event). What it asserts at each event is placement
// arithmetic against reality as this process has changed it; the bytes are
// hashed at the end of every scenario, separately, by the assertion that
// reads each surviving ACTIVE placement back.

// watcher evaluates FR-30's standing invariant at every event that can
// falsify it, for every artifact in the scenario.
type watcher struct {
	t          *testing.T
	journal    *state.Journal
	ids        []model.ArtifactID
	sufficient []placement.Class

	mu sync.Mutex
	// observations counts how many times the invariant was evaluated.
	// A watcher that observed nothing has proved nothing, and the
	// scenario asserts a floor on this rather than trusting it ran.
	observations int
	// events is every event name observed, in order, so a failure says
	// what the engine was doing and a control can assert the watcher
	// really did see the middle of a move.
	events []string
	// breaches is every instant at which the invariant did not hold.
	breaches []string
	// destroyed is the set of locators this run has watched being
	// deleted and has not seen rewritten.
	destroyed map[string]bool

	// expectBreaches suppresses the immediate failure below, for the one
	// control that plants a breach on purpose (sampler_test.go).
	expectBreaches bool
}

func newWatcher(t *testing.T, j *state.Journal, ids []model.ArtifactID, sufficient ...placement.Class) *watcher {
	if len(sufficient) == 0 {
		sufficient = sufficientClass
	}
	return &watcher{
		t: t, journal: j, ids: ids, sufficient: sufficient,
		destroyed: map[string]bool{},
	}
}

// observe evaluates the invariant for every watched artifact, right now,
// against the durable journal minus whatever this run has destroyed.
func (w *watcher) observe(event string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.observeLocked(event, "")
}

// observeIfGone evaluates the invariant as it WILL be the instant the copy
// at locator stops existing. It is what runs immediately before a delete,
// and it is the check that has to refuse rather than merely record: a
// delete taken past a broken invariant is a lost backup, and this suite
// would rather fail with the copy still there.
func (w *watcher) observeIfGone(event, locator string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if broke := w.observeLocked(event, locator); broke != "" {
		return fmt.Errorf("the standing invariant does not hold, so %s is refused: %s", event, broke)
	}
	return nil
}

// observeLocked is the evaluation itself. pending, when non-empty, is a
// locator to treat as already gone.
func (w *watcher) observeLocked(event, pending string) string {
	w.observations++
	w.events = append(w.events, event)

	var broke string
	for _, id := range w.ids {
		rec, err := w.journal.Get(context.Background(), id)
		if err != nil {
			w.t.Fatalf("the watcher could not read %s at %q: %v", id.Name, event, err)
		}
		surviving := rec
		surviving.Placements = nil
		for _, p := range rec.Placements {
			if w.destroyed[canonicalLocator(p.Location)] {
				continue
			}
			if pending != "" && samePlace(p.Location, pending) {
				continue
			}
			surviving.Placements = append(surviving.Placements, p)
		}
		if err := placement.CheckInvariant(surviving, w.sufficient...); err != nil {
			msg := fmt.Sprintf("at %q: %v (journal said: %s)", event, err, describe(rec))
			w.breaches = append(w.breaches, msg)
			if broke == "" {
				broke = msg
			}
			if !w.expectBreaches {
				// Reported HERE, at the instant, rather than collected for
				// the end of the scenario. A scenario's later assertions
				// are mostly Fatalf, and a breach that surfaced only in a
				// closing tally would be swallowed whenever the breach
				// also derailed the move, which is most of the time. The
				// point of watching continuously is to say when, so the
				// message has to survive whatever happens next.
				w.t.Errorf("FR-30's standing invariant did not hold %s", msg)
			}
		}
	}
	return broke
}

// destroyedNow records that the copy at locator no longer exists, so every
// later observation stops counting its placement row as a copy.
func (w *watcher) destroyedNow(locator string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.destroyed[canonicalLocator(locator)] = true
}

// writtenNow records that a copy exists at locator again.
func (w *watcher) writtenNow(locator string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.destroyed, canonicalLocator(locator))
}

// report closes a scenario out.
//
// Every breach has already failed the test at the instant it happened, so
// this does not repeat them. What it adds is the tally, because a run that
// observed nothing and a run that observed everything and found nothing
// look identical from the outside, and only one of them proves anything.
func (w *watcher) report() {
	w.t.Helper()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.t.Logf("the invariant was evaluated %d times across this scenario, and broke %d times",
		w.observations, len(w.breaches))
	if w.observations == 0 {
		w.t.Error("the watcher never evaluated the invariant at all, so nothing here is watched")
	}
}

// breachCount is for a control that needs to compare two runs.
func (w *watcher) breachCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.breaches)
}

// observationCount is for the assertion that the watcher actually ran.
func (w *watcher) observationCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.observations
}

// sawEvent reports whether the watcher observed an event whose name
// contains substr. A scenario uses it to prove the watcher was awake
// during the interesting phases rather than only at the ends.
func (w *watcher) sawEvent(substr string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, e := range w.events {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}

// eventSummary lists the distinct events observed, sorted, for a failure
// message.
func (w *watcher) eventSummary() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	seen := map[string]int{}
	for _, e := range w.events {
		seen[e]++
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s x%d", k, seen[k]))
	}
	return strings.Join(parts, ", ")
}

// --- the decorators that make it continuous ---------------------------

// watchedJournal observes after every write the engine makes.
//
// Reads are not observed. A read changes nothing, so an observation after
// one would be an observation of the same state the previous write already
// produced, and padding the count with them would make observationCount
// mean less rather than more.
type watchedJournal struct {
	// inner is a MoveJournal rather than the concrete journal so a cell
	// can slip a decorator UNDER the watcher and have the watcher observe
	// what that decorator actually wrote. sampler_test.go's planted
	// breach is exactly that, and a watcher that could only sit on top of
	// the real journal could not be shown to catch one.
	inner placement.MoveJournal
	w     *watcher
}

func (j *watchedJournal) Get(ctx context.Context, artifact model.ArtifactID) (state.Record, error) {
	return j.inner.Get(ctx, artifact)
}

func (j *watchedJournal) ListMoves(ctx context.Context, phases ...string) ([]state.Move, error) {
	return j.inner.ListMoves(ctx, phases...)
}

func (j *watchedJournal) PlanMove(ctx context.Context, p state.MovePlan) (state.Move, error) {
	mv, err := j.inner.PlanMove(ctx, p)
	j.w.observe(fmt.Sprintf("after the journal recorded the intent to move %s to %q", p.Artifact.Name, p.DestinationMedium))
	return mv, err
}

func (j *watchedJournal) AdvanceMove(ctx context.Context, a state.MoveAdvance) (state.Move, error) {
	mv, err := j.inner.AdvanceMove(ctx, a)
	j.w.observe(fmt.Sprintf("after the journal wrote phase %s", a.To))
	return mv, err
}

// watchedStore observes around every call that adds or removes bytes on a
// medium.
type watchedStore struct {
	transport.MediumStore
	w *watcher
}

func (s *watchedStore) UploadFromLocal(ctx context.Context, medium transport.Medium, localPath, key string, opts transport.UploadOptions) (transport.UploadResult, error) {
	res, err := s.MediumStore.UploadFromLocal(ctx, medium, localPath, key, opts)
	if err == nil {
		s.w.writtenNow(key)
	}
	s.w.observe(fmt.Sprintf("after the copy to %q landed on %q", key, medium.ID))
	return res, err
}

func (s *watchedStore) DeleteObject(ctx context.Context, medium transport.Medium, key string) error {
	event := fmt.Sprintf("deleting the object %q on %q", key, medium.ID)
	if err := s.w.observeIfGone(event, key); err != nil {
		return err
	}
	err := s.MediumStore.DeleteObject(ctx, medium, key)
	if err == nil {
		s.w.destroyedNow(key)
	}
	s.w.observe("after " + event)
	return err
}

// watchedLocal is the same, for the local end.
type watchedLocal struct {
	artifactstore.Local
	w *watcher
}

func (l *watchedLocal) Put(ctx context.Context, locator string, r io.Reader) error {
	err := l.Local.Put(ctx, locator, r)
	if err == nil {
		l.w.writtenNow(locator)
	}
	l.w.observe(fmt.Sprintf("after the copy to the local path %q landed", filepath.Base(locator)))
	return err
}

func (l *watchedLocal) Remove(ctx context.Context, locator string) error {
	event := fmt.Sprintf("removing the local copy %q", filepath.Base(locator))
	if err := l.w.observeIfGone(event, locator); err != nil {
		return err
	}
	err := l.Local.Remove(ctx, locator)
	if err == nil {
		l.w.destroyedNow(locator)
	}
	l.w.observe("after " + event)
	return err
}

// canonicalLocator resolves a path so two spellings of one copy compare
// equal. On darwin /var is a symlink to /private/var, and the journal
// records the path the config computed while a guard is handed the path
// an FR-20 containment proof resolved. A mismatch would leave a destroyed
// copy counted as surviving, which is exactly the direction that makes
// this watcher weaker without saying so.
//
// An object key is not a filesystem path and EvalSymlinks will not resolve
// one; it comes back unchanged, which is correct, because two spellings of
// an object key do not arise.
func canonicalLocator(locator string) string {
	if locator == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(locator); err == nil {
		return resolved
	}
	return locator
}

func samePlace(a, b string) bool {
	if a == b {
		return true
	}
	return canonicalLocator(a) == canonicalLocator(b) && canonicalLocator(a) != ""
}

// --- assertions the scenario shares -----------------------------------

// assertBytesAreReal reads every surviving ACTIVE placement back from
// wherever the journal says it is and hashes it.
//
// This is the assertion the watcher cannot make, and the reason it exists
// separately: the watcher checks placement arithmetic at every instant,
// which is cheap enough to do continuously, and this checks the bytes,
// which is a download per copy and is done once at the end of a scenario.
// Neither substitutes for the other. A run with only the first proves the
// journal is self-consistent about copies that might be empty; a run with
// only the second proves the ends are fine and says nothing about the
// middle.
func assertBytesAreReal(t *testing.T, w *world, a seeded) {
	t.Helper()
	rec, err := w.journal.Get(w.ctx, a.id)
	if err != nil {
		t.Fatalf("reading %s: %v", a.id.Name, err)
	}
	var checked int
	for _, p := range rec.Placements {
		if p.Status != state.PlacementActive {
			continue
		}
		checked++
		got := readPlacement(t, w, p)
		if sha256Hex(got) != a.hash {
			t.Errorf("%s's ACTIVE copy on %q does not hold the artifact's bytes", a.id.Name, p.Medium)
		}
	}
	if checked == 0 {
		t.Errorf("%s has no ACTIVE placement at all, so it has no copy: %s", a.id.Name, describe(rec))
	}
}

func readPlacement(t *testing.T, w *world, p state.Placement) []byte {
	t.Helper()
	if p.Medium == state.MediumLocal {
		b, err := os.ReadFile(p.Location)
		if err != nil {
			t.Fatalf("reading the local copy at %q: %v", p.Location, err)
		}
		return b
	}
	medium, ok := w.mediumByID(p.Medium)
	if !ok {
		t.Fatalf("the journal names medium %q, which this scenario does not configure", p.Medium)
	}
	rc, err := adapter().OpenObject(w.ctx, medium, p.Location)
	if err != nil {
		t.Fatalf("opening %s's copy at %q on %q: %v", p.Medium, p.Location, medium.ID, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading %s's copy at %q: %v", p.Medium, p.Location, err)
	}
	return b
}
