package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/core/internal/model"
)

// Validate checks a Config for every problem this package knows how to
// catch, and fills in the documented default for every field that has one.
//
// Those two things happen together on purpose. A Config that comes back
// from a successful Validate call is fully resolved: no retention tier is
// still zero, ProtectLastKnownGood is never nil, and every backup set's ID
// is populated. Downstream code (retention, discovery, health) can read
// those fields directly and never has to re-derive "what does zero mean
// here", which is exactly the kind of scattered, ad hoc check this function
// exists to replace with one, tested, place.
//
// Validate collects every problem it finds rather than returning on the
// first one: a config wrong in three places should not cost an operator
// three restarts to fix.
//
// Validate is safe to call more than once on the same Config: every default
// it fills in is only applied when the field is still at its zero value, so
// a second call is a no-op.
func (c *Config) Validate() error {
	v := &validator{}

	if c.PollInterval.Duration() <= 0 {
		v.addf("poll_interval: must be set to a positive duration (got %s)", c.PollInterval)
	}

	v.validateState(c.State)

	if len(c.Sources) == 0 {
		v.addf("sources: at least one source is required")
	}

	seenSourceNames := map[string]bool{}
	seenSetIDs := map[string]string{} // BackupSetID.String() -> the field path that first claimed it

	for i := range c.Sources {
		src := &c.Sources[i]
		path := fmt.Sprintf("sources[%d]", i)

		if src.Name == "" {
			v.addf("%s: id must not be empty", path)
		} else if seenSourceNames[src.Name] {
			v.addf("%s: duplicate source id %q", path, src.Name)
		}
		seenSourceNames[src.Name] = true

		if len(src.BackupSets) == 0 {
			v.addf("%s: at least one backup set is required", path)
		}

		for j := range src.BackupSets {
			bsPath := fmt.Sprintf("%s.backup_sets[%d]", path, j)
			v.validateBackupSet(bsPath, src.Name, src.ReadOnly, &src.BackupSets[j], seenSetIDs)
		}
	}

	v.validateRetention(&c.Retention)
	v.validateAlerts(&c.Alerts)
	v.validateCapacity(&c.Capacity)

	return v.err()
}

// validateAlerts resolves the Alerts block (docs/EPIC-B-multi-nas.md
// §71). Enabled needs no checking: both of its values are meaningful, and
// false is the safe one.
//
// RepeatedFailureThreshold gets the same treatment validateRetention
// already gives a retention tier, and for the same reason. A key left out
// of the YAML file arrives here as a literal zero, and reading that
// literally would mean "alert as soon as a single artifact fails", which
// is not what omitting a key asks for; it is how an operator who turned
// alerting on gets a notification per failed transfer and switches the
// whole thing back off. So zero means "the documented default" and a
// negative number is refused outright as a config mistake rather than
// clamped, since there is no sensible reading of a negative count of
// failures.
func (v *validator) validateAlerts(a *Alerts) {
	switch {
	case a.RepeatedFailureThreshold == 0:
		a.RepeatedFailureThreshold = DefaultRepeatedFailureThreshold
	case a.RepeatedFailureThreshold < 0:
		v.addf("alerts.repeated_failure_threshold: must be a positive number of failed artifacts (got %d)", a.RepeatedFailureThreshold)
	}
}

// validateCapacity checks FR-21's configuration block (issue #286).
//
// It resolves nothing. Every other block in this file fills in a documented
// default for a field left at zero, and this one deliberately does not:
// zero is the meaning here, not the absence of one. capacity.cap_bytes at
// zero is "no cap, use the whole volume", and the two thresholds at zero
// are "no line here", which is what every deployment written before this
// block existed has been running with and must keep running with after an
// upgrade.
//
// What it does instead is refuse three configurations that are individually
// well-formed and jointly mean something an operator did not intend:
//
//   - A negative byte count anywhere. Nothing below zero has a meaning, and
//     for the cap the refusal has to say so out loud, because "-1" is a
//     plausible way for somebody to try to spell "no cap".
//   - A warning line below the critical floor, including the common case of
//     a critical floor with no warning line at all.
//     internal/capacity.Thresholds.Validate refuses that pair, so accepting
//     it here would mean every storage reading on the running deployment
//     coming back "misconfigured" with nothing naming the two numbers that
//     did it.
//   - A cap at or below the critical floor. Each number is fine; together
//     they mean no transfer can ever be admitted, because finishing any of
//     them would leave the allowance at or under the floor. Left to run, it
//     is indistinguishable from a broken product.
func (v *validator) validateCapacity(c *Capacity) {
	if c.CapBytes < 0 {
		v.addf("capacity.cap_bytes: must not be negative (got %d); use 0 for no cap, meaning this manager may use the whole volume", c.CapBytes)
	}
	if c.WarningFreeBytes < 0 {
		v.addf("capacity.warning_free_bytes: must not be negative (got %d); use 0 for no warning level", c.WarningFreeBytes)
	}
	if c.CriticalFreeBytes < 0 {
		v.addf("capacity.critical_free_bytes: must not be negative (got %d); use 0 for no critical level", c.CriticalFreeBytes)
	}
	if c.SafetyMarginBytes < 0 {
		v.addf("capacity.safety_margin_bytes: must not be negative (got %d)", c.SafetyMarginBytes)
	}

	if c.WarningFreeBytes >= 0 && c.CriticalFreeBytes >= 0 && c.WarningFreeBytes < c.CriticalFreeBytes {
		v.addf(
			"capacity.warning_free_bytes (%d) must be at or above capacity.critical_free_bytes (%d): free space crosses the warning line first as it drops, so the pair cannot be honoured the other way round; set both or neither",
			c.WarningFreeBytes, c.CriticalFreeBytes,
		)
	}

	if c.CapBytes > 0 && c.CriticalFreeBytes > 0 && c.CapBytes <= c.CriticalFreeBytes {
		v.addf(
			"capacity.cap_bytes (%d) must be above capacity.critical_free_bytes (%d): a cap at or below the critical floor leaves no headroom any transfer could ever be admitted into",
			c.CapBytes, c.CriticalFreeBytes,
		)
	}

	if c.BackupRoot != "" {
		if err := validAbsolutePath(c.BackupRoot); err != nil {
			v.addf("capacity.backup_root %v", err)
		}
	}
}

