package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/apiclient"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/service"
)

// Issue #544, Phase 2 of #536: the read commands get the mode mode.go
// deliberately left them without, and a read beside a live engine answers
// about the world that engine holds or does not answer at all.
//
// # What #535 looked like from this side
//
// `sources` listed two backup sets and the Web UI showed none. Neither
// surface was wrong about the configuration it had: the engine read
// config.yaml when it started and has no watcher on it, and the CLI read
// whatever was on disk a moment ago. What was missing was anything at all
// comparing the two, so both answered confidently and one of them was
// about a deployment nobody was running.
//
// mode.go closed the writing half of that. This is the reading half, and
// it is the half an operator actually looks at: a write that lands
// somewhere unexpected is found later, by reading.
//
// # Reads take no claim, and that is a decision
//
// enterConfigWriteMode decides inside service.BeginConfigWrite, because a
// write has to still be the only authority when the bytes land. A read
// changes nothing, so there is nothing for a claim to protect, and taking
// one would cost something real: BeginConfigWrite takes the same
// `.startup-lock` every engine start has to take, so a `status` left in a
// terminal would block a container from starting for as long as it ran.
// core/service's own ConfigWriteGuard doc names that cost and accepts it
// for a write; paying it for a read would buy nothing.
//
// What reads DO share with writes is the shape: the question is asked
// exactly once per invocation, the answer is carried rather than
// re-derived, and it is said out loud. The detector is the same package
// variable mode.go uses, so mode_test.go's probe counter watches both.
//
// # What "routed through the engine" means here, and what it cannot mean
//
// The obvious reading is that each command re-renders its output from the
// engine's own response. That is not available, and the reason is worth
// writing down rather than discovering later: every one of these four
// surfaces prints something api/v1/openapi.json cannot express.
//
//	sources     BackupSet carries no remote type and no stale_after, and
//	            the CLI prints both on every line.
//	artifacts   the detail view's `reason`/`reason_at` is the literal
//	            sentence internal/lifecycle recorded on the transition
//	            that quarantined an artifact (issue #284). The schema has
//	            no such field, deliberately: it is this command's whole
//	            reason for existing.
//	status      the report is ages, and the response carries timestamps.
//	            Two processes rendering an age from one timestamp differ
//	            by however long the round trip took. The #418
//	            "retained outside every backup set" section has no
//	            endpoint at all.
//	retention   sibling collisions (issue #292) and the per-set
//	            last-known-good sentence are not on the wire.
//
// A route that re-rendered from the wire would therefore print LESS than
// the same command prints today, and it would print less exactly when an
// engine is up, which is when an operator is most likely to be
// diagnosing something. Two surfaces showing different amounts of the
// truth is the defect this issue exists to remove, in a new place.
//
// So the engine is asked the question rather than asked to render the
// answer. GET /system/version carries the engine's own config_revision,
// which core/service derives by hashing the canonical encoding of the
// loaded configuration (service.ConfigRevisionOf): equal revisions mean
// the two processes hold content-identical configurations, across every
// field, including all four the API cannot carry. The journal is the
// other half of every answer here and it cannot diverge, because both
// processes read the same database live at request time, which is what
// #544 means by artifact state already coming from the journal.
//
// Equal revision plus one journal is a proof that the answer this command
// is about to print is the answer the engine would give. Unequal revision
// is #535, caught before anything is printed, and it is a refusal.
//
// On top of that, each command puts its own question to the engine
// through the operations #544 names -- listBackupSets, listArtifacts,
// getArtifact, previewRetention, getSystemHealth -- and compares. That is
// belt and braces on purpose: the revision proves the two configurations
// agree, and these prove the two IMPLEMENTATIONS agree about them. A
// difference either way is a refusal rather than two surfaces quietly
// telling an operator different things.
//
// # A read that cannot reach the engine
//
// It answers from the file, and it says so, every time, on stderr. It does
// not refuse, and that is the one place this file deliberately differs
// from the write path.
//
// A write that cannot reach the engine has an alternative: not writing.
// A read has none. Refusing every `status` on a host whose engine has no
// address configured would take away the command an operator runs when
// something is wrong, at the moment it is wrong, and it would take it away
// on every deployment that has not been told where its own engine is,
// which today is all of them. What it would buy is nothing an announcement
// does not already buy: the answer would still be the file's.
//
// So the third mode exists, it is named, and it says in one sentence that
// what follows was not confirmed against the process that would be
// serving it. That is the whole of "the fallback is not silent".

