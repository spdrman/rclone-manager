package service

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/sourcecheck"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
)

// What `Test Connection` actually did (EPIC G, issues #592 and #596).
//
// # The button that could not be told apart from no button
//
// TestBackupSetConnection built a transport.Source out of the persisted
// set, made exactly one Transport.List call, threw the error away and
// answered {ok:false, message:"could not connect and list the remote
// path"}. DNS, the TCP connect, the host key check, the key material,
// the authentication and the listing were all one boolean, so a typo'd
// hostname, an unauthorised key, a rotated host key and a path that does
// not exist read identically. Those are four different afternoons.
//
// Throwing the error away was right and this file still does it. What is
// new is that six things are asked separately and answered separately,
// which is internal/sourcecheck's job; this file is the wiring that gives
// that package a real permission check on the configured key, a real
// known_hosts reading for the wording, knownhosts' own callback for the
// DECISION, and the real transport, and that puts what came back where an
// operator can read it.
//
// The candidate half of the same route (backupsets.go's testConnectionVia)
// wires the identical six steps from the request instead of from a
// persisted set, so a caller never has to know which mode it asked for to
// know what comes back.
//
// # The one host key comparison this repository makes
//
// hostKeyVerifier below is knownhosts.New over the file the transport is
// about to verify against, and it is the only reading of a known_hosts
// entry anywhere on this path. An earlier version of this file parsed the
// file itself, kept each line's KEY and dropped the marker and the host
// patterns, then compared fingerprints. That is permissive in both
// directions at once: an @revoked line reported as trusted, a key pinned
// for another host counted for this one, and a hashed entry (whose salt
// nothing else can reproduce) reported as a mismatch on a host that was
// perfectly fine. The first two are the dangerous ones and the third is
// what teaches an operator to click past them.
//
// # Why the steps go on the feed and not only in the response
//
// The response answers the browser that pressed the button. The feed
// answers everybody else: the global terminal, a second operator's
// window, the CLI following the same tail, and the log that outlives
// this process. A result that existed only in one HTTP response would
// leave "what did that test say" answerable only by the person who ran
// it and only until they navigated away, which is most of what was wrong
// with the button in the first place.
//
// Emission goes through obs, like every other line on that feed, so the
// steps arrive with sequence numbers from the same counter as the
// cycle's own events and interleave with them exactly where they
// happened. It also means they go through the redaction that already
// runs on that path.
//
// # A local source answers a shorter question honestly
//
// Five of the six steps are about reaching an SSH server. A backup set
// whose source is local has no host, no key and no host key, and the
// honest report is five skipped steps saying so plus a real listing,
// rather than five invented passes.

// connectionTestEventName is the obs event name every connection-test
// step is emitted under.
//
// Its own name rather than EventError or EventAPIAction: an operator
// filtering a terminal for "what did that test say" is asking about a
// thing, and a step that PASSED is not an error and is not an API action
// either. The client renders it by name (activityLine's own switch), the
// way it renders every other event on this feed.
const connectionTestEventName = "connection_test"

// connectionTestAction is the action name the start and the completion of
// one whole test are paired under (obs.Action, issue #625). It is the
// event's own name because the two are the same thing here: this event
// exists to report this action and nothing else.
const connectionTestAction = "connection_test"

