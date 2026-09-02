package lifecycle

// This file answers one question the FR-21 capacity guard needs and only
// this package can answer honestly: in which lifecycle states does an
// artifact occupy space on the local filesystem?
//
// It lives here, beside the state machine and the transitions that move
// artifacts between those states, rather than in the journal or in whatever
// happens to be summing bytes this week. A list of state names maintained
// anywhere else is a list that drifts the first time a state is added, and
// the failure mode of that drift is silent: a cap computed from a stale
// list simply under-counts, and under-counting a ceiling is how a ceiling
// stops being one. StatesHoldingLocalCopy is derived from AllStates below,
// so a new state has to be classified before this package compiles.

// StatesHoldingLocalCopy is every state in which an artifact has bytes on
// the local filesystem.
//
// The line is drawn where the first local byte is written and where the
// last one would be removed:
//
//   - Discovered has nothing local yet. The artifact has been noticed on
//     the remote and recorded; transfer.go has not run.
//   - Transferring onwards do. transfer.go writes a .partial file, and from
//     that moment the artifact's bytes are on the disk this guard is about.
//     A .partial part-way through a copy occupies less than the artifact's
//     final size, so counting it at full size over-states slightly and in
//     the safe direction (see the package-level note in
//     core/service/usage.go for why every bias here points that way).
//   - Committed through Complete hold the durable final copy, which is the
//     same inode the .partial was: commit.go promotes by hard link, never
//     by copy, so the footprint does not double at any point (see
//     internal/capacity's headroom-arithmetic section).
//   - Failed does NOT. transfer.go removes a stale .partial before every
//     attempt, and a failed transfer leaves nothing this manager considers
//     its own to account for.
//   - Quarantined and QuarantinedLost DO. A quarantined artifact's local
//     copy is deliberately retained (that is the whole point of holding it
//     for a human rather than deleting it), and issue #220's reinstatement
//     path exists precisely because those bytes are still there.
//   - RemoteRetained DOES (issue #282). It is reached from Committed or
//     RemoteDeletePending exactly where Complete normally would be, and
//     neither of those edges touches the local file at all; the same
//     durable final copy Committed already holds is simply never handed
//     off to a delete step for its remote counterpart.
var StatesHoldingLocalCopy = []State{
	Transferring,
	Transferred,
	Verifying,
	Verified,
	Committing,
	Committed,
	RemoteDeletePending,
	Complete,
	RemoteRetained,
	Quarantined,
	QuarantinedLost,
}

// StatesWithNoLocalCopy is the complement, listed explicitly rather than
// computed, so that TestEveryStateIsClassifiedAsHoldingALocalCopyOrNot can
// prove the two together cover AllStates exactly once. A new state added to
// AllStates and to neither list fails that test, which is the only thing
// standing between "we added a state" and "the cap quietly stopped
// counting it".
var StatesWithNoLocalCopy = []State{
	Discovered,
	Failed,
}

// HoldsLocalCopy reports whether an artifact in state s occupies space on
// the local filesystem. An unrecognised state reports false, which is the
// same answer Valid gives it: this package will not claim disk usage on
// behalf of a state it does not define.
func HoldsLocalCopy(s State) bool { return holdsLocalCopy[s] }

var holdsLocalCopy = func() map[State]bool {
	m := make(map[State]bool, len(StatesHoldingLocalCopy))
	for _, s := range StatesHoldingLocalCopy {
		m[s] = true
	}
	return m
}()

// StatesHoldingLocalCopyStrings is StatesHoldingLocalCopy in the plain
// string form the FR-9 journal stores states as, for a caller building a
// query over that column. internal/state cannot import this package (this
// package imports it), so the vocabulary travels as an argument rather than
// being restated there.
func StatesHoldingLocalCopyStrings() []string {
	out := make([]string, 0, len(StatesHoldingLocalCopy))
	for _, s := range StatesHoldingLocalCopy {
		out = append(out, string(s))
	}
	return out
}
