// Package lifecycle defines the artifact state machine for FR-10.
//
// An artifact moves through the eleven states FR-10 names, plus one more
// this package adds while working the issue (QUARANTINED_LOST, see below),
// between being noticed on a remote and being fully retired there. The
// string form of each state is exactly one of the uppercase names below,
// and that string is a contract with the FR-9 journal: the journal stores
// it as a plain string column and owns neither the Go type nor the rules
// for how it may change. This package owns both.
//
//	DISCOVERED
//	TRANSFERRING
//	TRANSFERRED
//	VERIFYING
//	VERIFIED
//	COMMITTING
//	COMMITTED
//	REMOTE_DELETE_PENDING
//	COMPLETE
//	REMOTE_RETAINED    (a second terminal state, issue #282: see below)
//	FAILED             (exceptional)
//	QUARANTINED        (exceptional, recoverable)
//	QUARANTINED_LOST   (exceptional, terminal)
//
// # Why a graph, not a set of constants
//
// A crash can land the process anywhere. If any caller can assign any state
// to any artifact, "restart-safe" becomes a property every call site has to
// re-verify by hand, forever. This package makes the rule mechanical
// instead: Validate and Machine.Apply consult the single Transitions table
// declared in machine.go, so a move that table doesn't list fails at the
// point it's attempted, and a test that walks the table is a complete proof
// of what the machine allows. See machine.go for that table and for the
// crash-safety reasoning behind the transitions that matter most.
//
// # Why QUARANTINED_LOST exists
//
// FR-10 names only QUARANTINED as the content-is-suspect state. Working out
// its exits surfaced a gap FR-17's reconciliation table doesn't cover
// either: COMPLETE means the remote source is already confirmed deleted, so
// an artifact whose only local copy is found corrupted after COMPLETE has
// no source left anywhere to recover from. Routing that case through the
// same QUARANTINED -> DISCOVERED exit as a recoverable quarantine would ask
// the pipeline to rediscover and re-transfer something that no longer
// exists, which fails, lands in FAILED, and FAILED -> DISCOVERED sends it
// right back around: a livelock, and one that also mislabels an
// irrecoverable loss as an ordinary retryable failure. QUARANTINED_LOST is
// the state that loss is recorded in instead, reachable only from COMPLETE
// (see TestOnlyCompletePrecedesQuarantinedLost), and it has no path back
// into the pipeline at all. Leaving it requires an operator to act, not
// another automatic retry: its one exit is back to the COMPLETE it came
// from, and only when the operator can prove the durable local copy is
// intact after all (issue #220, see machine.go's "Reinstatement" section
// and TestCompleteCannotLivelockThroughQuarantine).
package lifecycle

// This file is the vocabulary: the thirteen state names, the type that
// carries one, and the parse and validity checks that keep a raw string from
// becoming a State without being looked at.
//
// It holds no rules about MOVEMENT at all. Which state may follow which is
// machine.go's table, and the separation is deliberate: a name is a fact
// about what an artifact is, and an edge is a claim about what may happen
// next, and mixing them is how a constant file grows opinions nobody
// reviews. The package doc above sits here because this is the file that
// introduces the vocabulary the rest of the package is written in.

// State is one named point in the FR-10 artifact lifecycle.
//
// It is a defined string type rather than an int, because the journal stores
// exactly this string and an operator reads exactly this string in a log
// line. An integer with a lookup table would make the stored form an
// implementation detail that could be renumbered, and the stored form is a
// contract with every row already on disk.
type State string