// runConnectionTest performs the six-step check against one persisted
// backup set and returns the result the API and the CLI both render.
//
// It never returns a Go error. Every way this can go wrong is a fact
// about the operator's configuration or about the far side, which is an
// ordinary outcome and belongs on a step. That is the same argument
// TestConnection has always made about its own boolean, kept.
func (b *BackupService) runConnectionTest(ctx context.Context, id string, set *config.BackupSet, src transport.Source) ConnectionTestResult {
	target := sourcecheck.Target{
		BackupSetID:          id,
		Host:                 src.Host,
		Port:                 src.Port,
		User:                 src.User,
		RemotePath:           src.Root,
		KeyReference:         keyReferenceOf(set.Remote),
		PassphraseConfigured: set.Remote.Key.Passphrase.File != "" || set.Remote.Key.Passphrase.Env != "" || len(set.Remote.Key.Passphrase.Command) > 0,
	}

	deps := sourcecheck.Deps{
		// The same mode and directory-chain rule a real transfer applies
		// before it will touch the key, asked of the same code, plus the
		// public fingerprint out of this deployment's own key store. No
		// private half is opened anywhere on this path.
		Credentials: func(context.Context) (string, error) {
			return b.sourceKeyIdentity(src, set.Remote)
		},
		Trusted: func(context.Context) ([]sourcecheck.TrustedKey, error) { return trustedHostKeys(src.KnownHosts) },
		Verify:  hostKeyVerifier(src.KnownHosts),
		List: func(ctx context.Context) (int, error) {
			tr := b.state.Load().inner.Transport
			if tr == nil {
				return 0, fmt.Errorf("service: this deployment has no transport configured")
			}
			entries, err := tr.List(ctx, src)
			if err != nil {
				return 0, err
			}
			return len(entries), nil
		},
		// The classified cause goes to the log and never to the wire.
		// That costs a diagnostic in the API response deliberately: the
		// cause names a path on this host or the name of an environment
		// variable, which is a fact about this machine an API caller has
		// no use for and a reader of an exported response has every use
		// for. mediumcheck makes the identical trade.
		Observe: func(step sourcecheck.Step, err error) {
			b.logger.CompletedAt(ctx, obs.LevelWarn, obs.OutcomeError, connectionTestEventName,
				"connection test: "+string(step)+" failed",
				slog.String("backup_set", id),
				slog.String("step", string(step)),
				slog.String("cause", err.Error()),
			)
		},
	}

	// Bracketed, so a test that reaches an SSH server which then never
	// answers is a start with nothing behind it rather than silence
	// (issue #625). It is the shape every action that takes real time
	// owes: this one opens a connection to somebody else's machine, and
	// "did the button do anything" is exactly the question an operator
	// is left with when it hangs.
	action := b.logger.Begin(ctx, connectionTestEventName, connectionTestAction,
		"connection test starting", slog.String("backup_set", id))

	var report sourcecheck.Report
	if set.Remote.Type == "sftp" {
		report = sourcecheck.Run(ctx, target, deps)
	} else {
		report = runLocalConnectionTest(ctx, target, deps)
	}

	result := resultFromReport(report)
	b.recordConnectionTest(ctx, id, report)
	// The level stays a note or a warning whatever the verdict is, and
	// only the outcome goes to error. obs reserves LevelError for the
	// manager failing at something (see Alert and RetentionHold), and a
	// check that correctly reports a source as unreachable is the manager
	// working exactly as designed.
	verdict := connectionTestOutcome(report)
	// No backup_set here: the handle carries what Begin was given, so the
	// completion lands in the same ring the start did. Repeating it was
	// how this call site happened to be correct while the API let every
	// other one be wrong.
	action.EndAt(ctx, connectionTestLevel(verdict), verdict, "connection test finished")
	return result
}

// connectionTestOutcome is how the whole test went, as one value, from
// the six steps.
//
// A skipped step makes the test a warning rather than a success, and that
// is the same argument runLocalConnectionTest makes one layer down: five
// green rows for a backup set with no host at all would tell an operator
// a host key was checked when no host was involved. A verdict that
// reported it as an unqualified success would put that back, one line up.
func connectionTestOutcome(report sourcecheck.Report) obs.Outcome {
	if !report.OK {
		return obs.OutcomeError
	}
	for _, c := range report.Checks {
		if c.Outcome == sourcecheck.Skipped {
			return obs.OutcomeWarning
		}
	}
	return obs.OutcomeSuccess
}

// resultFromReport is how a six-step report becomes the two fields
// #211's callers have always read, and it is the SAME function for both
// modes of this endpoint.
//
// Message is the failed step's own Detail rather than a generic sentence
// or a re-derived one. It is a strict improvement for every existing
// caller: still one safe-to-render string this package composed, and now
// it says what went wrong instead of naming the whole button. Deriving it
// once here is also what keeps the two modes from drifting into two
// different messages for the same failure.
func resultFromReport(report sourcecheck.Report) ConnectionTestResult {
	result := ConnectionTestResult{OK: report.OK}
	for _, c := range report.Checks {
		result.Checks = append(result.Checks, ConnectionCheck{
			Step:     string(c.Step),
			Outcome:  string(c.Outcome),
			Category: c.Category,
			Detail:   c.Detail,
			// Milliseconds, and 0 stays 0: sourcecheck leaves Duration
			// unset on the steps that share one call, and every layer
			// above omits the field rather than printing a zero.
			DurationMs: int(c.Duration.Milliseconds()),
		})
	}
	if report.OK {
		return result
	}
	if stopped, ok := report.StoppedAt(); ok && stopped.Detail != "" {
		result.Message = stopped.Detail
		return result
	}
	result.Message = "could not connect and list the remote path"
	return result
}

