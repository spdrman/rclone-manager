package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
	"github.com/spdrman/rclone-manager/core/internal/state"
)

// This file is issue #227's own safety property, and it is a drift
// property rather than a behavioural one.
//
// FR-15's delete gate refuses a reinstated artifact by asking the
// append-only transition log, one artifact at a time, about every edge in
// ReinstatementEdges() (lastReinstatement, remotedelete.go). Reporting how
// many such artifacts a backup set holds needs the same question asked
// once for the whole set. Two reads of the same fact are two chances to
// disagree, and the one that would go unnoticed is the reporting read
// quietly under-counting a population that is permanent by design.
//
// So ReinstatedArtifacts is required here to agree with the gate's own
// read artifact by artifact, and to ask about exactly the edges the gate
// asks about, on a fixture that contains an artifact of every kind the
// answer could get wrong.

// multiArtifactSet builds one backup set holding five artifacts:
// three reinstated (two through QUARANTINED -> COMMITTED, one through
// QUARANTINED_LOST -> COMPLETE), one still quarantined and never
// reinstated, and one that walked the happy path and was never distrusted
// at all.
//
// The still-quarantined artifact is the control that matters most: a
// reporting read that counted "has ever been quarantined" rather than "was
// reinstated" produces the right answer on any fixture that does not
// contain one.
type multiArtifactSet struct {
	set         model.BackupSetID
	reinstated  []model.ArtifactID
	quarantined model.ArtifactID
	untouched   model.ArtifactID
}

func (m multiArtifactSet) all() []model.ArtifactID {
	return append(append([]model.ArtifactID(nil), m.reinstated...), m.quarantined, m.untouched)
}

func buildMultiArtifactSet(t *testing.T, j *state.Journal) multiArtifactSet {
	t.Helper()
	ctx := context.Background()
	set := mustID(t).Set
	t0 := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	id := func(name string) model.ArtifactID {
		out, err := model.NewArtifactID(set, name)
		if err != nil {
			t.Fatalf("NewArtifactID(%s): %v", name, err)
		}
		return out
	}
	walk := func(a model.ArtifactID, prefix string, edges [][2]State) {
		if _, err := j.Discover(ctx, a, prefix+"-discover", "/incoming/"+a.Name, state.RemoteIdentity{}, t0); err != nil {
			t.Fatalf("Discover %s: %v", a, err)
		}
		for i, e := range edges {
			if _, err := j.RecordTransition(ctx, state.Transition{
				Artifact:   a,
				Key:        prefix + "-" + string(e[1]) + "-" + string(rune('a'+i)),
				From:       string(e[0]),
				To:         string(e[1]),
				OccurredAt: t0.Add(time.Duration(i) * time.Minute),
			}); err != nil {
				t.Fatalf("%s: %s -> %s: %v", a, e[0], e[1], err)
			}
		}
	}

	toCommitted := [][2]State{
		{Discovered, Transferring}, {Transferring, Transferred},
		{Transferred, Verifying}, {Verifying, Verified},
		{Verified, Committing}, {Committing, Committed},
	}
	withMore := func(extra ...[2]State) [][2]State {
		return append(append([][2]State(nil), toCommitted...), extra...)
	}

	m := multiArtifactSet{
		set:         set,
		quarantined: id("still-quarantined.dump"),
		untouched:   id("never-distrusted.dump"),
	}

	first := id("reinstated-one.dump")
	walk(first, "r1", withMore([2]State{Committed, Quarantined}, [2]State{Quarantined, Committed}))

	second := id("reinstated-two.dump")
	walk(second, "r2", withMore([2]State{Committed, Quarantined}, [2]State{Quarantined, Committed}))

	third := id("reinstated-lost.dump")
	walk(third, "r3", withMore(
		[2]State{Committed, RemoteDeletePending},
		[2]State{RemoteDeletePending, Complete},
		[2]State{Complete, QuarantinedLost},
		[2]State{QuarantinedLost, Complete},
	))
	m.reinstated = []model.ArtifactID{first, second, third}

	walk(m.quarantined, "q1", withMore([2]State{Committed, Quarantined}))
	walk(m.untouched, "u1", toCommitted)

	return m
}

