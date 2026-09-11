package cliecho

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/spdrman/backupd/core/apicontract"
)

// The table: every route the /api/v1 router registers, and what an
// operator would have typed instead.
//
// A route is in exactly one of two states, and the parity test in
// apps/common/webhost is what keeps it that way. Either it has a builder,
// or it has a `why` saying there is no equivalent. A route added to the
// router with neither fails that test, which is the whole point of this
// file: a UI action with no command to name becomes a visible line in a
// panel an operator is reading, rather than a gap somebody finds at an
// audit nobody runs.
//
// The `why` lines are written for an operator, not for us. "No equivalent"
// on its own is not actionable; "there is no verb that does X" says what
// would have to be built.
//
// A `why` is also a claim about this binary, and claims go stale: five of
// these said a verb did not exist while the same tree shipped it, and one
// of the five quoted a usage() line that had been replaced by the flag it
// was denying. core/cmd/backupd's TestNoGapClaimsAVerbThisBinaryShips
// reads every sentence here against the verb tables now. A sentence that
// names a shipped verb on purpose, as a counterexample ("`backupd
// run` is not this"), says so with namesShippedVerbs, and a sentence that
// names one by accident fails.
//
// The request bodies are core/apicontract's own types rather than a map
// read by string keys, deliberately. That is what makes a field renamed in
// the contract a compile error here instead of a builder that quietly
// stops emitting a flag.
// The sentences a builder prints in place of a command, as constants
// because each of them is written in two places: the builder that refuses
// with it, and the entry that declares it so Gaps can report it. A
// sentence that could differ between those two would be a sentence the
// guard in core/cmd/backupd checks a copy of.
const (
	gapRunCycle = "`" + Binary + " run` starts a cycle in your own shell, not in this engine, so it is a different act against a different process"

	gapRunBackupSet = "`" + Binary + " fetch --backup-set <source/backup-set>` runs that set's cycle in your own shell, not in this engine, so it is a different act against a different process"

	gapDeploymentScope = "there is no flag that narrows `activity --follow` to the deployment's own events: --backup-set names one set, and naming none already means every set"

	// Issue #624. The candidate half of the connection check has no verb
	// and deliberately gets none, which is a decision rather than the next
	// gap to close. A candidate is a form somebody is still typing: its
	// values exist nowhere, so the command would have to carry all of
	// them, and the moment they are worth carrying is the create, which
	// proves the connection before it writes anything.
	gapCandidateConnection = "there is no verb that checks a source that is not saved yet: `" + Binary + " backup-set create` proves the connection before it writes, and `" + Binary + " backup-set test-connection <source/backup-set>` re-checks one that exists"

	// I2.2 (issue #669). Two gaps, and both are real rather than
	// oversights, which is the distinction this table exists to make.
	//
	// `medium add` and `medium edit` take the flags they were written
	// with - --type, --region, --endpoint, --bucket, --prefix,
	// --storage-class, --upload-verification and the four credential
	// spellings - and a manifest can declare a field none of them names.
	// local_volume's `path` is exactly that, so a local volume cannot be
	// configured from a terminal at all. Printing --path anyway would
	// read correctly and fail on execution, which is worse than printing
	// nothing: the entire reason these lines exist is that an operator
	// can copy them.
	gapConfigureMedium = "there is no verb that writes a destination's configuration in its backend's own field names: `" + Binary + " medium add` and `" + Binary + " medium edit` carry one flag per S3 field and have no --path, so a local volume cannot be configured from a terminal until they take a manifest's field ids"

	// And a configuration that has not been written cannot be checked by
	// id, because there is no id yet. `medium test-connection` checks
	// what is SAVED, which is a different question and the one an
	// operator would get a misleading answer to.
	gapPreflightMediumConfiguration = "there is no verb that checks a destination's configuration before it is written: `" + Binary + " medium test-connection <medium-id>` checks the one already saved, and `" + Binary + " medium add` proves a destination as it declares it"
)