// hostKeyVerifier is the host key DECISION, and it is knownhosts' own
// callback over the very file the transport is about to verify against.
//
// This is the only kind of host key comparison this repository makes.
// backupsethostkey.go reaches for knownhosts.New five times for the same
// reason: a marker (@revoked, @cert-authority), a line's host patterns
// and a hashed |1|salt|hash entry are all part of what a known_hosts line
// MEANS, and a reading that compares only fingerprints discards every one
// of them. It discards them permissively, too, so a check built that way
// passes on connections the transport then refuses: a revoked key reports
// as trusted, and a key pinned for some other host counts for this one.
//
// A path that cannot be read produces a callback that refuses everything
// with the reason, rather than one that accepts. "I could not check" is
// the outcome an attacker would choose, so it is never a pass.
func hostKeyVerifier(path string) func(string, net.Addr, ssh.PublicKey) error {
	if path == "" {
		return func(string, net.Addr, ssh.PublicKey) error {
			return fmt.Errorf("service: this backup set names no known_hosts file, so no host key can be trusted for it")
		}
	}
	check, err := knownhosts.New(path)
	if err != nil {
		return func(string, net.Addr, ssh.PublicKey) error {
			return fmt.Errorf("service: this backup set's known_hosts file could not be read, so the offered host key was compared against nothing: %w", err)
		}
	}
	return check
}

// sourceKeyIdentity answers the credentials step: whether this set's key
// may be used on this host, and what its public half is called.
//
// Two questions, one os.Stat pair and one store lookup, and no private
// key is opened for either. rclone.CheckSourceKeyFile is the same mode
// and directory-chain rule a real transfer applies, asked of the same
// code so the two cannot disagree. The fingerprint comes out of this
// deployment's key store, which already holds the public half.
//
// An empty fingerprint with a nil error is a real answer: this set points
// at a key this deployment does not manage, which is every set using a
// mounted or hand-provisioned key file and a perfectly ordinary shape.
// The step passes and its detail says the public half could not be named
// without opening the private one, which is a fact about how the set is
// configured rather than a failure.
func (b *BackupService) sourceKeyIdentity(src transport.Source, remote config.Remote) (string, error) {
	if err := rclone.CheckSourceKeyFile(src); err != nil {
		return "", err
	}
	id := sshKeyIDFor(b.configPath, remote)
	if id == "" {
		return "", nil
	}
	return storedKeyFingerprint(b.configPath, id), nil
}

// runLocalConnectionTest answers for a backup set whose source is not
// sftp. Five of the six steps are about reaching an SSH server and there
// is not one, so they are SKIPPED with the reason rather than passed.
//
// Passing them would be the exact failure sourcecheck.Skipped exists to
// prevent, one layer up: an operator reading six green rows would believe
// a host key was checked when no host was involved at all.
func runLocalConnectionTest(ctx context.Context, target sourcecheck.Target, deps sourcecheck.Deps) sourcecheck.Report {
	const reason = "this backup set reads a local path, so there is no host to reach"
	report := sourcecheck.Report{BackupSetID: target.BackupSetID, OK: true}
	for _, step := range sourcecheck.Steps {
		if step != sourcecheck.StepList {
			report.Checks = append(report.Checks, sourcecheck.Check{Step: step, Outcome: sourcecheck.Skipped, Detail: reason})
			continue
		}
		entries, err := deps.List(ctx)
		if err != nil {
			if deps.Observe != nil {
				deps.Observe(sourcecheck.StepList, err)
			}
			report.OK = false
			path := target.RemotePath
			if path == "" {
				path = "/"
			}
			report.Checks = append(report.Checks, sourcecheck.Check{
				Step: sourcecheck.StepList, Outcome: sourcecheck.Failed,
				Category: sourcecheck.CategoryRemotePath,
				Detail:   path + " could not be listed on this host",
			})
			continue
		}
		path := target.RemotePath
		if path == "" {
			path = "/"
		}
		report.Checks = append(report.Checks, sourcecheck.Check{
			Step: sourcecheck.StepList, Outcome: sourcecheck.Passed,
			Detail: path + " listed, " + entriesWord(entries),
		})
	}
	return report
}

func entriesWord(n int) string {
	if n == 1 {
		return "1 entry"
	}
	return fmt.Sprintf("%d entries", n)
}

// recordConnectionTest puts every step on the feed, one line each,
// attributed to this backup set.
//
// One line per STEP rather than one per test, because the terminal is
// where an operator reads this and a single line carrying six outcomes is
// a line nobody can filter, colour or quote. The `backup_set` field is
// what liveactivity's attributeRecord reads, so these land on that set's
// own ring and on nobody else's.
func (b *BackupService) recordConnectionTest(ctx context.Context, id string, report sourcecheck.Report) {
	for _, c := range report.Checks {
		attrs := []slog.Attr{
			slog.String("backup_set", id),
			slog.String("step", string(c.Step)),
			// step_outcome, not outcome. The line states how the step
			// went in obs's own four-value vocabulary, under the
			// reserved `outcome` key, and this is sourcecheck's finer
			// word for the same fact: passed, failed or skipped. Two
			// values under one key would be a duplicate in the JSON
			// object, where the tap takes the first and encoding/json
			// keeps the last, so the terminal and the log would disagree
			// about how a step went (issue #625; see obs/action.go's
			// reserved keys, and DiskPressure's own doc for the same
			// mistake one field along).
			slog.String("step_outcome", string(c.Outcome)),
			slog.String("detail", c.Detail),
		}
		if c.Category != "" {
			attrs = append(attrs, slog.String("category", c.Category))
		}
		outcome := stepOutcome(c.Outcome)
		b.logger.CompletedAt(ctx, connectionTestLevel(outcome), outcome, connectionTestEventName,
			"connection test: "+string(c.Step)+" "+string(c.Outcome), attrs...)
	}
}