// The drift proof. For every artifact in the set, the set-wide read and
// the delete gate's own per-artifact read must give the same answer. A
// disagreement in either direction is a bug: an artifact the gate refuses
// but the report does not count is a silently under-reported permanent
// retention, and an artifact the report counts but the gate would delete
// is a far worse thing than a wrong number.
func TestReinstatedArtifactsAgreesWithTheDeleteGatesOwnRead(t *testing.T) {
	ctx := context.Background()
	j := openTestJournal(t)
	m := buildMultiArtifactSet(t, j)

	got, err := ReinstatedArtifacts(ctx, j, m.set)
	if err != nil {
		t.Fatalf("ReinstatedArtifacts: %v", err)
	}

	inReport := make(map[model.ArtifactID]bool, len(got))
	for _, a := range got {
		inReport[a] = true
	}

	// The count itself, first, so a read that agreed with the gate by
	// both being empty cannot pass.
	if len(got) != len(m.reinstated) {
		t.Fatalf("ReinstatedArtifacts returned %d artifact(s) %v, want the %d that were reinstated %v",
			len(got), got, len(m.reinstated), m.reinstated)
	}

	for _, a := range m.all() {
		_, refusedByGate, err := lastReinstatement(ctx, Deps{Journal: j}, a)
		if err != nil {
			t.Fatalf("lastReinstatement(%s): %v", a, err)
		}
		if inReport[a] != refusedByGate {
			t.Errorf("%s: the set-wide report says reinstated=%v and FR-15's delete gate says reinstated=%v; the two reads have drifted apart",
				a, inReport[a], refusedByGate)
		}
	}

	// The control, stated as its own assertion rather than left implicit
	// in the loop: a quarantined artifact that was never reinstated is
	// not in this population, and neither is one that was never
	// distrusted.
	if inReport[m.quarantined] {
		t.Errorf("%s is quarantined and was never reinstated, so it must not be counted; this count is counting quarantine, not reinstatement", m.quarantined)
	}
	if inReport[m.untouched] {
		t.Errorf("%s walked the happy path and was never distrusted, so it must not be counted", m.untouched)
	}
}

// recordingLog captures the edge set the set-wide read asks about, so the
// assertion below is about the question rather than about this fixture's
// answer to it.
type recordingLog struct {
	edges []state.TransitionEdge
	err   error
}

func (r *recordingLog) ArtifactsWithAnyTransition(_ context.Context, _ model.BackupSetID, edges []state.TransitionEdge) ([]model.ArtifactID, error) {
	r.edges = edges
	return nil, r.err
}

// ReinstatementEdges() is derived from the Transitions table itself, which
// is what makes a future quarantine exit into a durable state covered by
// the delete gate the moment it is declared. The reporting read has to be
// derived from the same place, or a new edge would forfeit a remote delete
// without ever appearing in the count of forfeited remote deletes.
func TestReinstatedArtifactsAsksAboutExactlyTheDeclaredReinstatementEdges(t *testing.T) {
	log := &recordingLog{}
	if _, err := ReinstatedArtifacts(context.Background(), log, mustID(t).Set); err != nil {
		t.Fatalf("ReinstatedArtifacts: %v", err)
	}

	want := ReinstatementEdges()
	if len(want) == 0 {
		t.Fatal("ReinstatementEdges() is empty; this test would pass vacuously")
	}
	if len(log.edges) != len(want) {
		t.Fatalf("ReinstatedArtifacts asked about %v, want exactly the declared reinstatement edges %v", log.edges, want)
	}
	for i, e := range want {
		if log.edges[i].From != string(e.From) || log.edges[i].To != string(e.To) {
			t.Errorf("edge %d: asked about %s -> %s, want %s -> %s", i, log.edges[i].From, log.edges[i].To, e.From, e.To)
		}
	}
}

// A failed read is a failed read. Returning an empty slice and a nil error
// would report "nothing has been reinstated", which is the reassuring
// answer, on a database that could not be queried at all.
func TestReinstatedArtifactsPropagatesAReadFailure(t *testing.T) {
	boom := errors.New("database is locked")
	log := &recordingLog{err: boom}

	_, err := ReinstatedArtifacts(context.Background(), log, mustID(t).Set)
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want it to wrap %v: a failed read must never read as \"none reinstated\"", err, boom)
	}
}