// The thirteen states. Their string forms are a contract with the FR-9
// journal and with every log line an operator has already read, so a name
// here is not renameable without a migration and an FR-35 conversation.
const (
	// Discovered is the entry point: the artifact exists on the remote and
	// the manager has recorded it, but nothing has moved yet.
	Discovered State = "DISCOVERED"

	// Transferring means a copy from the remote to a local .partial file
	// (FR-11, FR-12) is underway.
	Transferring State = "TRANSFERRING"

	// Transferred means the copy finished. The local file still has its
	// .partial name and has not been verified yet.
	Transferred State = "TRANSFERRED"

	// Verifying means the required and configured checks (FR-13) are
	// running against the transferred file.
	Verifying State = "VERIFYING"

	// Verified means every required check passed. The content is vouched
	// for; only durable commit remains.
	Verified State = "VERIFIED"

	// Committing means the local file is being fsynced and renamed from its
	// .partial name to its final name, followed by a directory fsync
	// (FR-11's "durably commit local file").
	Committing State = "COMMITTING"

	// Committed means the local file durably exists at its final name. From
	// here on the backup has already succeeded, regardless of what happens
	// to the remote copy next.
	Committed State = "COMMITTED"

	// RemoteDeletePending means the manager has durably recorded its intent
	// to delete the source object and has not yet done so. This is the only
	// state a remote delete call may ever be issued from.
	RemoteDeletePending State = "REMOTE_DELETE_PENDING"

	// Complete means the remote object is confirmed gone, or reconciliation
	// independently confirmed it was already gone, and the local durable
	// copy is retained. This is the happy-path terminal state.
	Complete State = "COMPLETE"

	// RemoteRetained means this artifact's backup set is declared
	// read-only (issue #282, config.BackupSet.ReadOnly): FR-15's delete
	// step is never offered this artifact, transport.Transport.DeleteRemote
	// is never called for it, and the remote object is retained by policy
	// rather than pending deletion. It is a second happy-path terminal
	// state, reached from Committed or RemoteDeletePending exactly where
	// Complete normally would be, and it exists so that a read-only set's
	// artifacts stop being re-offered to the delete gate every cycle and
	// logging a refusal each time -- the exact complaint the issue makes
	// about a config that can only delay, never withhold, consent to
	// delete. Unlike Complete, reaching this state says nothing about
	// whether the remote object still exists: this manager never checked,
	// on purpose, because it was never going to touch it either way.
	RemoteRetained State = "REMOTE_RETAINED"

	// Failed means the pipeline could not produce a valid backup on this
	// attempt because of a permanent, non-retryable error (FR-22). It is
	// never reached once the artifact is Committed: by then the backup has
	// already succeeded, so there's no "the backup failed" left to say.
	Failed State = "FAILED"

	// Quarantined means one specific artifact's content is suspect and
	// needs a human, either because a validator rejected it (FR-13) or
	// because later reconciliation (FR-17) found the durable local copy had
	// gone bad after the fact, while a remote copy still exists (or hasn't
	// been confirmed gone) to recover from. It has two operator-only exits
	// and no automatic one: back to Discovered, which throws the local copy
	// away and re-fetches, and back to Committed, which keeps the local
	// copy and trusts it again on re-checked evidence (issue #220).
	// Contrast QuarantinedLost, where the source is confirmed gone and only
	// the second kind of recovery is available.
	Quarantined State = "QUARANTINED"

	// QuarantinedLost means an artifact's durable local copy was found
	// corrupted after Complete, when the remote source is already confirmed
	// deleted. There is no source left to re-fetch from, so this state has
	// no automatic exit at all and no route back into the pipeline:
	// retrying would only rediscover nothing and fail again. What it does
	// have is an operator-only way back to the Complete it came from, taken
	// when the local copy turns out to be intact after all and the finding
	// was the mistake, an unmounted volume being the case that motivated it
	// (issue #220). This is FR-10's one addition beyond the names the issue
	// lists, added to close a gap FR-17's reconciliation table doesn't
	// cover (see the package doc).
	QuarantinedLost State = "QUARANTINED_LOST"
)

// AllStates lists every state this package recognizes, happy path first and
// then the exceptional states. Tests use it to try every state as a
// candidate for some role (predecessor, successor, starting point) instead
// of hand-maintaining a second list that can drift from the constants above.
var AllStates = []State{
	Discovered,
	Transferring,
	Transferred,
	Verifying,
	Verified,
	Committing,
	Committed,
	RemoteDeletePending,
	Complete,
	RemoteRetained,
	Failed,
	Quarantined,
	QuarantinedLost,
}

// validStates is AllStates as a set, built once at init.
//
// It is derived from AllStates rather than written out again, which is the
// same discipline the tests follow: a state added to the constants and to
// AllStates becomes valid automatically, and a state added to the constants
// alone is caught by TestAllStatesAreValidAndDistinct rather than silently
// being rejected at runtime by a lookup nobody remembered to update.
var validStates = func() map[State]bool {
	m := make(map[State]bool, len(AllStates))
	for _, s := range AllStates {
		m[s] = true
	}
	return m
}()

// Valid reports whether s is one of the states this package defines. A
// journal row holding anything else isn't this package's idempotent no-op
// case, it's a bug: a schema drift, a hand-edited row, or a build mismatch
// between the journal and the state machine.
func (s State) Valid() bool { return validStates[s] }

// String satisfies fmt.Stringer so a State prints as its contract form in
// logs (FR-23) without a cast.
func (s State) String() string { return string(s) }

// ParseState validates a raw string, as read back from the FR-9 journal's
// string column, and returns the matching State. It refuses anything that
// isn't exactly one of the names this package defines.
func ParseState(raw string) (State, error) {
	s := State(raw)
	if !s.Valid() {
		return "", &UnknownStateError{Raw: raw}
	}
	return s, nil
}
