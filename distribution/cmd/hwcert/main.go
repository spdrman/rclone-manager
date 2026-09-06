// Command hwcert drives D2.1's resource certification on real UGREEN
// hardware (issue #89, EPIC D #177).
//
//	go run ./cmd/hwcert status                       # what is certified today
//	go run ./cmd/hwcert verify -record PATH          # hold one record to the procedure
//	go run ./cmd/hwcert record -samples PATH -out P  # aggregate a capture into a record
//
// Every threshold it applies comes out of
// docs/acceptance/ugos-resource-certification.md. There is no copy of any
// of them here, so a number cannot move without an edit somebody reviews.
//
// # Which architecture a record is for is not this command's opinion
//
// `record` stamps the record's probe.goarch from its own runtime.GOARCH,
// the architecture this binary was compiled for, and refuses a capture
// that claims a different one. That is what makes an amd64 pass unable to
// stand in for an arm64 one: the operator ships a binary built for the
// device (scripts/hwcert/build-probe.sh builds both), and the binary
// cannot be talked into recording the other architecture.
//
// # Nothing here touches a device or a credential
//
// Sampling is scripts/hwcert/measure-ugos.sh's job, on the NAS. This
// command reads what that wrote, and reads it as data.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/spdrman/rclone-manager/distribution/hwcert"
)

// version is stamped at build time by scripts/hwcert/build-probe.sh and
// lands in every record this binary writes, so a record can be traced to
// the harness that produced it.
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "status":
		status(os.Args[2:])
	case "verify":
		verify(os.Args[2:])
	case "record":
		record(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "hwcert: unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: hwcert <command> [flags]

  status   print one line per claimed architecture: certified, uncertified
           or failed. Exits non-zero only on a failure; an architecture
           with no evidence record is uncertified, which §68 says is an
           honest state to be in rather than a red gate.

  verify   hold one evidence record to the acceptance procedure's
           thresholds and print every metric, passing or failing.

  record   turn a capture written on the device into an evidence record,
           aggregating the raw samples. Run this on the device, with a
           binary built for the device's architecture.
`)
}

// ---------------------------------------------------------------- status

func status(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	root := fs.String("repo-root", "", "repository root (default: walk up to go.work)")
	_ = fs.Parse(args)

	dir := mustRoot(*root)
	proc, manifest := mustInputs(dir)

	sts, err := hwcert.Status(proc, dir, manifest)
	if err != nil {
		fail(err)
	}
	failures := 0
	for _, s := range sts {
		fmt.Printf("%-8s %-12s %s\n", s.Architecture, s.Status, s.Reason)
		if s.Status == hwcert.Failed {
			failures++
		}
	}
	if failures > 0 {
		fmt.Fprintf(os.Stderr, "\n%d architecture(s) have an evidence record that does not hold up. Read it with:\n\n    go run ./cmd/hwcert verify -record <path>\n", failures)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------- verify

func verify(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	root := fs.String("repo-root", "", "repository root (default: walk up to go.work)")
	path := fs.String("record", "", "evidence record to verify")
	arch := fs.String("architecture", "", "architecture to verify it as (default: the record's own)")
	_ = fs.Parse(args)

	if *path == "" {
		fail(fmt.Errorf("verify needs -record"))
	}
	dir := mustRoot(*root)
	proc, manifest := mustInputs(dir)

	rec, err := hwcert.ReadRecord(*path)
	if err != nil {
		fail(err)
	}
	want := *arch
	if want == "" {
		want = rec.Architecture
	}
	res, err := hwcert.Verify(proc, rec, want, manifest)
	if err != nil {
		fail(err)
	}
	fmt.Print(res.Report())
	if !res.Passed {
		os.Exit(1)
	}
}

// ---------------------------------------------------------------- record

func record(args []string) {
	fs := flag.NewFlagSet("record", flag.ExitOnError)
	samples := fs.String("samples", "", "capture written by scripts/hwcert/measure-ugos.sh")
	out := fs.String("out", "", "where to write the evidence record")
	_ = fs.Parse(args)

	if *samples == "" || *out == "" {
		fail(fmt.Errorf("record needs -samples and -out"))
	}

	c, err := hwcert.ReadCapture(*samples)
	if err != nil {
		fail(err)
	}

	// The capture may say which architecture it is, and it is not
	// believed. This binary's own GOARCH is the answer, because it is
	// the one nobody on the day can type in. A capture that disagrees is
	// a probe shipped to the wrong device, which is worth stopping
	// rather than recording.
	if c.Probe.GOARCH != "" && c.Probe.GOARCH != runtime.GOARCH {
		fail(fmt.Errorf("the capture says probe.goarch is %q and this binary was built for %s; ship the %s probe to a %s device, or the %s one to this device",
			c.Probe.GOARCH, runtime.GOARCH, c.Probe.GOARCH, c.Probe.GOARCH, runtime.GOARCH))
	}
	c.Probe = hwcert.Probe{GOARCH: runtime.GOARCH, Version: version}

	rec, err := hwcert.BuildRecord(*c)
	if err != nil {
		fail(err)
	}
	if err := rec.WriteFile(*out); err != nil {
		fail(err)
	}
	fmt.Printf("wrote %s: %s evidence for %s %s, firmware %s\n",
		*out, rec.Architecture, rec.Device.Vendor, rec.Device.Model, rec.Device.FirmwareVersion)
}

// ---------------------------------------------------------------------

func mustInputs(root string) (*hwcert.Procedure, *hwcert.ReleaseManifest) {
	proc, err := hwcert.ParseProcedure(filepath.Join(root, hwcert.ProcedurePath))
	if err != nil {
		fail(err)
	}
	manifest, err := hwcert.ReadReleaseManifest(filepath.Join(root, "container", "release-manifest.json"))
	if err != nil {
		fail(err)
	}
	return proc, manifest
}

func mustRoot(given string) string {
	if given != "" {
		return given
	}
	root, err := repoRoot()
	if err != nil {
		fail(err)
	}
	return root
}

// repoRoot walks up from the working directory to the checkout root,
// identified by go.work, so this behaves the same from distribution/ and
// from the repository root. Same shape as cmd/provenance's, for the same
// reason.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.work above the working directory, so the repository root cannot be located; pass -repo-root")
		}
		dir = parent
	}
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "hwcert: %v\n", err)
	os.Exit(1)
}
