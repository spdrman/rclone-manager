package cliecho

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/spdrman/rclone-manager/core/apicontract"
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
// The request bodies are core/apicontract's own types rather than a map
// read by string keys, deliberately. That is what makes a field renamed in
// the contract a compile error here instead of a builder that quietly
// stops emitting a flag.
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
		why: "there is no verb that reports FR-21's capacity reading. `settings` prints the thresholds it is compared against, not the free space measured against them",
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
		build: func(a Action) *cmd {
			var req apicontract.SubmitOperationRequest
			if !decode(a.Body, &req) {
				return nil
			}
			switch req.Action {
			case "restore":
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
			default:
				// The gap the issue names, and the one a lazier
				// implementation gets wrong. `backup-manager run`
				// exists and is NOT this: usage() puts it among the
				// commands that are "ordinary beside a running engine",
				// so it opens the service in the operator's own process
				// and runs a cycle there. This asks the SERVING engine
				// to run one. Printing `backup-manager run` would print
				// a command that does something different to a
				// different process.
				return newCmd().refuse("`backup-manager run` starts a cycle in your own shell, not in this engine, so it is a different act against a different process")
			}
		},
		why: "`backup-manager run` starts a cycle in your own shell, not in this engine, so it is a different act against a different process",
		examples: []Action{
			{Body: []byte(`{"action":"restore","config_revision":"r1","restore":{"artifact_id":"api-server/var-backups/dump.tar","medium":"offsite_s3","window_days":7,"acknowledged":true}}`)},
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
		why: "`sources` lists every configured backup set; there is no verb that prints one on its own",
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
		why: "there is no verb that tests a connection on its own; a terminal finds out by creating the set or by running its cycle",
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
		why: "`backup-set patch` refuses --disabled, which is a create-only flag, so there is no verb that enables or disables a set that already exists",
	},
	key("POST", "/backup-sets/{source}/{set}/read-only"): {
		why: "`backup-set patch` refuses --read-only, which is a create-only flag, so there is no verb that changes a set's read-only posture after it exists",
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
			if len(req.Tiers) > 0 {
				// A whole tier chain does not fit on a command line and
				// the CLI does not pretend it does: --policy-file takes
				// the contents of a config.yaml "retention:" block. So
				// the flag is named and its value is a placeholder,
				// which is what makes the line say it is not runnable as
				// printed rather than quietly wrong.
				c.placeholderFlag("policy-file", "a file holding these tiers as a retention: block")
			}
			if req.Timezone != "" {
				c.flag("timezone", req.Timezone)
			}
			if req.WeekStartsOn != "" {
				c.flag("week-starts-on", req.WeekStartsOn)
			}
			if req.DailyDays > 0 {
				c.flag("daily-days", itoa(req.DailyDays))
			}
			if req.WeeklyMonths > 0 {
				c.flag("weekly-months", itoa(req.WeeklyMonths))
			}
			if req.MonthlyMonths > 0 {
				c.flag("monthly-months", itoa(req.MonthlyMonths))
			}
			if req.ProtectLastKnownGood != nil {
				c.assigned("protect-last-known-good", *req.ProtectLastKnownGood)
			}
			if req.AcknowledgeMediumDisclosure {
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
		// usage() is explicit that this is deliberate rather than
		// missing: "FR-20 deletion runs through the API's retention
		// preview/apply pair, against a reviewed plan_id".
		why: "deletion runs through the API's preview/apply pair against a reviewed plan_id on purpose; `retention` on a terminal previews and deletes nothing",
	},
	key("GET", "/backup-sets/{source}/{set}/edit-hold"): {
		why: "there is no verb that reads a backup set's edit hold; a terminal edit takes and releases one for the duration of the command",
	},
	key("POST", "/backup-sets/{source}/{set}/edit-hold"): {
		why: "there is no verb that takes an edit hold and leaves it held, because a hold outliving the command that took it is exactly what a browser needs and a terminal does not",
	},
	key("POST", "/backup-sets/{source}/{set}/edit-hold/release"): {
		why: "there is no verb that releases an edit hold, for the same reason there is none that takes one",
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
		why: "there is no verb that lists quarantined backups; `quarantine` only acts on one, and `artifacts` does not filter by quarantine",
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
	key("GET", "/activity"): {
		why: "there is no `activity` verb yet, so the durable journal cannot be read from a terminal at all (issue #598)",
	},
	key("GET", "/activity/live"): {
		why: "there is no `activity --follow` yet, so the live feed this terminal shows cannot be watched from a terminal (issues #598 and #599)",
	},

	// --------------------------------------------------------- storage ---
	key("POST", "/storage-mediums/{id}/preflight"): {
		build:    func(a Action) *cmd { return newCmd("medium", "preflight", a.Params["id"]) },
		examples: []Action{{Params: map[string]string{"id": "offsite_s3"}}},
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
		why: "there is no verb that lists the registered validators; on a terminal an id is named on `backup-set create --validator-id` and refused if it is not one",
	},

	// ------------------------------------------------------------- ssh ---
	key("POST", "/ssh-keys"): {
		// Never a command, and the reason is the interesting half. This
		// request carries private_key_pem and a passphrase, and the only
		// place a key reaches the CLI is as a FILE on
		// `backup-set create --ssh-key-file`. There is no import verb, so
		// there is nothing to print, and what is printed instead names
		// the flag and never the material.
		//
		// #592 gave this route a second mode, selecting a listed
		// candidate by its opaque id, and that half has no CLI either.
		// Both halves are named, because they are different gaps: a
		// pasted key has nowhere to go on a command line, and a selected
		// one has an id that would sit on one perfectly well if a verb
		// took it.
		why: "a key reaches the CLI as a file on `backup-set create --ssh-key-file <the private key file you chose>`; there is no verb that imports one on its own, and the key itself never goes on a command line. Selecting a key already on this machine has no verb either: `backup-manager ssh-key import --candidate ID` would be it",
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
	// test in core/cmd/backup-manager catches on purpose.
	key("GET", "/ssh-keys"): {
		why: "there is no verb that lists the key store, which is why `backup-set patch --ssh-key-id ID` currently takes an id nothing will print for you. `backup-manager ssh-key list` would be it",
	},
	key("GET", "/ssh/key-candidates"): {
		why: "there is no verb that scans this machine for keys it can offer, so the key the installer generated and mounted is reachable from the browser and not from a terminal. `backup-manager ssh-key discover` would be it",
	},

	// -------------------------------------------------------- settings ---
	key("GET", "/settings"): {
		build:    func(Action) *cmd { return newCmd("settings") },
		examples: []Action{{}},
	},
	key("PATCH", "/settings"): {
		build: func(a Action) *cmd {
			var req apicontract.UpdateSettingsRequest
			if !decode(a.Body, &req) {
				return nil
			}
			if req.Retention != nil && len(req.Retention.Tiers) > 0 {
				// usage() says so in as many words: "a full retention
				// tier-chain replacement is still a config-file edit".
				return newCmd().refuse("`settings patch` takes the scalar retention and capacity settings; a full tier-chain replacement is still a config-file edit")
			}
			c := newCmd("settings", "patch")
			if r := req.Retention; r != nil {
				if r.Timezone != nil {
					c.flag("timezone", *r.Timezone)
				}
				if r.WeekStartsOn != nil {
					c.flag("week-starts-on", *r.WeekStartsOn)
				}
				if r.ProtectLastKnownGood != nil {
					c.assigned("protect-last-known-good", *r.ProtectLastKnownGood)
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
			return c
		},
		why: "`settings patch` takes the scalar retention and capacity settings; a full tier-chain replacement is still a config-file edit",
		examples: []Action{
			{Body: []byte(`{"retention":{"timezone":"Europe/Berlin","week_starts_on":"monday","protect_last_known_good":false}}`)},
			{Body: []byte(`{"capacity":{"cap_bytes":1099511627776,"warning_free_bytes":107374182400,"critical_free_bytes":53687091200,"safety_margin_bytes":1073741824}}`)},
		},
	},
}

// backupSetCreateCommand is `backup-set create`, shared by POST
// /backup-sets and POST /system/first-run because they write the same
// thing: usage() puts the first config.yaml on this same verb (#176).
func backupSetCreateCommand(spec apicontract.BackupSetSpec, runNow, acknowledgeRepoint bool) *cmd {
	c := newCmd("backup-set", "create", spec.SourceName+"/"+spec.Name)
	c.flag("host", spec.Host)
	if spec.Port > 0 {
		c.flag("port", itoa(spec.Port))
	}
	c.flag("user", spec.User)
	c.flag("remote-path", spec.RemotePath)
	c.flag("local-path", spec.LocalPath)
	// An id, never key material: the key itself was imported by its own
	// request and this names the copy this deployment already holds.
	c.flag("ssh-key-id", spec.SSHKeyID)
	// A known_hosts line is a PUBLIC host key. It is the one long value
	// on this line and it is deliberately printed rather than placed
	// behind a placeholder: an operator pasting this command needs to
	// pin the same key, and hiding it would make the command unrunnable
	// for no gain.
	c.flag("known-hosts-line", spec.KnownHostsLine)
	c.flag("completion-strategy", spec.CompletionStrategy)
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
