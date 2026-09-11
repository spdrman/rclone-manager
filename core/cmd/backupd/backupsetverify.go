package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/backupdproject/backupd/core/internal/app"
	"github.com/backupdproject/backupd/core/service"
)

// Issue #624 (H2.3): proving a backup set's SSH connection before this
// command relies on it, and the verb that proves one afterwards.
//
// # The asymmetry this closes
//
// This product relies on two kinds of connection: the SSH connection to a
// backup source, and the connection to a storage destination. The
// destination side has been proven before it is relied on since #443.
// `medium add` verifies by default and refuses to write when the check
// fails, `--no-verify` is an explicit opt-out whose output says in so many
// words that no credential was obtained and no endpoint was contacted, and
// the S3 wizard keeps Save disabled until the candidate check comes back
// ok. A destination nobody proved is therefore something somebody asked
// for on purpose.
//
// The source side had none of it. `backup-set create` ran no check and had
// no flag to skip, because there was nothing to skip. The wizard was
// stronger and still short of it: Save was gated on a pinned known_hosts
// line and an imported key, which proves HOST IDENTITY and not a working
// connection, so it settled that the machine answering is the one whose
// fingerprint somebody compared and settled nothing about whether the key
// authenticates or whether the account can read the folder the backups are
// in. The six-step check #596 built proves all of that and was reachable
// only from a set that ALREADY existed, so the order was backwards: create,
// rely on it, and only then be able to test it.
//
// This file gives the source side the destination side's shape rather than
// a second shape of its own.
//
// # Which world the check runs in, and why it is never skipped
//
// A connection test needs somewhere to run and, for a persisted set, its
// six steps go onto the live feed, which needs a serving process. This
// command answers that by putting the check through the SAME door the
// write goes through (openConfigWriteRoute), so there are three outcomes
// and none of them is a silent pass:
//
// With nothing serving this deployment, this process is the deployment's
// only authority. It opens the service over the same config.yaml it is
// about to write, runs the check here, and writes here. Nothing is
// skipped, and the six steps are printed to the terminal that asked, which
// is where the answer is wanted.
//
// With a serving engine this command can reach, the check is made BY that
// engine, over its own API, exactly as the write is. That matters for more
// than symmetry: proving a route from this shell and then writing the set
// into a process on the other end of a container boundary would be proving
// one deployment's path to a host and declaring the set on another. It is
// also what puts the steps on the live feed for everybody else, because
// the process with the feed is the one that ran them.
//
// With a serving engine this command cannot reach, openConfigWriteRoute
// refuses before either the check or the write, which it already did
// before this issue. Nothing is written and nothing is claimed.
//
// The one thing that never happens is the one #624 asks about by name: a
// create against a deployment with nothing running does NOT quietly skip
// the check and report success. It runs it, on the durable path, in this
// process.

// sourceConnectionTester is the one call proving a candidate source takes.
//
// An interface of exactly one method rather than the whole route, for the
// reason backupSetCreatePrereqs is one: the first-run path has no engine
// route at all and calls service.FirstRun, and both are asked the same
// question in the same words. It also means the helper below cannot reach
// a write from inside a check.
type sourceConnectionTester interface {
	TestConnection(ctx context.Context, req service.ConnectionTestRequest) (service.ConnectionTestResult, error)
}

// connectionTestFor is the candidate a create is about to write, as the
// check's own request.
//
// It reads the CreateBackupSetRequest after resolveKeyAndTrust has filled
// in the key id and the trusted line, which is why every caller runs it
// there and not earlier: a check against a request still carrying an empty
// ssh_key_id would be refused as malformed rather than answering anything
// about the source.
func connectionTestFor(req service.CreateBackupSetRequest) service.ConnectionTestRequest {
	return service.ConnectionTestRequest{
		Host:           req.Host,
		Port:           req.Port,
		User:           req.User,
		SSHKeyID:       req.SSHKeyID,
		KnownHostsLine: req.KnownHostsLine,
		RemotePath:     req.RemotePath,
	}
}

// proveSourceConnection runs the six-step check in front of a write and
// reports the exit code the caller should return, or 0 to carry on.
//
// The shape is mediumWrite's, deliberately: verify, print the whole
// report, refuse on a failure, and say out loud when the check was skipped.
// Two shapes for one decision would be two things for an operator to learn
// about one product.
//
// afterwards is the command that proves this set once the reason for
// skipping has gone away. It is printed on every --no-verify, because an
// escape hatch with no way back out of it is a hole.
func proveSourceConnection(ctx context.Context, svc sourceConnectionTester, noVerify bool, req service.ConnectionTestRequest, afterwards string) int {
	if noVerify {
		// Said out loud, every time. A --no-verify write that printed
		// what a verified one printed would be a green line standing
		// behind a check nobody ran, which is the whole reason the
		// destination side says this too.
		fmt.Println("not verified: --no-verify was given, so nothing was resolved, no host was contacted, no key was offered and no remote folder was listed")
		fmt.Println("  this backup set is marked as unverified until a connection test passes")
		fmt.Printf("  prove it afterwards with: %s\n", afterwards)
		return 0
	}

	result, err := svc.TestConnection(ctx, req)
	if err != nil {
		// A Go error here is not a failed connection: the check reports
		// one of those through the result. This is a request the service
		// would not accept or a deployment with no transport at all, and
		// it is reported as what it is.
		return fail(err)
	}
	printConnectionReport("connection to "+req.Host, result)
	if !result.OK {
		fmt.Println("nothing was written: a source that cannot be proven is not a source this manager will back up from")
		fmt.Println("  --no-verify writes it anyway, marked as unverified, for building configuration offline")
		return 1
	}
	return 0
}

