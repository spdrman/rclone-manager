package compat

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/spdrman/rclone-manager/core/cliecho"
	"github.com/spdrman/rclone-manager/core/internal/lifecycle"
	"github.com/spdrman/rclone-manager/core/service"
)

// FR-35 clause 4, the CLI: build rbm from this working tree,
// run a fixed table of invocations against the seeded medium-free
// deployment, and write down exactly what an operator would have seen.
//
// This is the file that decides what "operator-visible" means for the rest
// of the repository. Every line it records becomes a line in the corpus,
// so the usage block, the column padding, the error sentences and the exit
// statuses of the commands listed below are pinned byte for byte from
// here. Anything reworded on the other side of that boundary, in
// core/cmd/rbm, arrives as a red cell in this package, which is
// the intended and only route.
//
// Three things are normalized before anything is compared and no more: the
// throwaway root directory, the Go toolchain version, and the wall clock
// on an FR-23 event line. All three are the machine's facts rather than
// the product's, and each is argued where it happens (normalizeRoot in
// capture_state.go, normalizeGoVersion and normalizeEventTime below). The
// rclone version deliberately is not normalized, and neither is anything
// else on those event lines.
//
// The argv table is chosen rather than exhaustive, and captureCLI says
// which surfaces it leaves out and why, because a surface nobody mentions
// cannot be told apart from one nobody thought of.

// buildCLI builds rbm from this working tree.
//
// The binary, not run() called in-process: FR-35's CLI clause is about
// what an operator sees in a terminal after an upgrade, and the only way
// to see that is to look at a process's real stdout, real stderr and real
// exit status. An in-process call would also have had to live in package
// main, where every other lane is editing.
// It builds into outDir, which every caller gets from t.TempDir(), so the
// binary is cleaned up by the framework that made the directory. An
// earlier version cached one build in an os.MkdirTemp of its own to save
// the second test a rebuild; it saved about a second against a warm Go
// build cache and left a binary behind in the system temp directory on
// every run, which is a bad trade.
func buildCLI(coreRoot, outDir string) (string, error) {
	bin := filepath.Join(outDir, cliecho.Binary)
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/rbm")
	cmd.Dir = coreRoot
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("building "+cliecho.Binary+": %w\n%s", err, out)
	}
	return bin, nil
}

// cliCase is one invocation to pin.
type cliCase struct {
	label string
	args  []string

	// env is added to the scrubbed environment runCLI builds, and is the
	// only way anything beyond PATH, HOME and TZ reaches the child. Empty
	// for every case that is about argv alone, which is most of them.
	//
	// Every loop over a cliCase passes it through, including the ones
	// whose cases all leave it empty. A table that quietly dropped it
	// would make a case set here and ignored, which is the shape of bug
	// that reads as the product behaving differently from what the
	// capture says it was asked.
	env []string
}