// positiveQuery reads a query parameter that is a count, and reports 0
// for anything the route itself would treat as absent.
//
// The routes that take one (a limit on the journal read and on the live
// feed) treat an absent, unparseable or non-positive value alike, as the
// backend's own default. A line printing --limit 0 or --limit banana
// would name a value the request did not make and, in the second case,
// one the binary would refuse.
func positiveQuery(a Action, name string) int {
	raw := a.Query.Get(name)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

var routes = map[string]entry{
	// ---------------------------------------------------------- system ---
	key("GET", "/system/version"): {
		build:    func(Action) *cmd { return newCmd("version") },
		examples: []Action{{}},
	},
	key("GET", "/system/health"): {
		build:    func(Action) *cmd { return newCmd("status") },
		examples: []Action{{}},
	},
	key("GET", "/system/capabilities"): {
		why: "capabilities are what the PLATFORM this deployment runs on can do, which is a question about the host rather than about backups; nothing on a terminal asks it",
	},
	key("GET", "/system/storage"): {
		why:               "there is no verb that reports FR-21's capacity reading. `settings` prints the thresholds it is compared against, not the free space measured against them",
		namesShippedVerbs: []string{"settings"},
	},
	key("GET", "/system/first-run"): {
		why: "there is no verb that asks whether this instance has been configured; on a terminal the answer is whether config.yaml exists",
	},
	key("POST", "/system/first-run"): {
		// The first config.yaml, which usage() puts on `backup-set
		// create`: "On an instance with no config.yaml yet this writes
		// the first one instead (#176)". Same act, same verb.
		build: func(a Action) *cmd {
			var spec apicontract.BackupSetSpec
			if !decode(a.Body, &spec) {
				return nil
			}
			return backupSetCreateCommand(spec, false, false)
		},
		why:      "there is no verb that writes a first configuration from a request body",
		examples: []Action{{Body: []byte(`{"source_name":"api-server","name":"var-backups","host":"10.0.0.14","port":22,"user":"backups","remote_path":"/var/backups","local_path":"/data/backups","ssh_key_id":"key_1","known_hosts_line":"10.0.0.14 ssh-ed25519 AAAAC3Nz","completion_strategy":"rename","include":["*.gz"],"stale_after_seconds":172800,"validator_id":"gzip","read_only":true,"disabled":true}`)}},
	},

	// ------------------------------------------------------ operations ---
	key("POST", "/operations"): {
		// Three actions arrive here and they are three different acts, so
		// this switch is on apicontract's own constants rather than on
		// string literals. It used to be on "restore", which is not one
		// of the three and never was: the client sends restore_placement
		// and run_backup_set, so NO real request matched that arm. Every
		// restore and every per-set run fell through to a default whose
		// sentence is about `backupd run`, a different verb for a
		// different act, and the example body said "restore" too, so the
		// dispatcher-driven parse test certified a branch production
		// never reaches. A constant spelled in one place cannot be wrong
		// in one of them.
		build: func(a Action) *cmd {
			var req apicontract.SubmitOperationRequest
			if !decode(a.Body, &req) {
				return nil
			}
			switch req.Action {
			case apicontract.ActionRestorePlacement:
				if req.Restore == nil {
					return nil
				}
				c := newCmd("restore", req.Restore.ArtifactID).flag("medium", req.Restore.Medium)
				if req.Restore.WindowDays > 0 {
					c.flag("days", itoa(req.Restore.WindowDays))
				}
				if req.Restore.Acknowledged {
					c.bare("acknowledge")
				}
				return c
			case apicontract.ActionRunBackupSet:
				// Its own sentence, because it is its own act. `fetch`
				// runs one backup set's cycle and is the verb an operator
				// reaches for, and it is not this for the same reason
				// `run` is not a run_cycle: usage() puts both among the
				// commands that are "ordinary beside a running engine",
				// so it opens the service in the operator's own process
				// and runs the set THERE, while this asks the serving
				// engine to run it. Answering with run_cycle's sentence,
				// which is what happened until #599's review, sends
				// somebody asking about one set to a verb about all of
				// them.
				return newCmd().refuse(gapRunBackupSet)
			default:
				// The gap the issue names, and the one a lazier
				// implementation gets wrong. `backupd run`
				// exists and is NOT this: usage() puts it among the
				// commands that are "ordinary beside a running engine",
				// so it opens the service in the operator's own process
				// and runs a cycle there. This asks the SERVING engine
				// to run one. Printing `backupd run` would print
				// a command that does something different to a
				// different process.
				return newCmd().refuse(gapRunCycle)
			}
		},
		// A body that will not decode names no action at all, so the
		// route-level sentence is the one every other builder here gives
		// for that case rather than one of the two below, which are
		// answers about a specific act.
		why:               "there is no verb that submits an operation from a request body",
		refusals:          []string{gapRunCycle, gapRunBackupSet},
		namesShippedVerbs: []string{"run", "fetch"},
		examples: []Action{
			{Body: []byte(`{"action":"` + apicontract.ActionRestorePlacement + `","config_revision":"r1","restore":{"artifact_id":"api-server/var-backups/dump.tar","medium":"offsite_s3","window_days":7,"acknowledged":true}}`)},
			{Body: []byte(`{"action":"` + apicontract.ActionRunBackupSet + `","config_revision":"r1","backup_set_id":"api-server/var-backups"}`)},
			{Body: []byte(`{"action":"` + apicontract.ActionRunCycle + `","config_revision":"r1"}`)},
		},
	},
	key("GET", "/operations"): {
		why: "there is no verb that lists submitted operations; a cycle started from a terminal runs in that terminal and reports there",
	},
	key("GET", "/operations/{id}"): {
		why: "there is no verb that polls one submitted operation, for the same reason the list above has none",
	},

	// ----------------------------------------------------- backup sets ---
	key("GET", "/backup-sets"): {
		build:    func(Action) *cmd { return newCmd("sources") },
		examples: []Action{{}},
	},
	key("GET", "/backup-sets/*"): {
		why:               "`sources` lists every configured backup set; there is no verb that prints one on its own",
		namesShippedVerbs: []string{"sources"},
	},
	key("POST", "/backup-sets"): {
		build: func(a Action) *cmd {
			var req apicontract.CreateBackupSetRequest
			if !decode(a.Body, &req) {
				return nil
			}
			return backupSetCreateCommand(req.BackupSetSpec, req.RunImmediately, req.AcknowledgeRepoint)
		},
		why: "there is no verb that creates a backup set from a request body",
		examples: []Action{
			{Body: []byte(`{"source_name":"api-server","name":"var-backups","host":"10.0.0.14","user":"backups","remote_path":"/var/backups","local_path":"/data/backups","ssh_key_id":"key_1","known_hosts_line":"10.0.0.14 ssh-ed25519 AAAAC3Nz","completion_strategy":"stable","stable_for_seconds":300,"run_immediately":true,"acknowledge_repoint":true}`)},
		},
	},
	key("POST", "/backup-sets/test-connection"): {
		// This entry was a `why` line for two issues. #596 built the
		// check, so the route answered with six named steps rather than a
		// boolean, and nothing could reach it from a terminal; the gap
		// sentence named the verb that had to exist rather than saying
		// "no equivalent", which is what makes a gap actionable. #624
		// shipped that verb, so this is a builder now.
		//
		// Only the PERSISTED mode has one. A candidate check is the step
		// the wizard runs while an operator is still filling a form in,
		// against values that exist nowhere yet: the equivalent command
		// would have to carry the whole unsaved form, and what a terminal
		// can do with it is run the create, which verifies before it
		// writes. So the candidate mode refuses with the reason rather
		// than printing a command that checks something else.
		build: func(a Action) *cmd {
			var req apicontract.TestConnectionRequest
			if !decode(a.Body, &req) {
				return nil
			}
			if req.BackupSetID == "" {
				return newCmd().refuse(gapCandidateConnection)
			}
			return newCmd("backup-set", "test-connection", req.BackupSetID)
		},
		why:      "there is no verb that checks a source that has not been saved yet",
		refusals: []string{gapCandidateConnection},
		// `backup-set` is named as the counterexample rather than as the
		// gap: the refusal's whole point is that the thing to run instead
		// is `backup-set create`, which proves the connection before it
		// writes, or `backup-set test-connection` once the set exists.
		namesShippedVerbs: []string{"backup-set"},
		examples: []Action{
			{Body: []byte(`{"backup_set_id":"api-server/var-backups"}`)},
			{Body: []byte(`{"host":"10.0.0.14","user":"backups","ssh_key_id":"key_1","known_hosts_line":"10.0.0.14 ssh-ed25519 AAAAC3Nz","remote_path":"/var/backups"}`)},
		},
	},
	key("PATCH", "/backup-sets/{source}/{set}"): {
		build: func(a Action) *cmd {
			var req apicontract.UpdateBackupSetRequest
			if !decode(a.Body, &req) {
				return nil
			}
			c := newCmd("backup-set", "patch", setID(a))
			if req.Host != nil {
				c.flag("host", *req.Host)
			}
			if req.Port != nil {
				c.flag("port", itoa(*req.Port))
			}
			if req.User != nil {
				c.flag("user", *req.User)
			}
			if req.RemotePath != nil {
				c.flag("remote-path", *req.RemotePath)
			}
			if req.LocalPath != nil {
				c.flag("local-path", *req.LocalPath)
			}
			if req.Include != nil {
				c.flag("include", strings.Join(*req.Include, ","))
			}
			if req.CompletionStrategy != nil {
				c.flag("completion-strategy", *req.CompletionStrategy)
			}
			if req.StableForSeconds != nil {
				c.flag("stable-for", seconds(*req.StableForSeconds))
			}
			if req.StaleAfterSeconds != nil {
				c.flag("stale-after", seconds(*req.StaleAfterSeconds))
			}
			if req.ValidatorID != nil {
				c.flag("validator-id", *req.ValidatorID)
			}
			if req.SSHKeyID != nil {
				c.flag("ssh-key-id", *req.SSHKeyID)
			}
			if req.KnownHostsLine != nil {
				c.flag("known-hosts-line", *req.KnownHostsLine)
			}
			if req.AcknowledgeRepoint {
				c.bare("acknowledge-repoint")
			}
			if req.AcknowledgeHostKeyChange {
				c.bare("acknowledge-host-key-change")
			}
			return c
		},
		why: "there is no verb that edits a backup set from a request body",
		examples: []Action{
			{Params: map[string]string{"source": "api-server", "set": "var-backups"},
				Body: []byte(`{"stale_after_seconds":172800}`)},
			// The duration shapes, and they are examples rather than a
			// unit test's table because this is the corpus the dispatcher
			// is driven with in core/cmd/backupd: a shape no
			// example carries is a shape nothing parses end to end. These
			// three are the ones the old renderer got wrong (a bare
			// seconds value, a value whose last unit ends in a zero, and
			// an hour whose minutes component is empty), and they were
			// missing for the reason such gaps usually are: the two
			// values above are the two it happened to render correctly.
			{Params: map[string]string{"source": "api-server", "set": "var-backups"},
				Body: []byte(`{"stale_after_seconds":30,"stable_for_seconds":600}`)},
			{Params: map[string]string{"source": "api-server", "set": "var-backups"},
				Body: []byte(`{"stale_after_seconds":3610,"stable_for_seconds":90}`)},
			{Params: map[string]string{"source": "api-server", "set": "var-backups"},
				Body: []byte(`{"host":"10.0.0.15","port":2222,"user":"backups","remote_path":"/var/backups","local_path":"/data/backups","include":["*.gz","*.sql"],"completion_strategy":"stable","stable_for_seconds":300,"validator_id":"gzip","ssh_key_id":"key_2","known_hosts_line":"10.0.0.15 ssh-ed25519 AAAAC3Nz","acknowledge_repoint":true,"acknowledge_host_key_change":true}`)},
		},
	},
	key("DELETE", "/backup-sets/{source}/{set}"): {
		build:    func(a Action) *cmd { return newCmd("backup-set", "remove", setID(a)) },
		examples: []Action{{Params: map[string]string{"source": "api-server", "set": "var-backups"}}},
	},
	key("POST", "/backup-sets/{source}/{set}/enabled"): {
		// A real gap, and one this feature is how anybody noticed.
		// --disabled is in backupSetCreateOnlyFlags, so `backup-set
		// patch` refuses it: a set can be created disabled from a
		// terminal and never enabled or disabled again from one.
		why:               "`backup-set patch` refuses --disabled, which is a create-only flag, so there is no verb that enables or disables a set that already exists",
		namesShippedVerbs: []string{"backup-set"},
	},
	key("POST", "/backup-sets/{source}/{set}/read-only"): {
		why:               "`backup-set patch` refuses --read-only, which is a create-only flag, so there is no verb that changes a set's read-only posture after it exists",
		namesShippedVerbs: []string{"backup-set"},
	},
	key("GET", "/backup-sets/{source}/{set}/retention"): {
		build:    func(a Action) *cmd { return newCmd("backup-set", "retention", setID(a)) },
		examples: []Action{{Params: map[string]string{"source": "api-server", "set": "var-backups"}}},
	},
	key("PUT", "/backup-sets/{source}/{set}/retention"): {
		build: func(a Action) *cmd {
			var req apicontract.RetentionOverride
			if !decode(a.Body, &req) {
				return nil
			}
			c := newCmd("backup-set", "retention", setID(a))
			wrote := false
			if len(req.Tiers) > 0 {
				// A whole tier chain does not fit on a command line and
				// the CLI does not pretend it does: --policy-file takes
				// the contents of a config.yaml "retention:" block. So
				// the flag is named and its value is a placeholder,
				// which is what makes the line say it is not runnable as
				// printed rather than quietly wrong.
				//
				// And it is named ALONE. --policy-file carries the whole
				// retention section, so `backup-set retention` refuses it
				// beside --timezone and the rest (backupsetretention.go's
				// second mutual-exclusion rule); a body carrying tiers
				// and a timezone would otherwise have printed a line the
				// binary exits 2 on. Those values are in the block, which
				// is what the placeholder says.
				c.placeholderFlag("policy-file", "a file holding this whole retention: block")
				wrote = true
			} else {
				if req.Timezone != "" {
					c.flag("timezone", req.Timezone)
					wrote = true
				}
				if req.WeekStartsOn != "" {
					c.flag("week-starts-on", req.WeekStartsOn)
					wrote = true
				}
				if req.DailyDays > 0 {
					c.flag("daily-days", itoa(req.DailyDays))
					wrote = true
				}
				if req.WeeklyMonths > 0 {
					c.flag("weekly-months", itoa(req.WeeklyMonths))
					wrote = true
				}
				if req.MonthlyMonths > 0 {
					c.flag("monthly-months", itoa(req.MonthlyMonths))
					wrote = true
				}
				if req.ProtectLastKnownGood != nil {
					c.assigned("protect-last-known-good", *req.ProtectLastKnownGood)
					wrote = true
				}
			}
			// The acknowledgement consents to a policy write, and the CLI
			// refuses it on a command line that writes no policy, so it
			// only goes on beside one.
			if req.AcknowledgeMediumDisclosure && wrote {
				c.bare("acknowledge-medium-disclosure")
			}
			return c
		},
		why: "there is no verb that sets a backup set's retention policy from a request body",
		examples: []Action{
			{Params: map[string]string{"source": "api-server", "set": "var-backups"},
				Body: []byte(`{"daily_days":7,"weekly_months":3,"monthly_months":12,"timezone":"Europe/Berlin","week_starts_on":"monday","protect_last_known_good":false,"acknowledge_medium_disclosure":true}`)},
			{Params: map[string]string{"source": "api-server", "set": "var-backups"},
				Body: []byte(`{"tiers":[{"name":"daily","granularity":"day","keep":7}]}`)},
			// A chain AND the scalars that go in the block with it, which
			// is the shape that used to print --policy-file beside
			// --timezone and get exit 2.
			{Params: map[string]string{"source": "api-server", "set": "var-backups"},
				Body: []byte(`{"tiers":[{"name":"daily","granularity":"day","keep":7}],"timezone":"Europe/Berlin","week_starts_on":"monday","acknowledge_medium_disclosure":true}`)},
		},
	},
	key("DELETE", "/backup-sets/{source}/{set}/retention"): {
		build: func(a Action) *cmd {
			return newCmd("backup-set", "retention", setID(a)).bare("inherit")
		},
		examples: []Action{{Params: map[string]string{"source": "api-server", "set": "var-backups"}}},
	},
	key("GET", "/backup-sets/{source}/{set}/retention/preview"): {
		build:    func(a Action) *cmd { return newCmd("retention", setID(a)) },
		examples: []Action{{Params: map[string]string{"source": "api-server", "set": "var-backups"}}},
	},
	key("POST", "/backup-sets/{source}/{set}/retention/apply"): {
		// This said `retention` on a terminal previews and deletes
		// nothing, quoting a usage() line that #602 has since replaced:
		// `retention apply <source/backup-set> --acknowledge` is in this
		// tree, in retentionapply.go, and it deletes.
		//
		// The plan_id is not on the line and cannot be: over HTTP a plan
		// is issued, rendered for an administrator and applied against a
		// fingerprint of exactly what they were shown, while on a
		// terminal the preview and the apply are one invocation and the
		// person is the one who typed it. Both go through the same
		// PreviewRetention/ApplyRetentionPlan pair and both are refused
		// with RETENTION_PLAN_STALE if anything moved in between, so the
		// act is the same act. --acknowledge is the terminal's half of
		// the confirmation and is required, which is why it is on the
		// line rather than conditional on anything in the body.
		build: func(a Action) *cmd {
			return newCmd("retention", "apply", setID(a)).bare("acknowledge")
		},
		examples: []Action{{Params: map[string]string{"source": "api-server", "set": "var-backups"},
			Body: []byte(`{"plan_id":"plan_01HX"}`)}},
	},
	// Two of these three are commands now. #600 built `backup-set
	// edit-hold <source/backup-set> [--release]`, which reports a hold
	// and gives one back, so the entries saying no verb read or released
	// one were describing a tree that had one open in the next file.
	key("GET", "/backup-sets/{source}/{set}/edit-hold"): {
		build:    func(a Action) *cmd { return newCmd("backup-set", "edit-hold", setID(a)) },
		examples: []Action{{Params: map[string]string{"source": "api-server", "set": "var-backups"}}},
	},
	key("POST", "/backup-sets/{source}/{set}/edit-hold"): {
		// The one that is still a gap, and usage() says the same thing in
		// as many words: "There is no verb that TAKES a hold". A hold
		// protects an editing session, a browser has one and a terminal
		// does not, and a CLI edit is one `backup-set patch` that either
		// runs or does not.
		why:               "`backup-set edit-hold` reports a hold and releases one, and no verb TAKES one and leaves it held: a hold outliving the command that took it is exactly what a browser needs and a terminal does not",
		namesShippedVerbs: []string{"backup-set"},
	},
	key("POST", "/backup-sets/{source}/{set}/edit-hold/release"): {
		build: func(a Action) *cmd {
			return newCmd("backup-set", "edit-hold", setID(a)).bare("release")
		},
		examples: []Action{{Params: map[string]string{"source": "api-server", "set": "var-backups"}}},
	},

	// -------------------------------------------------------- backups ---
	key("GET", "/backups"): {
		build: func(a Action) *cmd {
			c := newCmd("artifacts")
			if set := a.Query.Get("backup_set"); set != "" {
				c.flag("backup-set", set)
			}
			if source := a.Query.Get("source"); source != "" {
				c.flag("source", source)
			}
			return c
		},
		examples: []Action{
			{},
			{Query: mustQuery("backup_set=api-server/var-backups")},
			{Query: mustQuery("source=api-server")},
		},
	},
	key("GET", "/backups/{source}/{set}/{name}"): {
		build:    func(a Action) *cmd { return newCmd("artifacts", artifactID(a)) },
		examples: []Action{{Params: map[string]string{"source": "api-server", "set": "var-backups", "name": "dump.tar"}}},
	},
	key("POST", "/backups/{source}/{set}/{name}/retry"): {
		build: func(a Action) *cmd {
			var req apicontract.RetryFailedRequest
			c := newCmd("retry", artifactID(a))
			if decode(a.Body, &req) && req.Note != "" {
				c.flag("note", req.Note)
			}
			return c
		},
		examples: []Action{
			{Params: map[string]string{"source": "api-server", "set": "var-backups", "name": "dump.tar"}},
			{Params: map[string]string{"source": "api-server", "set": "var-backups", "name": "dump.tar"},
				Body: []byte(`{"note":"the NAS came back"}`)},
		},
	},

	// ------------------------------------------------------ quarantine ---
	key("GET", "/quarantine"): {
		why:               "there is no verb that lists quarantined backups; `quarantine` only acts on one, and `artifacts` does not filter by quarantine",
		namesShippedVerbs: []string{"artifacts", "quarantine"},
	},
	key("POST", "/quarantine/{source}/{set}/{name}/revalidate"): {
		build:    func(a Action) *cmd { return newCmd("quarantine", "revalidate", artifactID(a)) },
		examples: []Action{{Params: map[string]string{"source": "api-server", "set": "var-backups", "name": "dump.tar"}}},
	},
	key("POST", "/quarantine/{source}/{set}/{name}/retry"): {
		build:    func(a Action) *cmd { return newCmd("quarantine", "retry", artifactID(a)) },
		examples: []Action{{Params: map[string]string{"source": "api-server", "set": "var-backups", "name": "dump.tar"}}},
	},
	key("POST", "/quarantine/{source}/{set}/{name}/reinstate"): {
		build:    func(a Action) *cmd { return newCmd("quarantine", "reinstate", artifactID(a)) },
		examples: []Action{{Params: map[string]string{"source": "api-server", "set": "var-backups", "name": "dump.tar"}}},
	},

	// -------------------------------------------------------- activity ---
	// Both of these said "there is no verb yet" and both verbs are in
	// this same tree: #598 built `activity` and `activity --follow`, and
	// main.go has dispatched them since. A gap that outlives the thing it
	// describes is worse than no gap at all, because it tells an operator
	// a feature does not exist while the binary beside them ships it, and
	// TestNoGapClaimsAVerbThisBinaryShips is what stops it happening
	// again.
	key("GET", "/activity"): {
		build: func(a Action) *cmd {
			c := newCmd("activity")
			// The one query parameter this route reads that a command can
			// say. The handler treats an absent, unparseable or
			// non-positive value as the backend's own default, so a line
			// that printed --limit 0 would be naming a value the request
			// did not make.
			if n := positiveQuery(a, "limit"); n > 0 {
				c.flag("limit", itoa(n))
			}
			// `before` is deliberately not echoed, for the same reason
			// `since` is not on the live route below: it is a browser's
			// paging cursor into a record it is already partway through,
			// there is no flag for it, and a command run at a terminal
			// starts at the newest page. Printing one that named a cursor
			// would be printing a flag this binary does not have.
			return c
		},
		examples: []Action{{}, {Query: mustQuery("limit=50")}},
	},
	key("GET", "/activity/live"): {
		build: func(a Action) *cmd {
			// scope=deployment narrows the read to the deployment's own
			// bucket and no set at all, and --backup-set cannot say it:
			// naming no set already means every set. So this is a real
			// gap on a route that otherwise has a command, and it is
			// named as one rather than being answered with the wider
			// read, which would print a command that shows an operator
			// more than the panel they are reading does.
			if a.Query.Get("scope") == "deployment" {
				return newCmd().refuse(gapDeploymentScope)
			}
			c := newCmd("activity").bare("follow")
			if set := a.Query.Get("backup_set"); set != "" {
				c.flag("backup-set", set)
			}
			if n := positiveQuery(a, "limit"); n > 0 {
				c.flag("limit", itoa(n))
			}
			// `since` is deliberately not echoed. It is the browser's
			// resume cursor, a sequence number from a feed it was already
			// reading, and a follow started at a terminal starts from
			// now; there is no flag for it and there should not be one.
			return c
		},
		refusals:          []string{gapDeploymentScope},
		namesShippedVerbs: []string{"activity"},
		examples: []Action{
			{},
			{Query: mustQuery("backup_set=api-server%2Fvar-backups&limit=25")},
			{Query: mustQuery("since=42")},
			{Query: mustQuery("scope=deployment")},
		},
	},

	// --------------------------------------------------------- storage ---
	//
	// The whole storage surface, and every route on it is a real command
	// rather than a gap. `medium preflight` was the only verb until G2.2
	// (#594) added list, show, add, edit, remove and import-credentials,
	// which is what made a destination something an operator can configure
	// from a terminal instead of only by hand-editing config.yaml, and it
	// is why nothing down here has to say "there is no verb that".
	//
	// # What these lines can and cannot carry
	//
	// `medium` declares no flag that takes credential MATERIAL: the
	// material reaches the binary only on standard input, and the four
	// credential flags name an id this deployment minted, a path, a
	// variable NAME, and a command. Three of those four are references and
	// go on the line exactly as the request sent them.
	//
	// The fourth is not a reference, and neither is the endpoint. A
	// credentials command is a program and its arguments, written by
	// whoever made the request, and an endpoint URL takes userinfo in a
	// spelling rclone accepts and this contract does not validate. Both
	// were printed in full until #599's review, on a panel that is
	// copy-to-clipboard and exportable. So a command is named with a
	// placeholder and never printed, an endpoint has its userinfo removed,
	// and mediumSpecFlags says which of its flags are which. Everything
	// else down here is the command that actually works, byte for byte.
	key("GET", "/storage-mediums"): {
		build:    func(Action) *cmd { return newCmd("medium", "list") },
		examples: []Action{{}},
	},
	key("GET", "/storage-mediums/{id}"): {
		build:    func(a Action) *cmd { return newCmd("medium", "show", a.Params["id"]) },
		examples: []Action{{Params: map[string]string{"id": "offsite_s3"}}},
	},
	key("GET", "/storage-mediums/{id}/usage"): {
		// `show` again, and not a usage verb, because there is no usage
		// verb and that is deliberate rather than missing: `medium show`
		// prints the destination and then FR-30's report of what is on
		// it, on the reasoning its own doc gives, that whether anything
		// is there and whether it is the only copy is the fact an
		// operator is actually asking about when they look one up. Two
		// routes answering to one command is the honest line here; a gap
		// would be inventing a missing verb for a question the CLI does
		// answer.
		build:    func(a Action) *cmd { return newCmd("medium", "show", a.Params["id"]) },
		examples: []Action{{Params: map[string]string{"id": "offsite_s3"}}},
	},
	key("POST", "/storage-mediums"): {
		// `medium add` proves the destination before it writes it, and so
		// does this route since #636: the check is the engine's now,
		// whoever the caller is. The wizard still preflights first, on the
		// route below, and then saves here, so the two lines an operator
		// reads back are a candidate preflight followed by an add and the
		// add re-proves what the preflight just proved. That is a repeated
		// check rather than a wrong command.
		//
		// --no-verify is echoed when, and only when, the request asked to
		// skip. The browser never does: the wizard cannot save until its
		// own check has come back green, so there is nothing there to
		// skip. A request that DID skip and a line that did not would be a
		// printed command that behaves differently from the action it
		// claims to be the equivalent of, which is the one thing this
		// whole surface exists to avoid.
		build: func(a Action) *cmd {
			var req apicontract.StorageMediumRequest
			if !decode(a.Body, &req) {
				return nil
			}
			return mediumSkipFlag(mediumSpecFlags(newCmd("medium", "add", req.ID), req), req)
		},
		why: "there is no verb that declares a storage destination from a request body",
		examples: []Action{
			{Body: []byte(`{"id":"offsite_s3","type":"s3","region":"eu-central-1","bucket":"acme-backups","prefix":"prod","storage_class":"STANDARD_IA","upload_verification":"readback","credentials":{"credentials_id":"cred_01HX"}}`)},
			{Body: []byte(`{"id":"offsite_s3","type":"s3","region":"eu-central-1","bucket":"acme-backups","credentials":{"command":["aws-vault","exec","backups"]}}`)},
		},
	},
	key("PUT", "/storage-mediums/{id}"): {
		build: func(a Action) *cmd {
			var req apicontract.StorageMediumRequest
			if !decode(a.Body, &req) {
				return nil
			}
			// The path id, never the body's. That is this route's own
			// rule rather than a choice made here: a body naming a
			// different destination is refused outright, so by the time
			// anything is written the two agree, and echoing the path id
			// is echoing the one that was edited.
			return mediumSkipFlag(mediumSpecFlags(newCmd("medium", "edit", a.Params["id"]), req), req)
		},
		why: "there is no verb that edits a storage destination from a request body",
		examples: []Action{
			{Params: map[string]string{"id": "offsite_s3"},
				Body: []byte(`{"region":"eu-west-1","storage_class":"GLACIER_IR"}`)},
			{Params: map[string]string{"id": "offsite_s3"},
				Body: []byte(`{"type":"s3","region":"eu-west-1","endpoint":"https://s3.eu-west-1.example.net","bucket":"acme-backups","prefix":"prod","storage_class":"STANDARD","upload_verification":"attested","credentials":{"file":"/etc/backupd/aws-credentials"}}`)},
		},
	},
	key("DELETE", "/storage-mediums/{id}"): {
		build:    func(a Action) *cmd { return newCmd("medium", "remove", a.Params["id"]) },
		examples: []Action{{Params: map[string]string{"id": "offsite_s3"}}},
	},
	key("POST", "/storage-mediums/preflight"): {
		// The candidate form of the same verb, which is the whole reason
		// `preflight` takes --candidate: this route proves a destination
		// that has not been declared, and the flag is what says the id on
		// the line names a description rather than something already in
		// the configuration.
		build: func(a Action) *cmd {
			var req apicontract.StorageMediumRequest
			if !decode(a.Body, &req) {
				return nil
			}
			return mediumSpecFlags(newCmd("medium", "preflight", req.ID).bare("candidate"), req)
		},
		why: "there is no verb that proves an undeclared storage destination from a request body",
		examples: []Action{
			{Body: []byte(`{"id":"offsite_s3","type":"s3","region":"eu-central-1","endpoint":"https://s3.eu-central-1.example.net","bucket":"acme-backups","prefix":"prod","storage_class":"STANDARD","upload_verification":"readback","credentials":{"env":"BACKUP_MANAGER_S3_CREDENTIALS"}}`)},
		},
	},
	key("POST", "/storage-mediums/{id}/preflight"): {
		// `test-connection`, not `preflight`, since H2.2 (#622). The
		// check is the same one it always was and `preflight` still runs
		// it, kept as an alias so anything scripted against it goes on
		// working. What changed is the NAME an operator reads: the same
		// idea was called "Verify" in the browser, `preflight` on the
		// command line and "Test connection" on the source side of the
		// same product, and three names for one thing is three things to
		// an operator. This line echoes the name the button now carries,
		// because a command line printed under a button that says
		// something else teaches the wrong word.
		build:    func(a Action) *cmd { return newCmd("medium", "test-connection", a.Params["id"]) },
		examples: []Action{{Params: map[string]string{"id": "offsite_s3"}}, {Params: map[string]string{"id": "local"}}},
	},
	key("GET", "/storage-mediums/{id}/configuration"): {
		// `medium show` again, for /usage's reason: there is no
		// configuration-read verb and there does not need to be, because
		// what `medium show` prints IS the destination's fields. Two
		// routes answering to one command is the honest line.
		build:    func(a Action) *cmd { return newCmd("medium", "show", a.Params["id"]) },
		examples: []Action{{Params: map[string]string{"id": "offsite_s3"}}},
	},
	key("POST", "/storage-mediums/{id}/configuration/preflight"): {
		// A whole-route gap and not a builder that refuses: there is no
		// request on this route that HAS an equivalent, so the honest
		// shape is no builder at all.
		why:               gapPreflightMediumConfiguration,
		namesShippedVerbs: []string{"medium"},
	},
	key("PUT", "/storage-mediums/{id}/configuration"): {
		why:               gapConfigureMedium,
		namesShippedVerbs: []string{"medium"},
	},
	key("PUT", "/storage-mediums/{id}/default"): {
		// Moving the destination a newly created retention tier starts on
		// (#622). One verb, one operand, and no flags: the whole content
		// of the request is which destination, and the id in the path is
		// the only place it appears.
		build:    func(a Action) *cmd { return newCmd("medium", "default", a.Params["id"]) },
		examples: []Action{{Params: map[string]string{"id": "offsite_s3"}}, {Params: map[string]string{"id": "local"}}},
	},
	key("POST", "/storage-credentials"): {
		// The one request in this whole contract that carries S3
		// credential MATERIAL, and so the one line in this file that
		// could have leaked one.
		//
		// It cannot, and not because this builder is careful with the
		// body: it never reads the body at all, because the command it
		// names takes nothing from it. `medium import-credentials
		// --stdin` reads the shared-credentials text from standard
		// input, which is not in the process table and not in shell
		// history, and --stdin is required rather than assumed precisely
		// so that this is the only shape the command has.
		//
		// So this is a working invocation printed in full: no
		// placeholder, nothing starred out, and no "not runnable as
		// printed" note, because it IS runnable as printed. An operator
		// pastes it, pipes their credentials in, and gets back the same
		// id the wizard got.
		build:    func(Action) *cmd { return newCmd("medium", "import-credentials").bare("stdin") },
		examples: []Action{{}},
	},

	// --------------------------------------------------------- catalog ---
	key("POST", "/catalog/scan"): {
		build:    func(Action) *cmd { return newCmd("catalog", "rebuild").bare("dry-run") },
		examples: []Action{{}},
	},
	key("POST", "/catalog/rebuild"): {
		build:    func(Action) *cmd { return newCmd("catalog", "rebuild") },
		examples: []Action{{}},
	},

	// ------------------------------------------------------ validators ---
	key("GET", "/validators"): {
		why:               "there is no verb that lists the registered validators; on a terminal an id is named on `backup-set create --validator-id` and refused if it is not one",
		namesShippedVerbs: []string{"backup-set"},
	},

	// ------------------------------------------------------- backends ---
	// EPIC I (#664). A gap entry rather than a command, and the gap is
	// real: there is no verb that lists the registered backends at all.
	// `medium add --backend <id>` takes one and refuses an id no manifest
	// declares, so a terminal operator learns the set from a refusal
	// rather than from a listing. Naming the missing verb here is the
	// point of this table: a parity gap that is written down is a
	// decision, and inventing a `medium backends` verb to satisfy this
	// test would invert the rule, which exists so a missing verb is
	// VISIBLE rather than absent.
	key("GET", "/backends"): {
		why:               "there is no verb that lists the registered backends; on a terminal one is named on `medium add --backend <id>` and refused if no manifest declares it",
		namesShippedVerbs: []string{"medium"},
	},

	// ------------------------------------------------------------- ssh ---
	key("POST", "/ssh-keys"): {
		// Never a command, and the reason is the interesting half. This
		// request carries private_key_pem and a passphrase, and the only
		// place a key reaches the CLI is as a FILE on
		// `backup-set create --ssh-key-file`. There is no import verb, so
		// there is nothing to print, and what is printed instead names
		// the flag and never the material.
		why:               "a key reaches the CLI as a file on `backup-set create --ssh-key-file <the private key file you chose>`; there is no verb that imports one on its own, and the key itself never goes on a command line",
		namesShippedVerbs: []string{"backup-set"},
	},
	// The other import (#592), and a DIFFERENT gap from the one above,
	// which is why it is a separate sentence rather than a clause on that
	// one. A pasted key has nowhere to go on a command line at all. A
	// selected candidate has an opaque id that would sit on one
	// perfectly well, and the only thing missing is a verb that takes it.
	key("POST", "/ssh-keys/from-candidate"): {
		why: "there is no verb that imports a key this machine already holds; the id is an opaque handle that would sit on a command line perfectly well, and `" + Binary + " ssh-key import --candidate ID` would be it",
	},
	key("POST", "/ssh/host-key-probe"): {
		why: "there is no verb that probes a host key on its own; a terminal settles it with --trust-host-key or --known-hosts-line while creating or patching a set",
	},

	// The two reads the wizard's first step is built on (#592). Both are
	// gaps rather than commands, and the gap sentence is doing real work
	// here: `backup-set patch --ssh-key-id ID` has existed since #572 and
	// takes an id nobody has, because nothing on either surface would
	// print one. So this is the exact shape EPIC G's parity rule exists
	// to make visible, and it names the verbs that would close it rather
	// than saying "no equivalent".
	//
	// They stay gaps until those verbs exist. Printing a command this
	// binary does not declare would be printing something an operator
	// pastes and gets exit 2 from, which the dispatcher-driven parity
	// test in core/cmd/backupd catches on purpose.
	key("GET", "/ssh-keys"): {
		why:               "there is no verb that lists the key store, which is why `backup-set patch --ssh-key-id ID` currently takes an id nothing will print for you. `" + Binary + " ssh-key list` would be it",
		namesShippedVerbs: []string{"backup-set"},
	},
	key("GET", "/ssh/key-candidates"): {
		why: "there is no verb that scans this machine for keys it can offer, so the key the installer generated and mounted is reachable from the browser and not from a terminal. `" + Binary + " ssh-key discover` would be it",
	},

	// -------------------------------------------------------- settings ---
	key("GET", "/settings"): {
		build:    func(Action) *cmd { return newCmd("settings") },
		examples: []Action{{}},
	},
	key("PATCH", "/settings"): {
		// This refused a tier-chain replacement outright, and quoted
		// usage() as its authority: "a full retention tier-chain
		// replacement is still a config-file edit". usage() now says the
		// opposite, because #595 put --policy-file on `settings patch`,
		// so the sentence was quoting a line that no longer existed while
		// the flag it denied was three files away.
		build: func(a Action) *cmd {
			var req apicontract.UpdateSettingsRequest
			if !decode(a.Body, &req) {
				return nil
			}
			c := newCmd("settings", "patch")
			policy := false
			if r := req.Retention; r != nil {
				if len(r.Tiers) > 0 {
					// The chain, as a file. --policy-file carries the
					// whole retention section and `settings patch`
					// refuses it beside --timezone and the rest, so a
					// request that also carried those scalars has them
					// inside the block rather than beside it.
					c.placeholderFlag("policy-file", "a file holding this retention: block")
					policy = true
				} else {
					if r.Timezone != nil {
						c.flag("timezone", *r.Timezone)
						policy = true
					}
					if r.WeekStartsOn != nil {
						c.flag("week-starts-on", *r.WeekStartsOn)
						policy = true
					}
					if r.ProtectLastKnownGood != nil {
						c.assigned("protect-last-known-good", *r.ProtectLastKnownGood)
						policy = true
					}
				}
			}
			if cap := req.Capacity; cap != nil {
				if cap.CapBytes != nil {
					c.flag("cap-bytes", itoa64(*cap.CapBytes))
				}
				if cap.WarningFreeBytes != nil {
					c.flag("warning-free-bytes", itoa64(*cap.WarningFreeBytes))
				}
				if cap.CriticalFreeBytes != nil {
					c.flag("critical-free-bytes", itoa64(*cap.CriticalFreeBytes))
				}
				if cap.SafetyMarginBytes != nil {
					c.flag("safety-margin-bytes", itoa64(*cap.SafetyMarginBytes))
				}
			}
			// The acknowledgement rides beside a policy write and the CLI
			// refuses it on a line that writes none.
			if req.AcknowledgeMediumDisclosure && policy {
				c.bare("acknowledge-medium-disclosure")
			}
			return c
		},
		why: "there is no verb that patches the deployment's settings from a request body",
		examples: []Action{
			{Body: []byte(`{"retention":{"timezone":"Europe/Berlin","week_starts_on":"monday","protect_last_known_good":false}}`)},
			{Body: []byte(`{"capacity":{"cap_bytes":1099511627776,"warning_free_bytes":107374182400,"critical_free_bytes":53687091200,"safety_margin_bytes":1073741824}}`)},
			{Body: []byte(`{"retention":{"tiers":[{"name":"daily","granularity":"day","keep":7}],"timezone":"Europe/Berlin"},"acknowledge_medium_disclosure":true}`)},
			{Body: []byte(`{"retention":{"tiers":[{"name":"daily","granularity":"day","keep":7}]},"capacity":{"cap_bytes":1099511627776}}`)},
		},
	},
}

// backupSetCreateCommand is `backup-set create`, shared by POST
// /backup-sets and POST /system/first-run because they write the same
// thing: usage() puts the first config.yaml on this same verb (#176).
//
// Every string field is echoed only when the request carried it. The case
// that matters is a create REFUSED for a missing field: the line beside
// this one says what was missing, and this one has to be the command that
// was asked for, not --user followed by an empty string, which is a
// different command (an empty user rather than no user) and not one
// anybody can fill in and run.
func backupSetCreateCommand(spec apicontract.BackupSetSpec, runNow, acknowledgeRepoint bool) *cmd {
	c := newCmd("backup-set", "create", spec.SourceName+"/"+spec.Name)
	c.flagIfSet("host", spec.Host)
	if spec.Port > 0 {
		c.flag("port", itoa(spec.Port))
	}
	c.flagIfSet("user", spec.User)
	c.flagIfSet("remote-path", spec.RemotePath)
	c.flagIfSet("local-path", spec.LocalPath)
	// An id, never key material: the key itself was imported by its own
	// request and this names the copy this deployment already holds.
	c.flagIfSet("ssh-key-id", spec.SSHKeyID)
	// A known_hosts line is a PUBLIC host key. It is the one long value
	// on this line and it is deliberately printed rather than placed
	// behind a placeholder: an operator pasting this command needs to
	// pin the same key, and hiding it would make the command unrunnable
	// for no gain.
	c.flagIfSet("known-hosts-line", spec.KnownHostsLine)
	c.flagIfSet("completion-strategy", spec.CompletionStrategy)
	if len(spec.Include) > 0 {
		c.flag("include", strings.Join(spec.Include, ","))
	}
	if spec.StableForSeconds > 0 {
		c.flag("stable-for", seconds(spec.StableForSeconds))
	}
	if spec.StaleAfterSeconds > 0 {
		c.flag("stale-after", seconds(spec.StaleAfterSeconds))
	}
	if spec.ValidatorID != "" {
		c.flag("validator-id", spec.ValidatorID)
	}
	if spec.Disabled {
		c.bare("disabled")
	}
	if spec.ReadOnly {
		c.bare("read-only")
	}
	if runNow {
		c.bare("run")
	}
	if acknowledgeRepoint {
		c.bare("acknowledge-repoint")
	}
	return c
}

// mediumSpecFlags puts a storage destination's description onto a `medium`
// command line, shared by add, edit and the candidate preflight.
//
// One helper for the three because both surfaces already treat them as
// one: core/cmd/backupd declares a single flag set that all seven
// verbs read, and this API sends a single body shape to all three of
// these routes, on the reasoning that what is proven and what is saved
// must not be able to be different destinations.
//
// Two of these flags are printed differently from what the request
// carried, and both are corrections to a claim this doc used to make. It
// said no flag here could carry credential material, as a fact about
// apicontract.StorageMediumRequest rather than a rule applied here. That
// was true of the three credential spellings that name a reference (an id
// this deployment minted, a path, a variable NAME) and false of the other
// two fields a secret can reach this function in:
//
//   - Endpoint is a free string this contract deliberately does not
//     validate, and https://AKIA...:wJalr...@minio.internal:9000 is a
//     spelling rclone accepts. endpointWithoutUserinfo takes the
//     credential out and leaves the address.
//   - Credentials.Command is a program and its arguments, both the
//     caller's words. It is named with a placeholder and never printed.
//
// What holds that now is not this paragraph. It is
// TestNoRequestFieldReachesTheArgvUnlessItIsEchoedOnPurpose, which drives
// every builder with a body whose every string field, on every schema in
// the contract, carries a distinct canary, and fails on any canary that
// reaches an argv behind a flag nobody wrote an exemption for.
//
// flagIfSet throughout, for backupSetCreateCommand's reason: a field the
// request did not carry must not echo as a flag with an empty value after
// it, which is a different command from the one that was made. Nothing is
// lost on an edit, either, because this wire shape has plain strings
// rather than pointers, so the request itself cannot tell "leave the
// prefix alone" from "clear the prefix"; the line says exactly as much as
// the request did.
func mediumSpecFlags(c *cmd, req apicontract.StorageMediumRequest) *cmd {
	c.flagIfSet("type", req.Type)
	c.flagIfSet("region", req.Region)
	// The endpoint with any userinfo taken out of it. See
	// endpointWithoutUserinfo: a credential can be spelled into this
	// field, and this is the one flag here whose value is not printed
	// exactly as the request sent it.
	c.flagIfSet("endpoint", endpointWithoutUserinfo(req.Endpoint))
	c.flagIfSet("bucket", req.Bucket)
	c.flagIfSet("prefix", req.Prefix)
	c.flagIfSet("storage-class", req.StorageClass)
	c.flagIfSet("upload-verification", req.UploadVerification)
	c.flagIfSet("credentials-id", req.Credentials.CredentialsID)
	c.flagIfSet("credentials-file", req.Credentials.File)
	c.flagIfSet("credentials-env", req.Credentials.Env)
	if len(req.Credentials.Command) > 0 {
		// Named and never printed, unlike its three siblings. Those name
		// a reference this deployment already holds; this one is a
		// program and its arguments, both of them the caller's words, and
		// `printf %s AKIA...:wJalr...` is a valid credentials command and
		// a credential on a command line. No test can tell that apart
		// from an innocent one, so the value does not go on the line at
		// all and the line says it is not runnable as printed.
		c.placeholderFlag("credentials-command", "the command that prints these credentials")
	}
	return c
}

// mediumSkipFlag echoes issue #636's opt-out when the request carried it.
//
// It is separate from mediumSpecFlags because that function is shared with
// the CANDIDATE preflight, where the field means nothing: that route IS
// the check, so there is nothing for it to skip, and a `medium
// test-connection --candidate --no-verify` line would be an instruction
// nobody can mean.
//
// Bare rather than assigned, because the flag's own default is false: an
// operator retyping this line gets the skip by naming it and gets the
// check by leaving it off, which is the same shape `--candidate` beside
// it has.
func mediumSkipFlag(c *cmd, req apicontract.StorageMediumRequest) *cmd {
	if req.SkipConnectionCheck {
		return c.bare("no-verify")
	}
	return c
}

func itoa(v int) string     { return strconv.FormatInt(int64(v), 10) }
func itoa64(v int64) string { return strconv.FormatInt(v, 10) }

// mustQuery builds an example's query string. It panics rather than
// returning an error because the only caller is this file's own literals,
// so a bad one is a bug in this file and not a runtime condition.
func mustQuery(raw string) url.Values {
	v, err := url.ParseQuery(raw)
	if err != nil {
		panic("cliecho: bad example query " + raw + ": " + err.Error())
	}
	return v
}
