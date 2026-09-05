// The command's own edges: the default bounds, the argument split, and the
// exit status.
//
// It holds nothing that decides anything. The bound arithmetic is in
// tracker.go so it can be proved against a synthetic clock, and the process
// handling is in run.go; what is left here is the part that only makes sense
// as a program, which is where a caller's arguments stop being gotestwatch's
// and start being `go test`'s.
//
// That split is why "--" is optional. The gate's own invocations pass
// nothing but package paths and flags meant for `go test`, so the common
// case has to work with no separator at all, and a leading "--" is still
// accepted so a package path beginning with a dash is never ambiguous.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"
)

// defaultBounds mirrors tests/crashmatrix's defaultHarnessBounds (issue
// #247) deliberately: stepFloor is the same 45s the harness's own floor
// uses, for the same reason that comment gives (a real per-operation
// budget on a quiet machine, not a guess), and stepFactor 12 is the same
// already-justified multiplier. overallFloor/overallFactor are widened
// relative to the harness's own (4m/40 there too, kept identical) because
// this tracker bounds a whole `go test` invocation across every package
// and every -count repetition passed to it, not one harness subprocess;
// the derivation still widens both from real measurements, so the floor
// only matters before the run has measured itself once.
var defaultBounds = bounds{
	stepFloor:     45 * time.Second,
	stepFactor:    12,
	overallFloor:  4 * time.Minute,
	overallFactor: 40,
}

// parseArgs splits gotestwatch's own arguments from the ones destined for
// `go test`. With no "--" present, every argument is passed straight
// through to `go test` under defaultBounds, which is the form
// scripts/ci-local.sh uses. A leading "--" is also accepted with nothing
// before it, so a package path that happens to start with a dash is never
// ambiguous. Flags before "--" are parsed as gotestwatch's own.
func parseArgs(args []string) (bounds, []string, error) {
	cut := -1
	for i, a := range args {
		if a == "--" {
			cut = i
			break
		}
	}
	if cut < 0 {
		return defaultBounds, args, nil
	}

	fs := flag.NewFlagSet("gotestwatch", flag.ContinueOnError)
	stepFloor := fs.Duration("step-floor", defaultBounds.stepFloor, "no-progress window floor, used until the run has measured one gap")
	stepFactor := fs.Float64("step-factor", defaultBounds.stepFactor, "no-progress window = stepFactor x the slowest gap measured so far")
	overallFloor := fs.Duration("overall-floor", defaultBounds.overallFloor, "overall livelock cap floor")
	overallFactor := fs.Float64("overall-factor", defaultBounds.overallFactor, "overall livelock cap = overallFactor x the slowest gap measured so far")
	if err := fs.Parse(args[:cut]); err != nil {
		return bounds{}, nil, err
	}

	return bounds{
		stepFloor:     *stepFloor,
		stepFactor:    *stepFactor,
		overallFloor:  *overallFloor,
		overallFactor: *overallFactor,
	}, args[cut+1:], nil
}

func main() {
	b, goArgs, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "gotestwatch:", err)
		os.Exit(2)
	}
	if len(goArgs) == 0 {
		fmt.Fprintln(os.Stderr, "gotestwatch: no arguments to pass to `go test` (usage: gotestwatch [-step-floor=... -- ] <go test args>)")
		os.Exit(2)
	}

	res, err := Run(Options{
		Args:   goArgs,
		Bounds: b,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "gotestwatch:", err)
		os.Exit(1)
	}
	if res.Trip != nil {
		fmt.Fprintln(os.Stderr, "\ngotestwatch: killed the run because "+res.Trip.String())
		// 124 is the timeout(1) convention: this exit code is
		// gotestwatch's own verdict, distinct from any exit code `go
		// test` itself could have produced.
		os.Exit(124)
	}
	os.Exit(res.ExitCode)
}