// captureCLI runs the fixed argv table against the seeded, medium-free
// deployment and records exit status, stdout and stderr for each.
//
// The table is chosen for what EPIC E is about to touch: the artifact read
// surfaces (which FR-34 wants to grow an access state on), the per-artifact
// detail (where a placement would show up), the settings surface (which
// FR-27 wants to grow a medium block on), the refusals, and the usage text
// (which grows a line for every new subcommand). `retention` is not here;
// it is its own cell below, for the clock reason in this package's doc.
//
// It runs twice over, in two worlds. The table above is a deployment
// nothing is serving, which is where this package started and for a while
// was all it had. captureBesideAServingProcess below is the other one, and
// it is there because #551 made the exit status something a wrapper script
// branches on: see its own doc for what it drives and why four rows rather
// than one.
//
// What is left out, said out loud rather than quietly missing, because a
// surface nobody mentions is indistinguishable from one nobody thought of:
//
//   - `status` prints live free space. Two runs a second apart on the same
//     machine disagree, so it cannot be a golden line, and normalizing the
//     number away would leave a cell certifying the word "bytes".
//   - `run`, `daemon`, `fetch` and `reconcile` drive a real cycle against a
//     real remote and change state. They are the crash matrix's subject and
//     the CLI smoke slice's, not this cell's.
//   - `validate`, `quarantine` and `catalog rebuild` mutate the journal or
//     need an artifact in a condition this fixture does not stage.
//
// Of those, only `status` is a read surface EPIC E will touch, and it is
// the one gap in this cell worth knowing about.
func captureCLI(ctx context.Context, bin, cfgPath, root string) (Cell, Cell, error) {
	cases := []cliCase{
		{label: "version", args: []string{"version"}},
		{label: "sources", args: []string{"sources", "--config", cfgPath}},
		{label: "artifacts, unfiltered", args: []string{"artifacts", "--config", cfgPath}},
		{label: "artifacts, filtered to the source", args: []string{"artifacts", "--config", cfgPath, "--source", "production"}},
		{label: "artifacts, filtered to a source nobody configured", args: []string{"artifacts", "--config", cfgPath, "--source", "nope"}},
		{label: "artifacts, one artifact's detail", args: []string{"artifacts", "--config", cfgPath, "production/postgres-primary/recent-daily.dump"}},
		{label: "artifacts, a quarantined artifact's detail", args: []string{"artifacts", "--config", cfgPath, "production/postgres-primary/quarantined-newest.dump"}},
		{label: "artifacts, an artifact that does not exist", args: []string{"artifacts", "--config", cfgPath, "production/postgres-primary/no-such.dump"}},
		{label: "artifacts, a filter combined with an operand", args: []string{"artifacts", "--config", cfgPath, "--source", "production", "production/postgres-primary/recent-daily.dump"}},
		// Issue #598's verb. Four invocations, because the interesting
		// part of this surface is not the plain list: it is that --limit
		// counts MATCHING events rather than rows read (so a filter that
		// narrows still fills it), that an unrecognised --severity is a 2
		// rather than a silently unfiltered feed, and that --json emits
		// the contract's own object rather than this table.
		{label: "activity", args: []string{"activity", "--config", cfgPath, "--limit", "4"}},
		{label: "activity, errors only", args: []string{"activity", "--config", cfgPath, "--severity", "error", "--limit", "3"}},
		{label: "activity, as the contract shapes it", args: []string{"activity", "--config", cfgPath, "--limit", "1", "--json"}},
		{label: "activity, a severity nobody defined", args: []string{"activity", "--config", cfgPath, "--severity", "loud"}},
		{label: "settings", args: []string{"settings", "--config", cfgPath}},
		{label: "check", args: []string{"check", "--config", cfgPath}},
		{label: "a config path that is not there", args: []string{"check", "--config", filepath.Join(root, "absent.yaml")}},
	}

	var lines []string
	for _, c := range cases {
		res, err := runCLI(ctx, bin, c.args, root, c.env...)
		if err != nil {
			return Cell{}, Cell{}, err
		}
		lines = append(lines, res...)
	}

	held, err := captureBesideAServingProcess(ctx, bin, cfgPath, root)
	if err != nil {
		return Cell{}, Cell{}, err
	}
	lines = append(lines, held...)

	// The two invocations whose whole output is the usage block are their
	// own cell, compared additively.
	//
	// Not to be lenient. The usage text is the one part of this surface
	// where a pure addition is both routine and harmless: a new subcommand
	// adds lines and changes nothing an operator was already reading, and
	// with ten lanes active in this repository that happens often enough
	// that an exact comparison would train people to regenerate the corpus
	// without reading it, which is the failure this whole thing exists to
	// prevent. It happened on the first main this gate met: #350 added
	// `backup-set create` and `backup-set patch`, twenty-four lines, none
	// of them touching an existing one.
	//
	// Additive-only still refuses everything that matters here. A usage
	// line that is reworded, a flag that is removed, a subcommand that
	// disappears: each takes an existing line away and fails. And the
	// violation this cell family exists to catch, an additive column
	// rendered where there is no non-local placement, lands in the
	// artifact detail below, which is compared exactly.
	//
	// What it cannot refuse is a command that never got captured at all,
	// because a line that is not here is a line this rule has no opinion
	// about. `unconfigured` and `medium preflight` both shipped that way
	// and this cell stayed green throughout. That hole is closed from the
	// other end, by TestUsage_EveryRegisteredCommandIsPinned in
	// core/cmd/rbm, which is where the list of registered
	// commands can be read rather than guessed at (#549).
	var usage []string
	for _, c := range []cliCase{
		{label: "no arguments at all", args: []string{}},
		{label: "an unknown subcommand", args: []string{"definitely-not-a-command"}},
	} {
		res, err := runCLI(ctx, bin, c.args, root, c.env...)
		if err != nil {
			return Cell{}, Cell{}, err
		}
		usage = append(usage, res...)
	}

	return Cell{
		Certifies: "FR-35 clause 4: every one of these commands prints exactly what it printed before EPIC E, and exits the same way, in three worlds: with nothing serving this deployment, with something serving it and no route to that process, and with a route set to an address that is not this deployment's engine. A medium-free deployment has no non-local placement, so FR-35 allows this surface no additive column either.",
		Rule:      RuleIdentical,
		Lines:     lines,
	}, Cell{
		Certifies: "FR-35 clause 4, the usage block: a new subcommand may add lines to it, and nothing may reword, remove or renumber a line an operator already reads there.",
		Rule:      RuleAdditiveOnly,
		Lines:     usage,
	}, nil
}

