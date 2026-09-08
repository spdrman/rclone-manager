package service

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/sourcecheck"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
)

// What `Test Connection` actually did (EPIC G, issue #596).
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
// that package a real key resolver, a real known_hosts reading, a real
// host key probe and the real transport, and that puts what came back
// where an operator can read it.
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
		// The same three key sources, the same passphrase handling and
		// the same at-rest decryption a real transfer uses. A second
		// resolver here would be a second SSH posture; see
		// internal/transport/rclone.SourceSigner.
		Credentials: func(context.Context) (ssh.Signer, string, error) { return rclone.SourceSigner(src) },
		Trusted:     func(context.Context) ([]sourcecheck.TrustedKey, error) { return trustedHostKeys(src.KnownHosts) },
		ProbeHostKey: func(ctx context.Context, host string, port int) (string, string, error) {
			res, err := rclone.ProbeHostKey(ctx, host, port)
			if err != nil {
				return "", "", err
			}
			return res.Algorithm, res.Fingerprint, nil
		},
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
			b.logger.Event(ctx, obs.LevelWarn, connectionTestEventName,
				"connection test: "+string(step)+" failed",
				slog.String("backup_set", id),
				slog.String("step", string(step)),
				slog.String("cause", err.Error()),
			)
		},
	}

	var report sourcecheck.Report
	if set.Remote.Type == "sftp" {
		report = sourcecheck.Run(ctx, target, deps)
	} else {
		report = runLocalConnectionTest(ctx, target, deps)
	}

	result := ConnectionTestResult{OK: report.OK}
	for _, c := range report.Checks {
		result.Checks = append(result.Checks, ConnectionCheck{
			Step:     string(c.Step),
			Outcome:  string(c.Outcome),
			Category: c.Category,
			Detail:   c.Detail,
		})
	}
	if !report.OK {
		// The sentence a client reading only ok/message has always got,
		// now naming the step it stopped at rather than the whole
		// button. It stays a sentence this package owns and never
		// carries an underlying error's text.
		if stopped, ok := report.StoppedAt(); ok {
			result.Message = "the connection test stopped at " + string(stopped.Step)
		} else {
			result.Message = "could not connect and list the remote path"
		}
	}

	b.recordConnectionTest(ctx, id, report)
	return result
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
		level := obs.LevelInfo
		if c.Outcome == sourcecheck.Failed {
			level = obs.LevelWarn
		}
		attrs := []slog.Attr{
			slog.String("backup_set", id),
			slog.String("step", string(c.Step)),
			slog.String("outcome", string(c.Outcome)),
			slog.String("detail", c.Detail),
		}
		if c.Category != "" {
			attrs = append(attrs, slog.String("category", c.Category))
		}
		b.logger.Event(ctx, level, connectionTestEventName,
			"connection test: "+string(c.Step)+" "+string(c.Outcome), attrs...)
	}
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