// The three environment variables that tell this binary where its own
// engine is.
//
// Environment rather than flags or a configuration block, for two reasons.
// A flag would be operator-visible surface on six commands' usage text at
// once, and usage text is pinned by core/tests/compat and by
// spdrman/rclone-manager-tests; the address also belongs to the HOST a
// command is run from rather than to the deployment, so a deployment's own
// config.yaml is the wrong place for it: the same file is read from inside
// the container, where the engine is on loopback, and from a NAS shell,
// where it is a published port.
//
// The password is read and handed to apiclient, which holds it in memory
// for the life of one invocation and writes it nowhere. Nothing in this
// binary prints it, and apiclient renders every address through
// url.URL.Redacted precisely so that a base URL carrying userinfo cannot
// reach a terminal (PR #546).
const (
	engineURLEnv      = "BACKUP_MANAGER_API_URL"
	engineUsernameEnv = "BACKUP_MANAGER_API_USERNAME"
	enginePasswordEnv = "BACKUP_MANAGER_API_PASSWORD"
)

// unconfirmedMode is a read that answered from the configuration file
// without having been able to check it against the process that may be
// serving this deployment.
//
// It is a third mode rather than a qualified `direct` because it is a
// different fact and an operator acts differently on it, and it is spelled
// so that it shares no prefix with either of the other two: mode.go's
// announcement is meant to be greppable, and "direct-unconfirmed" would
// make every grep for "mode: direct" match this as well.
const unconfirmedMode executionMode = "unconfirmed"

// readDecision is one read invocation's answer to "which world am I
// reporting on", plus the route to the process that holds it.
type readDecision struct {
	// mode is the decision itself.
	mode executionMode

	// engine is the process that announced itself as serving this
	// deployment, or nil. Non-nil in unconfirmedMode too, when the reason
	// this is unconfirmed is that an engine was found and could not be
	// reached: those are different sentences and the difference is the
	// engine field.
	engine *service.RunningEngine

	// client is the route to that process, and is non-nil only in
	// engineAttachedMode. Everything below that asks the engine a question
	// goes through this, so a command cannot accidentally ask in a mode
	// where there is nothing to ask.
	client *apiclient.Client

	// configFile is what this command read, resolved, because --config may
	// name the packaged configuration DIRECTORY (#196).
	configFile string

	// because is the clause the unconfirmed announcement carries.
	because string
}

// attached reports whether there is an engine to put a question to.
func (d readDecision) attached() bool { return d.mode == engineAttachedMode && d.client != nil }

// announce writes the one line that says which world this answer is about.
//
// All three go to stderr, which is where this file differs from mode.go on
// purpose. A configuration write announces `direct` on stdout because the
// line qualifies the report printed beside it. A read's stdout IS its
// answer: it is what core/tests/compat pins, what an operator pipes into
// grep, and in `artifacts`' case a table with a column layout. A
// provenance line belongs beside that, not inside it.
func (d readDecision) announce(to io.Writer) {
	// Unchecked like every other diagnostic this binary prints: a terminal
	// that went away cannot change which world this answer is about.
	switch d.mode {
	case engineAttachedMode:
		_, _ = fmt.Fprintf(to, "%s%s. Another process is serving this deployment (state database %s) and it holds the same configuration this command read, so what follows is the world its Web UI shows.\n",
			modeLinePrefix, d.mode, d.engine.StateDatabase)
	case directMode:
		_, _ = fmt.Fprintf(to, "%s%s. No process has announced itself as serving this deployment, so this command answered from %s itself.\n",
			modeLinePrefix, d.mode, d.configFile)
	case unconfirmedMode:
		_, _ = fmt.Fprintf(to, "%s%s. %s, so what follows was read from %s without being checked against that process and can disagree with what it serves.\n",
			modeLinePrefix, d.mode, d.because, d.configFile)
	default:
		_, _ = fmt.Fprintf(to, "%s%q, which this binary does not recognise; this is a bug in the reader of %s\n",
			modeLinePrefix, d.mode, d.configFile)
	}
}