// captureBesideAServingProcess is the same binary against the same
// deployment with another process serving it, which is where the exit
// status stops being a footnote (issue #551).
//
// # Why an exit code belongs in this corpus at all
//
// FR-35 clause 4 is about what an operator sees in a terminal, and this
// package has always recorded the status beside the two streams for that
// reason (runCLI's own doc). #551 made the status something a WRAPPER
// reads rather than something a person glances at: the refusal a
// configuration write gets beside a serving engine now exits 3 and
// nothing else does, so a provisioning script can wait on that one and
// abort on the rest. A number a script branches on is operator-visible
// surface in exactly the way a printed line is, and it was not pinned
// anywhere until here: every case above runs on a deployment nothing is
// serving, so the whole engine-attached half of this binary's behaviour
// was outside the gate.
//
// # The table, and why it is seven rows rather than one
//
// A cell that only recorded the 3s could not tell this binary from one
// that exits 3 for everything while an engine is up. So the two refusals
// are here with two things that are not refusals: a usage mistake typed
// beside the same serving process, which is still a usage mistake and
// still 2, and an ordinary read-only command, which is unaffected and
// still 0. Between them the cell fails if the refusal loses its code and
// fails if the code spreads.
//
// The last three are the misaimed route, and they are here because the
// four above them all describe a host that was told nothing about its
// engine. That is the half of engine-attached mode this corpus had, and
// the other half, an operator who DID set an address and got it wrong, is
// where the sentences a script reads actually live: the write refusals
// name $BACKUP_MANAGER_API_URL, the exit-code table promises a named route
// that did not answer is a 1 and not a 3, and #544's caveat line is the
// only thing standing between a read and an answer about somebody else's
// world. None of it was pinned anywhere. Two writes and one read, because
// the whole point of the read is that the same broken address that refuses
// a write must not refuse it: it says so and answers.
//
// What this pins is the reachable half of a misaim, an address that names
// no engine. The other half, an address that reaches an engine serving a
// DIFFERENT deployment, cannot be captured from here: it needs a process
// speaking /api/v1, and core has no such server outside package main's own
// test fixtures. #555 is what makes that shape refuse rather than write
// somewhere else, and pinning it wants a cell that can stand an engine up,
// which is a bigger thing than this file.
//
// # How the engine is faked, and why it is not faked
//
// It is not. service.AnnounceServing is the exact call `daemon` and the
// web host make to say they serve a deployment, and the CLI child probes
// for it through the same kernel lock a real deployment is found by. What
// this process does NOT do is open the journal or serve anything, which
// is fine: the fact under test is the announcement, and every command
// below is refused or answered before anything would have talked to an
// engine. The announcement is given back before this returns, so nothing
// after it sees a deployment that is still held.
func captureBesideAServingProcess(ctx context.Context, bin, cfgPath, root string) ([]string, error) {
	release, err := service.AnnounceServing(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("announcing a serving process over %s: %w", cfgPath, err)
	}

	// A header rather than a comment in this file, because the corpus is
	// read by whoever meets a red cell and two identical `check` lines
	// with different meanings would be unreadable. It is a pinned line
	// like any other, so the section cannot quietly move either.
	lines := []string{"# and the same binary with another process serving this deployment:"}
	for _, c := range []cliCase{
		{label: "a settings patch, refused", args: []string{"settings", "--config", cfgPath, "patch", "--timezone", "America/Toronto"}},
		{label: "a backup-set patch, refused", args: []string{"backup-set", "--config", cfgPath, "patch", "production/postgres-primary", "--stale-after", "48h"}},
		{label: "a usage mistake, which is still a usage mistake", args: []string{"settings", "--config", cfgPath, "patch"}},
		{label: "a read-only command, which is unaffected", args: []string{"check", "--config", cfgPath}},
	} {
		res, runErr := runCLI(ctx, bin, c.args, root, c.env...)
		if runErr != nil {
			_ = release()
			return nil, runErr
		}
		lines = append(lines, res...)
	}

	lines = append(lines, "# and with a route set to an address that is not this deployment's engine:")
	for _, c := range []cliCase{
		{label: "a settings patch through a route that answers nothing", args: []string{"settings", "--config", cfgPath, "patch", "--timezone", "America/Toronto"}, env: misaimedRoute()},
		{label: "a backup-set patch through the same route", args: []string{"backup-set", "--config", cfgPath, "patch", "production/postgres-primary", "--stale-after", "48h"}, env: misaimedRoute()},
		{label: "a read through the same route, which answers anyway", args: []string{"sources", "--config", cfgPath}, env: misaimedRoute()},
	} {
		res, runErr := runCLI(ctx, bin, c.args, root, c.env...)
		if runErr != nil {
			_ = release()
			return nil, runErr
		}
		lines = append(lines, res...)
	}

	if err := release(); err != nil {
		return nil, fmt.Errorf("giving back the serving announcement over %s: %w", cfgPath, err)
	}
	return lines, nil
}