// backupSetConnectionFlags are the patch flags that name something a
// connection test can settle (issue #624).
//
// The same six things core/service's changesTheConnection compares, named
// as flags rather than as fields because this side of the boundary has
// flags and not a persisted set. Eight names for six things: the key and
// the trusted host key each have two spellings on this command, one that
// carries a reference and one that goes and gets it. It is used for what this command SAYS and
// never for what it does: the decision to run the check belongs to the
// process holding the configuration, for the reason
// UpdateBackupSetRequest.SkipConnectionCheck's own doc gives, and a copy of
// that decision here is a copy that can drift.
var backupSetConnectionFlags = []string{
	"host", "port", "user", "ssh-key-file", "ssh-key-id",
	"known-hosts-line", "trust-host-key", "remote-path",
}

// namesAConnectionField reports whether this invocation PASSED any of them.
//
// fs.Visit rather than a zero-value test, for the reason buildBackupSetPatch
// gives: `--port 0` and `--user ""` are values an operator can type and
// mean, so "not passed" and "passed as its zero value" have to stay
// distinguishable here too.
func namesAConnectionField(fs *flag.FlagSet) bool {
	named := false
	fs.Visit(func(fl *flag.Flag) {
		for _, name := range backupSetConnectionFlags {
			if fl.Name == name {
				named = true
			}
		}
	})
	return named
}

// printConnectionReport renders one connection test, every step in the
// engine's own order, skipped ones included.
//
// Never a single OK or FAILED, for the reason #596 exists: DNS, the TCP
// connect, the host key, the key material, the authentication and the
// listing used to be one boolean, so a typo'd hostname, an unauthorised
// key, a rotated host key and a path that does not exist read identically,
// and those are four different afternoons. Collapsing them again here
// would throw the whole diagnosis away at the surface an operator is
// actually looking at.
//
// A skipped step is printed as skipped rather than as a pass, which is the
// same rule printMediumReport follows: a local source honestly skips the
// five steps about reaching an SSH server, and rendering those as green
// would tell somebody a host key was checked when no host was involved.
//
// The column formatting is shared with the destination side's report
// (outcomeWordOf, medium.go) so that "did this step pass" has one spelling
// across both halves of the product.
//
// heading is the caller's, so each surface names what it is talking about
// in the words that surface has: a create has a host and no set id yet,
// and the check verb has a set id and no reason to repeat a host the
// configuration already holds.
func printConnectionReport(heading string, result service.ConnectionTestResult) {
	fmt.Printf("%s: %s\n", heading, connectionVerdictWord(result.OK))
	for _, c := range result.Checks {
		fmt.Printf("  %-14s %-8s %s\n", c.Step, outcomeWordOf(c.Outcome, c.Category), c.Detail)
	}
	if !result.OK && result.Message != "" {
		fmt.Printf("  %s\n", result.Message)
	}
}

// connectionVerdictWord renders the whole report's answer as something an
// operator reads rather than as a boolean, the same way verdictWord does
// for a destination.
func connectionVerdictWord(ok bool) string {
	if ok {
		return "ready to back up from"
	}
	return "NOT reachable; see the failing check below"
}

// backupSetTestConnection is the `test-connection` verb (and `preflight`,
// its alias): the six-step check against a backup set that already exists.
//
// # Why this verb had to exist
//
// The check has been the only thing behind POST
// /backup-sets/test-connection since #596 and no verb reached it, which
// core/cliecho recorded as a standing gap: the Web UI's global terminal
// echoes the command for every action an operator takes, and this action
// had no command to echo. So the one thing somebody most wants from a
// terminal when a backup has stopped working, ask the manager what it can
// actually see, could only be done from a browser.
//
// # Why it goes through the write door
//
// It is a check, and a check is a read, so openConfigWriteRoute looks like
// the wrong door until you notice what a passing check now does: it clears
// issue #624's unverified mark, which is a configuration write. Routing it
// with the writes is what keeps that honest. Beside a serving engine the
// check happens in the process that owns the configuration and the live
// feed, so the six steps reach every other window rather than only this
// terminal; with nothing serving, it happens here, against the same file
// this process would write. With a serving engine this command cannot
// reach it is refused, which is the right answer rather than a missing
// feature: a check run here would prove this shell's route to the host and
// then be unable to record it where the deployment could see it.
//
// A failing check exits non-zero, so this composes into a script the way
// `check`, `validate` and `medium preflight` already do.
func backupSetTestConnection(f *backupSetFlags, id string) int {
	ctx := context.Background()
	route, cleanup, err := openConfigWriteRoute(ctx, *f.cfgPath)
	if err != nil {
		return fail(err)
	}
	defer cleanup()

	logStartup(ctx, logger(), app.BuildVersionInfo(version, commit))

	result, err := route.TestBackupSetConnection(ctx, id)
	if err != nil {
		return fail(err)
	}
	printConnectionReport("backup set "+id, result)
	if !result.OK {
		return 1
	}
	return 0
}
