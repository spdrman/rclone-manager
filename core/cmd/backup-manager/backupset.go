package main

import (
	"strings"
)

// cmdBackupSet is `backup-manager backup-set <verb> <source/backup-set>
// [flags]`: the operations that act on one already-configured backup set.
//
// # The shape, and why it is a noun then a verb
//
// A noun then a verb, the same as `catalog rebuild`, `quarantine
// revalidate` and `settings patch`. The operand form is fixed: the verb,
// then exactly one backup set id, which is always "source/name".
//
// This dispatcher exists as its own file because it is a MEETING POINT.
// Issue #350 is landing `backup-set patch` into it and issue #356 is
// landing `backup-set create`, and this issue lands `backup-set
// retention`. All three are the same noun operating on the same operand,
// so they belong in one command rather than three top-level verbs that
// each rediscover how to split an id. They also conflict textually here,
// which is the loud kind: merging two of these is combining a switch, and
// nothing is silently lost.
//
// # Why the retention verb is not a flag on `patch`
//
// A per-set retention override is a whole policy that either exists or
// does not, not a field you patch. `patch`'s contract is "every flag is a
// pointer and an unpassed one leaves that field alone", and "this set
// should have no policy of its own" cannot be expressed inside a contract
// where absence already means the opposite. The verb here is the same
// three operations the API's own sub-resource has, for the same reason:
// see core/service/backupsetretention.go's package doc, and
// backupsetretention.go beside this file.
// backupSetVerbs is every verb this command dispatches on, and the map is
// where a new one is added rather than a new branch in a switch.
//
// Each handler is given the WHOLE argument list, its own verb included,
// and finds that verb as its first operand. That is what lets flags
// appear on either side of it, so `backup-set --config X retention a/b`
// works exactly as `settings --config X patch` already does. A dispatcher
// that sliced the verb off would silently drop every flag written before
// it, which reads as a command that ran against the wrong configuration
// file rather than as an error.
var backupSetVerbs = map[string]func([]string) int{
	"retention": cmdBackupSetRetention,
}

func cmdBackupSet(args []string) int {
	for _, a := range args {
		if verb, ok := backupSetVerbs[a]; ok {
			return verb(args)
		}
	}
	return usageError(`backup-set: expected a verb; the only one this build has is "retention" (see --help)`)
}

// isBackupSetID reports whether id has a backup set id's shape.
//
// A backup set id is exactly source/name (core/internal/model's own rule),
// so a value with no separator, two of them, or an empty half is refused
// by the caller with a message that says what the shape is, rather than
// reaching the service and coming back as a not-found for something that
// was never an id at all.
func isBackupSetID(id string) bool {
	return strings.Count(id, "/") == 1 && !strings.HasPrefix(id, "/") && !strings.HasSuffix(id, "/")
}