// captureCLIRetention pins `retention --dry-run` against a second
// deployment, seeded at whole-day offsets from the current day under a
// daily-only chain.
//
// The reason this is not folded into the table above is in the package
// doc: rbm exposes no way to pin its clock, so a multi-tier
// chain's attribution genuinely depends on the calendar date the gate
// runs on. Rather than normalize the verdicts away and keep a cell that
// certifies nothing, this narrows the chain until the verdicts are the
// same on every day of the year: with a single seven-day daily tier and
// records at 1, 2, 40 and 400 whole days back, the first two are inside
// the window and the last two are outside it, no matter what today is.
func captureCLIRetention(ctx context.Context, bin, root string) (Cell, error) {
	dir := filepath.Join(root, "cli-retention")
	for _, sub := range []string{"backups", "exports"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return Cell{}, err
		}
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	midnight := time.Now().UTC().Truncate(24 * time.Hour).Add(9 * time.Hour)
	specs := []seedSpec{
		{name: "one-day-old.dump", state: lifecycle.Complete, discoveredAt: midnight.AddDate(0, 0, -1), content: "inside the daily window"},
		{name: "two-days-old.dump", state: lifecycle.Complete, discoveredAt: midnight.AddDate(0, 0, -2), content: "inside the daily window"},
		{name: "forty-days-old.dump", state: lifecycle.Complete, discoveredAt: midnight.AddDate(0, 0, -40), content: "outside the daily window"},
		{name: "four-hundred-days-old.dump", state: lifecycle.Complete, discoveredAt: midnight.AddDate(0, 0, -400), content: "outside the daily window"},
	}
	if _, _, err := seedDeployment(ctx, dir, dailyOnlyConfigYAML(dir), specs); err != nil {
		return Cell{}, err
	}

	lines, err := runCLI(ctx, bin, []string{"retention", "--dry-run", "--config", cfgPath}, root)
	if err != nil {
		return Cell{}, err
	}
	more, err := runCLI(ctx, bin, []string{"retention", "--config", cfgPath}, root)
	if err != nil {
		return Cell{}, err
	}
	lines = append(lines, more...)

	return Cell{
		Certifies: "FR-20 and FR-35: the retention preview an operator types still reads the same, KEEP/DELETE and tier attribution included, and still says out loud that it deletes nothing.",
		Rule:      RuleIdentical,
		Lines:     lines,
	}, nil
}

