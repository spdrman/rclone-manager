package apicontract

// The three values SubmitOperationRequest.Action takes, which is the one
// field on this contract whose VALUE selects a code path rather than
// carrying data.
//
// # Why they are here and hand-written
//
// The contract document types that field as a string with no enum, so the
// generator has nothing to emit and this file is not generated (only
// contract.gen.go is compared against the document by
// scripts/api/check-contract-drift.sh). What the document does say, in
// SubmitOperationRequest's own description, is which action reads which
// parameter object, and these are those three names.
//
// They live in this package because it is the one both sides of the wire
// may import. core/service and core/internal/archive define the action a
// durable operation row is written with, and core/cliecho has to decide
// which `backup-manager` command an action is equivalent to; core/service
// imports core/cliecho, so cliecho cannot read the constants from there.
// Before this file it read string literals instead, and one of them was
// wrong: it switched on "restore", which no client has ever sent, so
// every restore and every per-set run fell through to a default arm whose
// sentence was about a different verb (issue #599 review). A constant
// spelled in one place cannot be wrong in one of them.
//
// core/service.ActionRunCycle, core/service.ActionRunBackupSet and
// core/internal/archive.ActionRestore are defined FROM these, so the wire
// value, the durable row's value and the echoed command all read one
// spelling.
const (
	// ActionRunCycle runs one cycle across every enabled backup set in
	// the deployment.
	ActionRunCycle = "run_cycle"

	// ActionRunBackupSet runs one cycle over exactly the backup set named
	// by SubmitOperationRequest.BackupSetID.
	ActionRunBackupSet = "run_backup_set"

	// ActionRestorePlacement asks a storage provider to make one archived
	// copy readable again, with the parameters in
	// SubmitOperationRequest.Restore.
	ActionRestorePlacement = "restore_placement"
)
