package lifecycle

import "strings"

// NameSet renders the members of set as the list a person reads in a
// refusal, a HELP line or a doc comment: "COMMITTED, REMOTE_DELETE_PENDING,
// COMPLETE or REMOTE_RETAINED".
//
// It exists because of issue #505. Every message that told an operator
// which states a decision accepts had the list typed out by hand beside the
// map that actually decides, and when REMOTE_RETAINED joined those maps
// (issue #282) the sentences stayed at three. The sharpest one sat directly
// under retention's own predicate and told the operator of a read-only
// backup set, by name, that REMOTE_RETAINED is not permitted, which is the
// only state that set's artifacts ever reach. Nothing behaved wrongly; the
// product just described itself wrongly to the person it was refusing, and
// they had a fault to go hunting for that did not exist.
//
// A message built from the map cannot say that. Pass the same map the
// predicate consults and the sentence moves when the map moves, which is
// the whole point: the next state added to one of these sets writes itself
// into every refusal that quotes it, with nobody having to remember.
//
// The order is AllStates order, not map order and not alphabetical, so the
// list reads down the lifecycle the way the package doc's diagram does and
// two calls never disagree about the order. A member that is not one of
// this package's states cannot be rendered in that order at all, so it is
// appended afterwards rather than silently dropped: a set holding a state
// this package does not define is a bug worth seeing in the message.
//
// An empty set renders as the empty string. No caller should ever hold one
// (a predicate that accepts nothing is its own defect), and inventing a
// placeholder here would only hide it one layer further in.
func NameSet(set map[State]bool) string {
	var names []string
	for _, s := range AllStates {
		if set[s] {
			names = append(names, string(s))
		}
	}
	for _, s := range sortedUnknownMembers(set) {
		names = append(names, string(s))
	}
	return joinWithOr(names)
}

// sortedUnknownMembers returns every member of set that AllStates does not
// list, in a stable order so the rendered sentence is deterministic.
func sortedUnknownMembers(set map[State]bool) []State {
	var out []State
	for s, in := range set {
		if !in || s.Valid() {
			continue
		}
		out = append(out, s)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// joinWithOr renders names as "a", "a or b", or "a, b or c".
func joinWithOr(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}

// DurableRestorePointNames is durableRestorePoints rendered by NameSet: the
// states in which a durable local final copy exists and the artifact counts
// as a restore point, spelled the way an operator reads them.
//
// internal/metrics quotes it in the HELP text for the newest-known-good
// gauge, which is the FR-24 surface where the old hand-typed three-state
// list was doing the most quiet damage: a read-only backup set's artifacts
// are all REMOTE_RETAINED, so the HELP line named none of the states that
// gauge was actually measuring for that operator.
func DurableRestorePointNames() string { return NameSet(durableRestorePoints) }