func dailyOnlyConfigYAML(root string) string {
	return fmt.Sprintf(`poll_interval: 15m

state:
  database: %s/state.db

sources:
  - id: production
    backup_sets:
      - id: postgres-primary
        remote:
          type: local
        remote_path: %s/exports
        local_path: %s/backups
        completion:
          strategy: stable
          stable_for: 10m
        stale_after: 30h

retention:
  timezone: UTC
  week_starts_on: monday
  protect_last_known_good: false
  tiers:
    - name: daily
      granularity: day
      keep: 7
`, root, root, root)
}

// misaimedRoute is the three route variables set to an address that is not
// this deployment's engine.
//
// Port 1 on the loopback interface, because a capture has to produce the
// same bytes on every machine that runs it and this is close to the only
// address that does. Binding it takes root, so nothing is ever listening,
// and a loopback connection to a closed port is refused at once rather
// than waiting out a timeout, which an address off this host (203.0.113.1,
// say) would do for the client's whole thirty seconds and would make this
// cell the slowest thing in the package.
//
// The credentials are placeholders and nothing ever reads them: the
// connection is refused before there is a server to present them to. They
// are set rather than left out because an operator who set an address set
// all three, and a case that left two of them unset would be pinning a
// half-configured route rather than a misaimed one.
func misaimedRoute() []string {
	return []string{
		"BACKUP_MANAGER_API_URL=http://127.0.0.1:1",
		"BACKUP_MANAGER_API_USERNAME=operator",
		"BACKUP_MANAGER_API_PASSWORD=placeholder-never-sent",
	}
}

// runCLI executes one invocation and renders it as corpus lines.
//
// Both streams and the exit status, every time. A command that starts
// printing a warning it did not print before has changed what an operator
// sees, and a cell that only looked at stdout would call that identical.
//
// The environment is built rather than inherited, and extra is the one way
// anything else gets into it. A developer with $BACKUP_MANAGER_API_URL
// exported for their own deployment would otherwise capture a different
// corpus from CI, and the difference would be that every refusal in this
// file quietly became a routed write aimed at their engine, which is the
// same hazard clearInheritedRouteSettings guards in package main. So the
// scrub stays, and a case that wants a route says so at the call site
// where a reader can see it.
func runCLI(ctx context.Context, bin string, args []string, root string, extra ...string) ([]string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = root
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "TZ=UTC"}, extra...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	code := 0
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if ok := asExitError(err, &exitErr); !ok {
			return nil, fmt.Errorf("running %v: %w", args, err)
		}
		code = exitErr.ExitCode()
	}

	label := cliecho.Binary + " " + strings.Join(redactArgs(args, root), " ")
	lines := []string{fmt.Sprintf("$ %s -> exit %d", label, code)}
	for _, l := range splitStream(stdout.String(), root) {
		lines = append(lines, "  out| "+l)
	}
	for _, l := range splitStream(stderr.String(), root) {
		lines = append(lines, "  err| "+l)
	}
	return lines, nil
}