// stepOutcome maps one step's own word onto the vocabulary every line on
// this feed states its outcome in.
//
// Skipped is a warning and not a note, which is the whole of what
// sourcecheck.Skipped is for: a step that did not run proved nothing, and
// a client colouring it the way it colours a passing step is the failure
// runLocalConnectionTest's doc describes. It used to be a note, because
// the level was the only field there was and "this did not apply" is not
// a warning about the system; with the two facts apart, the level can
// stay quiet about it while the outcome refuses to call it a pass.
func stepOutcome(o sourcecheck.Outcome) obs.Outcome {
	switch o {
	case sourcecheck.Failed:
		return obs.OutcomeError
	case sourcecheck.Skipped:
		return obs.OutcomeWarning
	default:
		return obs.OutcomeSuccess
	}
}

// connectionTestLevel is how loudly a step or a verdict is logged, which
// is a quieter question than how it went.
//
// A failed step is stated as an error outcome and logged as a warning,
// and the pair is deliberate rather than an inconsistency. obs reserves
// LevelError for the manager failing at something (Alert and
// RetentionHold both say so), and a test that reaches a host and is
// correctly refused has not failed at anything: it has answered. The
// outcome is what a terminal colours by, so an operator still reads it
// as the bad news it is, and `activity --follow --severity error` still
// means "the manager is in trouble" rather than "somebody typed the
// wrong hostname into a wizard".
func connectionTestLevel(outcome obs.Outcome) obs.Level {
	if outcome == obs.OutcomeSuccess {
		return obs.LevelInfo
	}
	return obs.LevelWarn
}

// keyReferenceOf is how the configuration NAMES this set's key, for the
// credentials step's detail. It is the reference an operator would go and
// look at, never the key and never a path this process resolved: an
// env-var name and a command are safe to print, a key file's path is the
// one of the three that is a fact about this machine, so it is named as
// its configured spelling rather than its expanded form.
func keyReferenceOf(r config.Remote) string {
	switch {
	case r.Key.File != "":
		return "this backup set's configured key file"
	case r.Key.Env != "":
		return "the key in $" + r.Key.Env
	case len(r.Key.Command) > 0:
		return "the key the configured command produces"
	default:
		return ""
	}
}

// trustedHostKeys reads the host keys a backup set's own known_hosts file
// pins.
//
// Line-by-line with ssh.ParseKnownHosts rather than through
// knownhosts.New's callback, because what this needs is the LIST: a
// mismatch is settled by an operator comparing the offered fingerprint
// against the trusted one by eye, and a callback answers yes or no
// without ever naming what it wanted.
//
// Hashed entries (|1|salt|hash) cannot be compared here: the salt is per
// line and x/crypto/ssh does not export the hash. This product writes
// plain entries itself (knownhosts.Line), so a hashed one is hand-written,
// and it is reported as a key this reading could not compare rather than
// silently treated as absent, which would draw a correctly trusted host
// as a mismatch.
func trustedHostKeys(path string) ([]sourcecheck.TrustedKey, error) {
	if path == "" {
		return nil, fmt.Errorf("service: this backup set names no known_hosts file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var out []sourcecheck.TrustedKey
	scanner := bufio.NewScanner(f)
	// A known_hosts line holds a full public key in base64, so the
	// default 64KiB token is comfortable for every algorithm in use and
	// bounded against a file that is not one.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		if strings.HasPrefix(text, "|") {
			out = append(out, sourcecheck.TrustedKey{
				Algorithm:   "hashed entry",
				Fingerprint: "a hashed known_hosts entry, which cannot be compared by fingerprint",
				Line:        line,
			})
			continue
		}
		_, _, key, _, _, err := ssh.ParseKnownHosts([]byte(text))
		if err != nil {
			// A line this build cannot parse is reported as a line, not
			// skipped: an operator whose known_hosts has a broken entry
			// needs to be told which one rather than shown a mismatch
			// against a file that looked empty.
			out = append(out, sourcecheck.TrustedKey{
				Algorithm:   "unreadable entry",
				Fingerprint: "this line could not be parsed as a known_hosts entry",
				Line:        line,
			})
			continue
		}
		out = append(out, sourcecheck.TrustedKey{
			Algorithm:   key.Type(),
			Fingerprint: ssh.FingerprintSHA256(key),
			Line:        line,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