func (v *validator) validateBackupSet(path, sourceName string, sourceReadOnly bool, bs *BackupSet, seenSetIDs map[string]string) {
	if bs.Name == "" {
		v.addf("%s: id must not be empty", path)
	}

	// Issue #282: resolve the fully-answered ReadOnly from this set's own
	// override, falling back to the parent source's default. Nothing here
	// can be wrong the way most other fields in this function can: every
	// bool is a meaningful, acceptable answer, so this is resolution, not
	// validation, exactly like ID just below it.
	if bs.ReadOnlyConfig != nil {
		bs.ReadOnly = *bs.ReadOnlyConfig
	} else {
		bs.ReadOnly = sourceReadOnly
	}

	// FR-7: the identity is source-plus-set, and it goes through
	// model.NewBackupSetID rather than string concatenation so the same
	// validation the model package tests (no separator, no whitespace, no
	// control characters in either half) applies here too, instead of being
	// re-implemented, and possibly reimplemented incompletely, in this
	// package.
	id, err := model.NewBackupSetID(sourceName, bs.Name)
	if err != nil {
		v.addf("%s: %v", path, err)
	} else {
		bs.ID = id
		if prev, dup := seenSetIDs[id.String()]; dup {
			v.addf("%s: backup set id %q is already used by %s", path, id, prev)
		} else {
			seenSetIDs[id.String()] = path
		}
	}

	v.validateRemote(path+".remote", &bs.Remote)

	if bs.RemotePath == "" {
		v.addf("%s: remote_path must not be empty", path)
	} else if err := validAbsolutePath(bs.RemotePath); err != nil {
		v.addf("%s: remote_path %v", path, err)
	}

	if bs.LocalPath == "" {
		v.addf("%s: local_path must not be empty", path)
	} else if err := validAbsolutePath(bs.LocalPath); err != nil {
		v.addf("%s: local_path %v", path, err)
	}

	for k, pat := range bs.Include {
		incPath := fmt.Sprintf("%s.include[%d]", path, k)
		switch {
		case pat == "":
			v.addf("%s: must not be empty", incPath)
		case strings.ContainsAny(pat, `/\`):
			// Include patterns match artifact basenames (see
			// model.ArtifactID, which refuses anything else), never paths:
			// remote filenames are untrusted (FR-8), and a pattern that
			// could itself contain a path separator invites exactly the
			// kind of traversal ArtifactID is built to reject downstream.
			v.addf("%s: %q must be a filename pattern, not a path", incPath, pat)
		default:
			if _, err := filepath.Match(pat, ""); err != nil {
				v.addf("%s: %q is not a valid pattern: %v", incPath, pat, err)
			}
		}
	}

	v.validateCompletion(path+".completion", &bs.Completion, bs.Include)

	// A stale_after that parses to the zero Duration must not be read as
	// "age >= 0 is always true, so every backup is stale": that would make
	// every backup set report STALE the instant it starts, which is a false
	// alarm at best and, if anything downstream ever reacts to STALE
	// automatically, a wrong one. There is no default duration documented
	// anywhere for this field, so rather than guess one, a missing or zero
	// stale_after is refused outright.
	if bs.StaleAfter.Duration() <= 0 {
		v.addf("%s: stale_after must be set to a positive duration (got %s)", path, bs.StaleAfter)
	}

	v.validateValidation(path+".validation", &bs.Validation)
	v.validateRevalidation(path+".revalidation", &bs.Revalidation)
}

func (v *validator) validateRemote(path string, r *Remote) {
	switch r.Type {
	case "sftp":
		if r.Host == "" {
			v.addf("%s: host is required for type \"sftp\"", path)
		}
		if r.User == "" {
			v.addf("%s: user is required for type \"sftp\"", path)
		}
		v.validateKey(path, r)
		if r.KnownHosts == "" {
			v.addf("%s: known_hosts is required for type \"sftp\"", path)
		} else if strings.EqualFold(strings.TrimSpace(r.KnownHosts), "none") {
			// rclone treats the literal value "none" as an explicit request
			// to disable host-key checking. See ssh.go's package comment
			// for the full trace; the same value is refused here so the
			// mistake is caught at config time.
			v.addf("%s: known_hosts value %q disables host-key verification, which is not allowed (FR-6)", path, r.KnownHosts)
		}
		if r.Port < 0 || r.Port > 65535 {
			v.addf("%s: port %d is out of range (0 selects the default port)", path, r.Port)
		}
	case "local":
		if r.Host != "" || r.User != "" || r.KeyFile != "" || !r.Key.isZero() || r.KnownHosts != "" || r.Port != 0 {
			v.addf("%s: host, port, user, key_file/key and known_hosts are not used for type \"local\"; remove them", path)
		}
	case "":
		v.addf("%s: type must be set (\"local\" or \"sftp\")", path)
	default:
		v.addf("%s: unsupported type %q; this build only registers \"local\" and \"sftp\" (FR-4)", path, r.Type)
	}
}

// validateKey checks Remote's key configuration for an sftp remote and, if
// it is valid, normalizes the deprecated KeyFile alias and the new Key.File
// field to agree with each other.
//
// #74's shape has three sources (Key.File, Key.Env, Key.Command) plus one
// deprecated alias for the first of those (Remote.KeyFile). KeyFile and
// Key.File are two spellings of the same "file" source, not two
// independent ones (see the Key type's doc), so they only count as one
// source below, and only conflict with each other when set to two
// different values; that keeps this function safe to call more than once
// with the same Remote, exactly like Validate itself (see
// TestValidateIsIdempotent), since after the first call normalizes them to
// agree, a second call sees them equal rather than newly "both set".
//
// Across the three real sources, exactly one may be set, never zero
// (mirrored again, independently, in transport/rclone/ssh.go, so a config
// mistake is reported here, before the manager ever tries to connect, not
// on the first attempt to reach the remote) and never more than one: two
// configured sources is a config error to fix, not a precedence order for
// this package or ssh.go to silently pick through.
//
// Once exactly one source is confirmed, and only then, this copies
// whichever of KeyFile/Key.File is non-empty into the other, so every
// reader downstream of Validate sees both agree regardless of which one
// the operator actually wrote. That matters in particular for
// internal/app's config.Remote -> transport.Source translation, which
// today only forwards r.KeyFile: without this, a config written using the
// new key.file: block would pass Validate but silently fail to connect,
// exactly the kind of protection-dies-quietly gap this project exists to
// avoid.
func (v *validator) validateKey(path string, r *Remote) {
	if r.KeyFile != "" && r.Key.File != "" && r.KeyFile != r.Key.File {
		v.addf("%s: key_file and key.file are both set, to different values; set only one (key_file is a deprecated alias for key.file)", path)
		return
	}
	fileValue := r.KeyFile
	if fileValue == "" {
		fileValue = r.Key.File
	}

	sources := 0
	if fileValue != "" {
		sources++
	}
	if r.Key.Env != "" {
		sources++
	}
	if len(r.Key.Command) != 0 {
		sources++
	}

	switch {
	case sources == 0:
		v.addf("%s: key_file, key.file, key.env or key.command is required for type \"sftp\" (key-based authentication is mandatory, ssh-agent fallback and password login are not offered)", path)
		return
	case sources > 1:
		v.addf("%s: exactly one of key_file (deprecated)/key.file, key.env or key.command may be set, not more than one", path)
		return
	}

	if len(r.Key.Command) != 0 {
		cmdPath := path + ".key.command"
		if r.Key.Command[0] == "" {
			v.addf("%s: the first element (the executable) must not be empty", cmdPath)
		} else if !filepath.IsAbs(r.Key.Command[0]) {
			// Same reasoning as validateValidation's identical rule on
			// Validation.Command: a key resolver has to resolve to exactly
			// one binary regardless of the process's working directory or
			// $PATH at the moment a connection happens to be made, which
			// matters even more here than for an application validator,
			// since this is what authentication itself depends on.
			v.addf("%s: executable %q must be an absolute path", cmdPath, r.Key.Command[0])
		}
	}

	if fileValue != "" {
		r.KeyFile = fileValue
		r.Key.File = fileValue
	}
}

func (v *validator) validateCompletion(path string, c *Completion, include []string) {
	switch c.Strategy {
	case "stable":
		if c.StableFor.Duration() <= 0 {
			v.addf("%s: stable_for must be set to a positive duration when strategy is \"stable\"", path)
		}
		// WP3.2: "stable" is a heuristic, not a producer completion
		// signal, so FR-15's remote-delete gate needs an extra
		// deletion-safety delay before it treats a stable artifact as
		// producer-confirmed. See Completion.DeleteSafetyDelay's own doc
		// for why this is a distinct field from stable_for rather than a
		// second use of it.
		//
		// Unlike stable_for just above, an omitted or zero value is
		// resolved to DefaultDeleteSafetyDelay rather than refused. The
		// key is new in WP3.2, so every config file written against an
		// earlier release omits it; refusing those would stop the daemon
		// loading a config that was valid the day before an upgrade, and
		// would reject every stable-strategy backup set the API layer
		// builds (service.CreateBackupSet has no field for this key), for
		// a value the project has a documented safe default for. Reading
		// the omission literally as "no delay required" is the other
		// wrong answer: it would silently disable the gate on exactly the
		// deployments that never had the chance to opt in. Only a
		// negative duration, which can only ever be something the
		// operator typed on purpose and which would make the gate a
		// no-op, is refused.
		switch {
		case c.DeleteSafetyDelay.Duration() < 0:
			v.addf("%s: delete_safety_delay must not be negative (got %s); omit it to use the default of %s", path, c.DeleteSafetyDelay, DefaultDeleteSafetyDelay)
		case c.DeleteSafetyDelay.Duration() == 0:
			c.DeleteSafetyDelay = Duration(DefaultDeleteSafetyDelay)
		}
		if c.ManifestMarker != "" {
			v.addf("%s: manifest_marker is not used by strategy %q; remove it", path, c.Strategy)
		}
	case "marker":
		if c.StableFor.Duration() != 0 {
			v.addf("%s: stable_for is not used by strategy %q; remove it", path, c.Strategy)
		}
		if c.DeleteSafetyDelay.Duration() != 0 {
			v.addf("%s: delete_safety_delay is not used by strategy %q; remove it", path, c.Strategy)
		}
		v.validateManifestMarker(path, c, include)
	case "rename":
		if c.StableFor.Duration() != 0 {
			v.addf("%s: stable_for is not used by strategy %q; remove it", path, c.Strategy)
		}
		if c.DeleteSafetyDelay.Duration() != 0 {
			v.addf("%s: delete_safety_delay is not used by strategy %q; remove it", path, c.Strategy)
		}
		if c.ManifestMarker != "" {
			v.addf("%s: manifest_marker is not used by strategy %q; remove it", path, c.Strategy)
		}
	case "":
		v.addf("%s: strategy must be set (\"rename\", \"marker\" or \"stable\", FR-8)", path)
	default:
		v.addf("%s: unsupported strategy %q; must be \"rename\", \"marker\" or \"stable\" (FR-8)", path, c.Strategy)
	}
}

// validateManifestMarker checks c.ManifestMarker's shape, resolves it to
// DefaultManifestMarker if it is unset (issue #291), and then cross-checks
// the resolved name against this backup set's own include patterns. Only
// called when c.Strategy == "marker".
//
// The shape checks mirror validateBackupSet's Include pattern validation on
// purpose: an operator-supplied filename read back off a remote listing is
// exactly the kind of untrusted-adjacent string include patterns already
// guard (no path separator, since this is a basename never a path; no "."
// or ".." segment, since those are directory references, not filenames,
// and model.ArtifactID would refuse them downstream anyway). It is
// deliberately not run through filepath.Match the way an include pattern
// is: ManifestMarker is a single literal name, matched by exact string
// comparison in internal/discovery/complete.go, never a glob, so a
// character that would be an invalid glob metachar (say, an unmatched "[")
// is still a perfectly valid literal filename here.
//
// include is this backup set's Include, threaded down from
// validateBackupSet through validateCompletion rather than read off a
// shared struct, since Completion, unlike BackupSet, has no field for it.
//
// The collision check is a safety & reliability finding, not a shape one:
// before ManifestMarker was operator-configurable it was always the fixed
// literal "_SUCCESS", implicitly never a real payload name. Now that an
// operator picks it, nothing stops them picking a name their own Include
// patterns would otherwise match. discovery.Discover's isMarkerObject
// check runs before Include filtering and unconditionally skips a match
// (see discovery.go's Discover), so an undetected collision here would
// silently and permanently exclude a real artifact from every backup,
// forever, with no error, no rejection entry, no warning. Matching uses
// filepath.Match, the same as the Include pattern shape check just above
// this function's call site: both operands are already guaranteed
// separator-free basenames by this point, so it agrees with
// internal/discovery/complete.go's includeMatches (which uses path.Match
// for GOOS-independence at actual matching time) on every input that
// reaches here. Only patterns actually configured are checked: an empty
// Include list has nothing to cross-check against, which is the same "no
// explicit filter" baseline that let "_SUCCESS" work unconditionally
// before this field existed.
func (v *validator) validateManifestMarker(path string, c *Completion, include []string) {
	markerPath := path + ".manifest_marker"
	switch {
	case c.ManifestMarker == "":
		c.ManifestMarker = DefaultManifestMarker
	case strings.ContainsAny(c.ManifestMarker, `/\`):
		// Mirrors the Include pattern message: this matches a remote
		// directory's basename, never a path, so a value that could
		// itself contain a path separator invites exactly the kind of
		// traversal ArtifactID is built to reject downstream.
		v.addf("%s: %q must be a filename, not a path", markerPath, c.ManifestMarker)
		return
	case c.ManifestMarker == "." || c.ManifestMarker == "..":
		v.addf("%s: %q must not be a directory reference", markerPath, c.ManifestMarker)
		return
	}

	for _, pat := range include {
		if ok, err := filepath.Match(pat, c.ManifestMarker); err == nil && ok {
			v.addf("%s: %q matches this backup set's own include pattern %q; a real artifact with that name would be silently and permanently excluded from every backup, since discovery treats a manifest-marker match as a completion signal, never a candidate", markerPath, c.ManifestMarker, pat)
			break
		}
	}
}

func (v *validator) validateValidation(path string, val *Validation) {
	switch val.Hash {
	case "", "sha256":
	default:
		v.addf("%s: unsupported hash %q; must be empty or \"sha256\"", path, val.Hash)
	}

	if val.ValidatorID != "" {
		idPath := path + ".validator_id"
		if val.Command != nil {
			// Two different answers to "which validator runs here". There
			// is no sensible precedence to invent: a config that says both
			// is a mistake, and picking one silently would run something
			// the operator did not intend.
			v.addf("%s: must not be set alongside %s.command; a backup set names its validator one way or the other", idPath, path)
		}
		if err := validValidatorID(val.ValidatorID); err != nil {
			v.addf("%s: %v", idPath, err)
		}
	}

	if val.Command == nil {
		return
	}
	cmdPath := path + ".command"
	if val.Command.Executable == "" {
		v.addf("%s: executable must not be empty", cmdPath)
	} else if !filepath.IsAbs(val.Command.Executable) {
		// Required so the validator that runs against a candidate restore
		// point resolves to exactly one binary regardless of the process's
		// working directory or $PATH at the moment it happens to run,
		// rather than whatever a relative name resolves to.
		v.addf("%s: executable %q must be an absolute path", cmdPath, val.Command.Executable)
	}
	// A required validator with no timeout can hang the lifecycle it's
	// gating on forever; there's no safe default duration to guess here
	// either, so this is required whenever a command is configured at all.
	if val.Command.Timeout.Duration() <= 0 {
		v.addf("%s: timeout must be set to a positive duration", cmdPath)
	}
}

// validateRevalidation checks Phase 4's scheduled-revalidation block.
//
// Revalidation is opt-in: the zero value (Hash false, Command nil) means
// disabled, and is accepted with nothing further to check, exactly the
// same "no key present, nothing enabled" shape validateValidation already
// gives a nil Command. Once either Hash or Command turns it on, Interval
// and MaxPerCycle both become required with no default: there is no
// universally safe cadence or batch size to guess for re-reading, and
// potentially re-hashing, an unknown amount of already-verified data on a
// NAS, so an unset value is refused rather than silently treated as
// "revalidate nothing" (zero) or "revalidate everything, every cycle"
// (unbounded), either of which would be a guess this package has no basis
// for making.
func (v *validator) validateRevalidation(path string, r *Revalidation) {
	enabled := r.Hash || r.Command != nil

	if !enabled {
		if r.Interval != 0 || r.MaxPerCycle != 0 {
			v.addf("%s: interval and max_per_cycle are only used when hash or command is set; remove them", path)
		}
		return
	}

	if r.Interval.Duration() <= 0 {
		v.addf("%s: interval must be set to a positive duration when hash or command is configured", path)
	}
	if r.MaxPerCycle <= 0 {
		v.addf("%s: max_per_cycle must be a positive integer when hash or command is configured", path)
	}

	if r.Command == nil {
		return
	}
	cmdPath := path + ".command"
	if r.Command.Executable == "" {
		v.addf("%s: executable must not be empty", cmdPath)
	} else if !filepath.IsAbs(r.Command.Executable) {
		// Mirrors validateValidation's identical rule on Validation.Command:
		// a restore-test hook has to resolve to exactly one binary
		// regardless of the process's working directory or $PATH at the
		// moment a scheduled pass happens to run it.
		v.addf("%s: executable %q must be an absolute path", cmdPath, r.Command.Executable)
	}
	if r.Command.Timeout.Duration() <= 0 {
		v.addf("%s: timeout must be set to a positive duration", cmdPath)
	}
}

// validValidatorID checks a validator_id's shape, and only its shape.
// Whether the id is actually registered is core/service's question (this
// package cannot see the catalog); what this rules out is a value that
// was never an id in the first place -- a path, a traversal, a command
// line -- so a config that tried to smuggle an executable in through this
// key is refused here rather than deeper in, with a message naming the
// key an operator actually wrote.
//
// This is belt and braces: core/service's resolver only ever returns a
// catalog entry it built itself, so an id shaped like a path resolves to
// nothing regardless. The value of checking here is the error message and
// the fact that it happens before any destructive processing begins,
// which is what this package exists for.
func validValidatorID(id string) error {
	switch {
	case strings.TrimSpace(id) != id:
		return fmt.Errorf("%q must not have leading or trailing whitespace", id)
	case strings.ContainsAny(id, "/\\\x00\n\r \t"):
		return fmt.Errorf("%q must be a bare identifier: no path separator, whitespace or control character", id)
	case id == "." || id == "..":
		return fmt.Errorf("%q must not be a directory reference", id)
	}
	return nil
}

func (v *validator) validateState(s State) {
	if s.Database == "" {
		v.addf("state.database: must not be empty")
		return
	}
	if err := validAbsolutePath(s.Database); err != nil {
		v.addf("state.database: %v", err)
	}
}

var validWeekdays = map[string]bool{
	"monday":    true,
	"tuesday":   true,
	"wednesday": true,
	"thursday":  true,
	"friday":    true,
	"saturday":  true,
	"sunday":    true,
}

// ValidateRetention validates and resolves-in-place every FR-18/FR-19
// retention field (timezone, week_starts_on, the tiers chain or the
// daily_days/weekly_months/monthly_months scalars it is the general form
// of, and protect_last_known_good), exactly as Validate does for
// a whole Config's embedded Retention block: it is not a second
// implementation of that logic, it is the same validateRetention method
// this file's Validate already calls, exported so a caller outside this
// package can run a candidate Retention value through the identical
// checks and defaults the config file itself goes through.
//
// This exists for issue #111 (B3.6): the CLI's retention override flags
// and any future UI-backing settings surface both need "a value set here
// means exactly what the same value written into the YAML file means,"
// including the same error text for the same mistake. Reimplementing
// "is this timezone loadable" or "is this tier negative" a second time in
// either of those places would be exactly the kind of second, potentially
// divergent, validation path this function exists to make unnecessary.
//
// Like Validate itself, this is safe to call more than once on the same
// Retention: every default it fills in is only applied when the field is
// still at its zero value, so a value already resolved by a previous call
// (or by the YAML file's own Validate pass) is left alone.
func ValidateRetention(r *Retention) error {
	v := &validator{}
	v.validateRetention(r)
	return v.err()
}

func (v *validator) validateRetention(r *Retention) {
	if r.Timezone == "" {
		r.Timezone = "UTC"
	}
	if _, err := time.LoadLocation(r.Timezone); err != nil {
		v.addf("retention.timezone: %q is not a loadable IANA timezone: %v", r.Timezone, err)
	}

	if r.WeekStartsOn == "" {
		r.WeekStartsOn = "monday"
	} else {
		r.WeekStartsOn = strings.ToLower(r.WeekStartsOn)
	}
	if !validWeekdays[r.WeekStartsOn] {
		v.addf("retention.week_starts_on: %q is not a day of the week", r.WeekStartsOn)
	}

	// FR-18's chain has two spellings, and validateRetention picks exactly
	// one of them per call rather than blending the two.
	//
	// An explicit tiers list is the general form: any number of named
	// tiers at any granularity. The three daily_days/weekly_months/
	// monthly_months scalars are sugar for the specific three-tier chain
	// config.DefaultTierChain builds, kept because a config file written
	// before the chain existed has to keep deciding exactly as it always
	// has (issue #156's own acceptance criterion).
	//
	// Writing both is refused rather than resolved by precedence. An
	// operator who sets daily_days alongside a tiers list is describing
	// two different policies, and silently keeping one of them is how a
	// retention pass ends up deleting on terms nobody wrote. Refusing also
	// keeps this function idempotent, which Validate's own doc promises
	// and the CLI's override path depends on: because the scalars are only
	// defaulted when no tiers list is present, a second call over an
	// already-resolved Retention finds the same branch and changes
	// nothing.
	if len(r.Tiers) > 0 {
		v.validateRetentionTiers(r)
	} else {
		// A tier left at zero, whether because the key was omitted or
		// written as 0, falls back to FR-18's documented default rather
		// than being read literally: a literal zero would mean "keep none
		// of this tier", and a retention pass that deletes an entire tier
		// because the operator left a key out of the YAML file is a
		// data-loss bug, not a permissive default. An operator who
		// explicitly wants a tighter policy sets a positive number smaller
		// than the default; there is no way to spell "disable this tier"
		// in this spelling, by design. The explicit tiers list is the way
		// to run fewer than three tiers.
		if r.DailyDays == 0 {
			r.DailyDays = DefaultDailyDays
		} else if r.DailyDays < 0 {
			v.addf("retention.daily_days: must not be negative (got %d)", r.DailyDays)
		}
		if r.WeeklyMonths == 0 {
			r.WeeklyMonths = DefaultWeeklyMonths
		} else if r.WeeklyMonths < 0 {
			v.addf("retention.weekly_months: must not be negative (got %d)", r.WeeklyMonths)
		}
		if r.MonthlyMonths == 0 {
			r.MonthlyMonths = DefaultMonthlyMonths
		} else if r.MonthlyMonths < 0 {
			v.addf("retention.monthly_months: must not be negative (got %d)", r.MonthlyMonths)
		}
	}

	// FR-19: the newest known-good restore point must never be deleted
	// solely because of its age. ProtectLastKnownGood is a *bool rather
	// than a bool specifically so "the key was left out of the YAML file"
	// and "the operator explicitly wrote false" are distinguishable inputs.
	// A plain bool can't tell them apart, and its zero value is false, so a
	// config that simply omits this key would silently turn the protection
	// off. Only the "absent" case is defaulted, and it defaults to the safe
	// reading; an explicit false is left as the operator wrote it.
	if r.ProtectLastKnownGood == nil {
		protect := true
		r.ProtectLastKnownGood = &protect
	}
}

// validRetentionGranularities is the closed set RetentionTier.Granularity
// accepts. It is a map rather than a switch so a settings form (B3.7) has
// something to enumerate, and so the error message below can list every
// legal value instead of only naming the illegal one.
var validRetentionGranularities = map[string]bool{
	GranularityDay:      true,
	GranularityWeek:     true,
	GranularityMonth:    true,
	GranularityQuarter:  true,
	GranularityHalfYear: true,
	GranularityYear:     true,
	GranularityDays:     true,
}

// retentionGranularities is the same closed set in a fixed order, so the
// same mistake always produces the same error text (a map range would
// reorder it per run and make the message untestable) and so a caller
// outside this package can enumerate it (RetentionGranularities below).
var retentionGranularities = []string{
	GranularityDay, GranularityWeek, GranularityMonth,
	GranularityQuarter, GranularityHalfYear, GranularityYear, GranularityDays,
}

// retentionGranularityList renders retentionGranularities for an error
// message.
var retentionGranularityList = strings.Join(retentionGranularities, ", ")

// RetentionGranularities returns every value RetentionTier.Granularity
// accepts, in the fixed order above, as a fresh slice the caller may keep
// or sort without moving this package's own copy.
//
// This exists for issue #140 (B3.7): the settings form renders the
// granularity picker from this list, so the closed set a client validates
// against client-side is the one validateRetentionTiers actually enforces
// server-side, rather than a second list transcribed by hand into a
// frontend and free to drift. RetentionTier's own doc already anticipates
// exactly that ("can validate every field client-side against the same
// closed value sets config.Validate checks server-side").
func RetentionGranularities() []string {
	return append([]string(nil), retentionGranularities...)
}

// RetentionWindowUnits returns every value RetentionTier.WindowUnit
// accepts: RetentionGranularities minus GranularityDays, which a window
// can never be measured in (see WindowUnit's own doc and
// validateRetentionTiers' refusal of it).
func RetentionWindowUnits() []string {
	units := make([]string, 0, len(retentionGranularities)-1)
	for _, g := range retentionGranularities {
		if g == GranularityDays {
			continue
		}
		units = append(units, g)
	}
	return units
}

// retentionTierNamePattern constrains a tier name to lower_snake_case.
//
// The constraint exists because the name is not decoration: internal/
// retention upper-cases it into the tier string apps/common/webhost sends
// to the client, so an unconstrained name would put arbitrary text on the
// wire and make "daily" report as something other than DAILY depending on
// how the operator capitalised it. One canonical spelling per tier also
// means a settings form can validate the field client-side against the
// same rule this function applies.
//
// The source string is exported as RetentionTierNamePattern so a client
// can apply the identical rule before submitting, for the same
// single-source reason as RetentionGranularities above.
var retentionTierNamePattern = regexp.MustCompile(RetentionTierNamePattern)

// RetentionTierNamePattern is the regular expression source
// retentionTierNamePattern is compiled from, in a syntax
// (RE2/JavaScript-compatible: an anchored character-class repetition,
// nothing Go-specific) both this package and a browser accept.
const RetentionTierNamePattern = `^[a-z][a-z0-9_]*$`

// retentionTierKeepMax and retentionTierPeriodDaysMax bound the two
// numbers in a tier from above.
//
// Both are already bounded from below, and both feed calendar arithmetic
// that walks a window backwards from today. A large enough value wraps
// time.Date's own int64 second arithmetic, the window's start lands after
// today, and the tier selects nothing at all with no error reported: the
// same silent empty selection every other rule in this function exists to
// refuse, reached from a config file Validate would otherwise accept. The
// wrap needs roughly 1e11, so no operator is going to type it by
// accident; the point of the ceiling is that an input whose out-of-range
// behaviour is "quietly keep nothing" has no business being unbounded in
// the last check between a YAML file and a deletion plan.
//
// The two numbers are arbitrary, chosen to be far past any policy anyone
// would write: 10,000 look-back units is 27 years of dailies or a hundred
// centuries of annuals, and 3,650 days is a ten-year custom period.
//
// Exported as RetentionTierKeepMax/RetentionTierPeriodDaysMax so a
// settings form can refuse an out-of-range number before it is ever
// submitted, against these values rather than a copy (issue #140).
const (
	retentionTierKeepMax       = RetentionTierKeepMax
	retentionTierPeriodDaysMax = RetentionTierPeriodDaysMax
)

// RetentionTierKeepMax and RetentionTierPeriodDaysMax are the exported
// spellings of the two ceilings documented above.
const (
	RetentionTierKeepMax       = 10000
	RetentionTierPeriodDaysMax = 3650
)

// validateRetentionTiers checks an explicit FR-18 chain. It is only
// reached when r.Tiers is non-empty; see validateRetention for why the two
// spellings are mutually exclusive.
func (v *validator) validateRetentionTiers(r *Retention) {
	if r.DailyDays != 0 {
		v.addf("retention.daily_days: cannot be combined with retention.tiers; daily_days is sugar for the default tiers chain, so write one or the other")
	}
	if r.WeeklyMonths != 0 {
		v.addf("retention.weekly_months: cannot be combined with retention.tiers; weekly_months is sugar for the default tiers chain, so write one or the other")
	}
	if r.MonthlyMonths != 0 {
		v.addf("retention.monthly_months: cannot be combined with retention.tiers; monthly_months is sugar for the default tiers chain, so write one or the other")
	}

	seen := map[string]int{} // tier name -> the index that first claimed it
	for i := range r.Tiers {
		path := fmt.Sprintf("retention.tiers[%d]", i)
		t := &r.Tiers[i]

		switch {
		case t.Name == "":
			v.addf("%s: name must not be empty", path)
		case !retentionTierNamePattern.MatchString(t.Name):
			v.addf("%s: name %q must be lower_snake_case (letters, digits and underscores, starting with a letter)", path, t.Name)
		case t.Name == TierLastKnownGoodName:
			v.addf("%s: name %q is reserved for FR-19's last-known-good protection and cannot name a retention tier", path, t.Name)
		default:
			if first, dup := seen[t.Name]; dup {
				v.addf("%s: duplicate tier name %q (already used by retention.tiers[%d])", path, t.Name, first)
			}
			seen[t.Name] = i
		}

		if !validRetentionGranularities[t.Granularity] {
			if t.Granularity == "" {
				v.addf("%s: granularity must be set to one of: %s", path, retentionGranularityList)
			} else {
				v.addf("%s: granularity %q is not one of: %s", path, t.Granularity, retentionGranularityList)
			}
		}

		// period_days is required by, and only meaningful to, the custom
		// granularity. Refusing a stray value rather than ignoring it is
		// the same call config.Load's KnownFields(true) already makes for
		// a mistyped key: a number the operator wrote and this code
		// silently drops is a policy they think they configured.
		if t.Granularity == GranularityDays {
			switch {
			case t.PeriodDays <= 0:
				v.addf("%s: granularity %q needs a positive period_days (got %d)", path, GranularityDays, t.PeriodDays)
			case t.PeriodDays > retentionTierPeriodDaysMax:
				v.addf("%s: period_days must not exceed %d (got %d); a longer period overflows the calendar arithmetic that walks this tier's window back from today, and a tier that overflows selects nothing at all", path, retentionTierPeriodDaysMax, t.PeriodDays)
			}
		} else if t.PeriodDays != 0 {
			v.addf("%s: period_days is only meaningful with granularity %q (got %d alongside granularity %q)", path, GranularityDays, t.PeriodDays, t.Granularity)
		}

		// Unlike the legacy scalars, an explicit tier has no "zero means
		// the default" reading available: the operator listed this tier
		// deliberately, so a zero window is a mistake, and the way to run
		// without a tier is to leave it out of the chain.
		switch {
		case t.Keep <= 0:
			// The advice has to carry the fallback with it. "Leave this
			// tier out" is right for one tier and wrong for all of them:
			// an operator who follows it down to the last tier arrives at
			// an empty list, which reads as an absent key and reinstates
			// the default chain rather than the narrow policy they were
			// writing (see Retention.Tiers' own doc).
			v.addf("%s: keep must be a positive number of look-back units (got %d); drop this one tier from the chain rather than writing it with a zero window, and note that emptying retention.tiers entirely falls back to the default daily/weekly/monthly policy instead of keeping nothing", path, t.Keep)
		case t.Keep > retentionTierKeepMax:
			v.addf("%s: keep must not exceed %d look-back units (got %d); a longer window overflows the calendar arithmetic that walks it back from today, and a tier that overflows selects nothing at all", path, retentionTierKeepMax, t.Keep)
		}

		switch {
		case t.WindowUnit == "":
			// Defaults to the tier's own granularity.
		case t.WindowUnit == GranularityDays:
			v.addf("%s: window_unit cannot be %q; a custom period only measures a window when it is the tier's own granularity, in which case leave window_unit unset", path, GranularityDays)
		case !validRetentionGranularities[t.WindowUnit]:
			v.addf("%s: window_unit %q is not one of: %s", path, t.WindowUnit, retentionGranularityList)
		}
	}
}

// validAbsolutePath rejects anything that is not an absolute, traversal-free
// path.
//
// This is deliberately stricter than the leeway ssh.go's sftpConfig gives
// key_file and known_hosts, which are passed through env.ShellExpand and so
// may legitimately start with "~" or an environment variable reference.
// This package does not import anything from rclone (only transport/rclone
// may), so it cannot replicate that exact expansion without risking a
// second, subtly different implementation of it; key_file and known_hosts
// are therefore only checked here for being non-empty and traversal-free,
// and are left to transport to resolve and confirm exist. local_path,
// remote_path and state.database have no documented expansion syntax
// anywhere in this codebase, and get to be held to the stricter standard:
// they define the managed backup root and the lifecycle journal location,
// so a path that resolves differently depending on the process's working
// directory is exactly the kind of ambiguity FR-20's "prove it is beneath
// the configured backup-set root" is guarding against.
func validAbsolutePath(p string) error {
	if !filepath.IsAbs(p) {
		return fmt.Errorf("%q must be an absolute path", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("%q must not contain \"..\"", p)
		}
	}
	return nil
}

// validator accumulates every problem Validate finds instead of stopping at
// the first one.
type validator struct {
	problems []error
}

func (v *validator) addf(format string, args ...any) {
	v.problems = append(v.problems, fmt.Errorf(format, args...))
}

func (v *validator) err() error {
	if len(v.problems) == 0 {
		return nil
	}
	return &ValidationError{Problems: v.problems}
}

// ValidationError is what Validate returns when the config has one or more
// problems. It carries every problem found in a single Validate call, not
// just the first, so fixing a config doesn't take one restart per mistake.
type ValidationError struct {
	Problems []error
}

func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return fmt.Sprintf("invalid config: %v", e.Problems[0])
	}
	var b strings.Builder
	fmt.Fprintf(&b, "invalid config (%d problems):", len(e.Problems))
	for _, p := range e.Problems {
		fmt.Fprintf(&b, "\n  - %v", p)
	}
	return b.String()
}

// Unwrap lets errors.Is and errors.As reach into any individual problem.
func (e *ValidationError) Unwrap() []error { return e.Problems }