// normalizeGoVersion is the second normalization this package does, and
// unlike the first it is not about tidiness.
//
// `rbm version` prints the Go runtime it was built with, and
// that is the machine's fact, not the product's. Pinning it into a
// checked-in corpus would make this gate red for every developer on a
// different patch release of Go and green only for whoever captured it,
// which is not a strict gate, it is a broken one. The rclone version on
// the line above it is deliberately NOT normalized: that one is pinned in
// go.mod, FR-2 has a whole procedure for changing it, and a corpus that
// noticed the change is doing its job.
//
// An exact replacement of runtime.Version(), not a pattern, for the same
// reason normalizeRoot is: a pattern that drifts can hide something.
func normalizeGoVersion(s string) string {
	return strings.ReplaceAll(s, runtime.Version(), "<GOVERSION>")
}

// eventTime matches the timestamp FR-23 puts at the front of every
// structured event line, and nothing else on it.
var eventTime = regexp.MustCompile(`"time":"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z"`)

// occurredAt matches the two spellings of a lifecycle transition's
// timestamp: the wall-clock column `activity` prints, and the RFC 3339
// field its --json form emits.
var occurredAt = regexp.MustCompile(`(?:[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2}|"occurred_at": "[^"]*")`)

// normalizeActivityTime is the fourth normalization, and it is the same
// argument the third one makes, one surface over (#598).
//
// `activity` prints when each transition happened, and that instant is set
// by the cycle this fixture runs a moment before capturing, so it is
// different on every run in the most literal sense. Pinning it would make
// this cell red for everybody and green for nobody.
//
// What survives is everything that is not the clock: the state entered,
// the backup set, the artifact, the caption, the ORDER (newest first, so a
// feed that started printing oldest-first fails), the row count under
// --limit, and the JSON field names. Only the value is replaced, and the
// field name stays, so a --json form that dropped occurred_at entirely
// still fails.
func normalizeActivityTime(s string) string {
	return occurredAt.ReplaceAllStringFunc(s, func(match string) string {
		if strings.HasPrefix(match, `"occurred_at"`) {
			return `"occurred_at": "<TIME>"`
		}
		return "<TIME>"
	})
}

// normalizeEventTime is the third normalization, and it arrived with the
// routed-write rows in captureBesideAServingProcess.
//
// A routed configuration write emits FR-23's two startup events on stdout
// before it reaches the engine (settings.go and backupset.go call
// logStartup once the route is open), so pinning what an operator sees
// from one means pinning a wall-clock instant, which is different on every
// run. That is the machine's fact in the most literal sense there is.
//
// The pattern is deliberately the whole field including its name, anchored
// to the RFC 3339 shape obs writes, rather than "a timestamp-looking
// thing". Everything else on those lines stays exactly as it was captured:
// the event names, the level, the version and commit a plain `go build`
// produces, and the embedded rclone version, which is pinned in go.mod and
// is a change this corpus is supposed to notice. A looser pattern is how a
// normalization stops being a normalization and starts being a place
// things hide.
func normalizeEventTime(s string) string {
	return eventTime.ReplaceAllString(s, `"time":"<TIME>"`)
}

func redactArgs(args []string, root string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		out = append(out, normalizeRoot(a, root))
	}
	return out
}

// splitStream turns a captured stream into corpus lines, keeping trailing
// whitespace visible rather than trimming it: column padding is part of
// what an operator sees, and a gate that trims it would not notice a
// widened column.
func splitStream(s, root string) []string {
	s = normalizeRoot(normalizeActivityTime(normalizeEventTime(normalizeGoVersion(s))), root)
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	return strings.Split(s, "\n")
}
