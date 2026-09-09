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
//	sources     BackupSet carries no remote type, and the CLI prints one
//	            on every line. stale_after used to be here too and is not
//	            any more: #555 put stale_after_seconds on the wire.
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
// through the operations #544 names (listBackupSets, listArtifacts,
// getArtifact, previewRetention, getSystemHealth) and compares. That is
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
		// for real reasons (EACCES on a lock file owned by another uid,
		// EIO on a sick volume, ENOTSUP where flock is unavailable, and
		// the whole non-unix build), and every one of those is a moment
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

	// Named for what it is rather than `version`, which is this package's
	// own build stamp: two different facts one letter apart is how a
	// wrong one gets printed.
	served, err := client.SystemVersion(ctx)
	if err != nil {
		return d.unconfirmed(fmt.Sprintf("another process is serving this deployment (state database %s) and it did not answer at %s (%v)", engine.StateDatabase, client.BaseURL(), err), announceTo), nil
	}

	// Which deployment before which configuration, because they are
	// different questions and the first one is the more basic (#555).
	// A revision is a hash of configuration CONTENT, so two deployments
	// built from one template hold the same one: on a host running a
	// staging and a production instance from one compose file, a read
	// pointed at the wrong address passed the revision comparison and
	// printed the other instance's world. Asking the revision first would
	// also mean telling an operator their configurations differ when what
	// actually happened is that they reached a different deployment.
	//
	// An identity either side cannot name is NOT a refusal here, unlike on
	// the write path. It joins the other three ways this file ends up
	// unconfirmed, for this file's own reason: a write that cannot confirm
	// something has an alternative, which is not writing, and a read has
	// none.
	//
	// What it must not do is stop the checking. It used to return here,
	// and #559's review drove what that costs: served.DeploymentID is ""
	// on every engine older than this build, and mine is "" on every
	// deployment until the first restart that mints one, so for the whole
	// of its own rollout this read skipped the revision comparison
	// underneath it and #535 came back on the four commands an operator
	// runs when something is already wrong, quietly, at exit 0. An
	// identity nobody can confirm is a reason to check less confidently
	// and never a reason to stop checking what still works, so the caveat
	// is carried and the revision comparison runs anyway.
	mine, err := service.DeploymentIdentity(engine.StateDatabase)
	if err != nil {
		return d.unconfirmed(fmt.Sprintf("another process is serving this deployment (state database %s) and this command could not read which deployment it is standing in (%v)", engine.StateDatabase, err), announceTo), nil
	}
	if mine != "" && served.DeploymentID != "" && mine != served.DeploymentID {
		d = d.unconfirmed(fmt.Sprintf("another process is serving this deployment (state database %s) and the engine at %s serves a different deployment", engine.StateDatabase, client.BaseURL()), announceTo)
		return d, wrongDeploymentRead(engine, client.BaseURL(), mine, served.DeploymentID)
	}
	// Split three ways rather than "one of the two could not say", for the
	// reason mode.go gives about the write path: the remedies differ. This
	// deployment having no name yet is fixed by restarting the process
	// that serves it. An engine that names none is a process to restart on
	// this build. Neither of them is the other, and an operator told only
	// that "one of the two" could not answer has to work out which.
	caveat := ""
	switch {
	case mine == "" && served.DeploymentID == "":
		caveat = fmt.Sprintf("another process is serving this deployment (state database %s) and neither end could say which deployment it is: this one has no identity yet and the engine at %s named none either", engine.StateDatabase, client.BaseURL())
	case mine == "":
		caveat = fmt.Sprintf("another process is serving this deployment (state database %s) and this deployment has no identity yet, so the engine at %s cannot be shown to be the one serving it (an identity is minted by the process that serves a deployment, so restarting that process gives this one a name)", engine.StateDatabase, client.BaseURL())
	case served.DeploymentID == "":
		caveat = fmt.Sprintf("another process is serving this deployment (state database %s) and the engine at %s answered without naming a deployment, which is what a build from before this check looks like and also what an engine that could not read its own identity looks like", engine.StateDatabase, client.BaseURL())
	}

	local := service.ConfigRevisionOf(cfg)
	if served.ConfigRevision != local {
		// Announced as unconfirmed rather than as engine-attached: the
		// engine answered, but what it answered is that this command is
		// holding a different deployment's configuration, so nothing that
		// follows would have been about the engine's world.
		because := fmt.Sprintf("another process is serving this deployment (state database %s) and holds a different configuration", engine.StateDatabase)
		if caveat != "" {
			because = caveat + ", and it holds a different configuration as well"
		}
		d = d.unconfirmed(because, announceTo)
		return d, configDivergence(engine, d.configFile, local, served.ConfigRevision, caveat != "")
	}
	if caveat != "" {
		// The revisions match, so the two processes hold content-identical
		// configurations, and that is worth having. It is not proof this
		// is the same deployment: two instances built from one template
		// share a revision, which is the whole reason the identity exists.
		// So the answer is printed and the mode says it was not confirmed.
		return d.unconfirmed(caveat, announceTo), nil
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
	base := strings.TrimSpace(os.Getenv(apiURLEnv))
	if base == "" {
		return nil, fmt.Errorf("%s is not set, so this command does not know where this deployment's engine is; set it to the engine's own address (http://127.0.0.1:8080 from inside its container) or to the published Web UI port, together with %s and %s", apiURLEnv, apiUsernameEnv, apiPasswordEnv)
	}
	return apiclient.New(apiclient.Config{
		BaseURL:   base,
		Username:  os.Getenv(apiUsernameEnv),
		Password:  os.Getenv(apiPasswordEnv),
		UserAgent: "rclone-manager-cli/" + version,
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
//
// unconfirmedDeployment says, when it is true, that the two sides could
// not be shown to be one deployment either. It is appended rather than
// substituted: the configurations really do differ and that really is
// worth refusing on, and the second possibility is that they are not the
// same deployment at all, which has a different remedy.
func configDivergence(engine *service.RunningEngine, configFile, local, served string, unconfirmedDeployment bool) error {
	also := ""
	if unconfirmedDeployment {
		also = fmt.Sprintf(" This command also could not confirm the two are the same deployment, because one of them could not name itself, so the other possibility is that $%s reaches a different deployment on this host rather than a stale copy of this one.", apiURLEnv)
	}
	return fmt.Errorf(
		"the process serving this deployment (state database %s) is holding a different configuration from %s, so nothing was printed: it is serving configuration %s and this command loaded %s, and nothing re-reads that file, so an answer from here would describe a deployment that process does not have.%s Restart that process to make it read %s, or make the change through the Web UI or HTTP API it serves",
		engine.StateDatabase, configFile, served, local, also, configFile)
}

// wrongDeploymentRead is #555 on the reading side: the engine this command
// was told to check itself against is not the one serving this deployment
// at all, so every answer it gave is about somebody else's world.
//
// It refuses rather than falling back to the file, for the same reason
// configDivergence does: the engine answered, and what it answered is that
// this is not its deployment, so printing under an announcement that
// something had been checked would be worse than printing nothing.
//
// Both identities, because an operator who has just mistyped an address
// needs to see the one they meant beside the one they reached. Told only
// where they ended up, they still cannot tell whether that was the
// instance they wanted.
func wrongDeploymentRead(engine *service.RunningEngine, address, mine, theirs string) error {
	return fmt.Errorf(
		"the engine at %s is not the process serving this deployment, so nothing was printed: it serves deployment %s, and this deployment (state database %s) is %s. An answer checked against that engine would describe a different deployment on this host, so check $%s",
		address, theirs, engine.StateDatabase, mine, apiURLEnv)
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