// enterReadMode decides this invocation's read mode once, says which one
// it is, and hands back the route the command uses to put its own question
// to the engine.
//
// cfg is the configuration this command has already loaded, and it must be
// the validated one (config.LoadAndValidate, which both openService and
// openBackupService go through): the revision comparison is over content,
// and Validate fills defaults in place, so a revision taken before it
// would differ from the engine's for a reason that is not the
// deployment's.
//
// A returned error is a refusal: the command must print nothing and exit
// non-zero. The mode has already been announced by then, so an operator
// reading the refusal can see which world it is about.
func enterReadMode(ctx context.Context, configPath string, cfg *config.Config, announceTo io.Writer) (readDecision, error) {
	d := readDecision{configFile: config.ResolvePath(configPath)}

	engine, err := detectRunningEngine(configPath)
	if err != nil {
		// "I could not tell" is not "nothing is running", and this is the
		// one place a read is allowed to carry on anyway. The probe fails
		// for real reasons -- EACCES on a lock file owned by another uid,
		// EIO on a sick volume, ENOTSUP where flock is unavailable, and
		// the whole non-unix build -- and every one of those is a moment
		// when an operator most needs `status` to still print something.
		return d.unconfirmed(fmt.Sprintf("this host could not be asked whether any process is serving this deployment (%v)", err), announceTo), nil
	}
	if engine == nil {
		d.mode = directMode
		d.announce(announceTo)
		return d, nil
	}
	d.engine = engine

	client, err := dialEngine()
	if err != nil {
		return d.unconfirmed(fmt.Sprintf("another process is serving this deployment (state database %s) and this command has no route to it (%v)", engine.StateDatabase, err), announceTo), nil
	}

	version, err := client.SystemVersion(ctx)
	if err != nil {
		return d.unconfirmed(fmt.Sprintf("another process is serving this deployment (state database %s) and it did not answer at %s (%v)", engine.StateDatabase, client.BaseURL(), err), announceTo), nil
	}

	local := service.ConfigRevisionOf(cfg)
	if version.ConfigRevision != local {
		// Announced as unconfirmed rather than as engine-attached: the
		// engine answered, but what it answered is that this command is
		// holding a different deployment's configuration, so nothing that
		// follows would have been about the engine's world.
		d = d.unconfirmed(fmt.Sprintf("another process is serving this deployment (state database %s) and holds a different configuration", engine.StateDatabase), announceTo)
		return d, configDivergence(engine, d.configFile, local, version.ConfigRevision)
	}

	d.mode = engineAttachedMode
	d.client = client
	d.announce(announceTo)
	return d, nil
}

// unconfirmed fills in the third mode and announces it, so the four places
// above that reach it cannot set the mode without saying why.
func (d readDecision) unconfirmed(because string, announceTo io.Writer) readDecision {
	d.mode = unconfirmedMode
	d.client = nil
	d.because = because
	d.announce(announceTo)
	return d
}

// dialEngine builds the route to this deployment's engine from the
// environment, or says what is missing.
//
// It never logs, prints or returns the password, and apiclient does not
// write it anywhere either: it is held in memory for the life of one
// invocation.
func dialEngine() (*apiclient.Client, error) {
	base := strings.TrimSpace(os.Getenv(engineURLEnv))
	if base == "" {
		return nil, fmt.Errorf("%s is not set, so this command does not know where this deployment's engine is; set it to the engine's own address (http://127.0.0.1:8080 from inside its container) or to the published Web UI port, together with %s and %s", engineURLEnv, engineUsernameEnv, enginePasswordEnv)
	}
	return apiclient.New(apiclient.Config{
		BaseURL:   base,
		Username:  os.Getenv(engineUsernameEnv),
		Password:  os.Getenv(enginePasswordEnv),
		UserAgent: "backup-manager-cli/" + version,
	})
}

// configDivergence is #535 caught on the read side: the process serving
// this deployment is holding a configuration that is not the one this
// command loaded, so every answer derived from it would be about a world
// that deployment's Web UI does not have.
//
// It names both revisions rather than trying to say WHAT differs. The
// revision is a hash of the whole configuration, so this function has no
// idea which field moved, and a message that guessed would be worse than
// one that points at the two files and stops. What it does say is the
// thing an operator can act on: which file this command read, and that the
// serving process read a different one and will not re-read it.
func configDivergence(engine *service.RunningEngine, configFile, local, served string) error {
	return fmt.Errorf(
		"the process serving this deployment (state database %s) is holding a different configuration from %s, so nothing was printed: it is serving configuration %s and this command loaded %s, and nothing re-reads that file, so an answer from here would describe a deployment that process does not have. Restart that process to make it read %s, or make the change through the Web UI or HTTP API it serves",
		engine.StateDatabase, configFile, served, local, configFile)
}

// disagreement is what a command reports when the engine answered its
// question and answered it differently.
//
// The two sides are named the same way in every one of the five callers
// below, because an operator meeting this has to be able to tell which
// half is which without knowing which command produced it.
func disagreement(engine *service.RunningEngine, about string, mine, theirs []string) error {
	return fmt.Errorf(
		"the process serving this deployment (state database %s) does not agree with this command about %s, so nothing was printed. It reports %s; this command has %s. Two surfaces answering one question differently is the failure this check exists to catch, and the answer that matters is the serving process's",
		engine.StateDatabase, about, render(theirs), render(mine))
}

// render spells a set of ids for a refusal, sorted so two runs of the same
// disagreement read the same way.
func render(items []string) string {
	if len(items) == 0 {
		return "nothing"
	}
	sorted := slices.Clone(items)
	slices.Sort(sorted)
	return strings.Join(sorted, ", ")
}

// differ reports whether two sets of ids, compared as sets, disagree.
func differ(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return !slices.Equal(x, y)
}
