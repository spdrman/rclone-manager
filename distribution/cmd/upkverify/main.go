// Command upkverify holds a staged UGOS ugcli project to the canonical
// release, and is what apps/ugos/upk/build-upk.sh runs BEFORE `ugcli
// pack`.
//
// Before rather than after on purpose. Issue #83's rule is that a
// mismatch fails the build rather than shipping a divergent UGOS build,
// and a verifier that runs on the finished .upk cannot do that: `ugcli
// pack` writes an opaque artifact (a real vendor .upk on a UGOS Pro NAS
// is not a tar, a zip or anything else this can open), so the last moment
// the bytes are still readable is the staged tree. That is where the
// check goes, and it exits non-zero so the script stops.
//
// It reads container/release-manifest.json from a path given on the
// command line rather than reaching for it relative to its own source
// directory, because unlike the test suite this runs from wherever the
// operator happens to be standing.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/spdrman/rclone-manager/distribution/packaging"
)

func main() {
	stage := flag.String("stage", "", "the staged ugcli project directory (the one holding project.yaml)")
	arch := flag.String("arch", "", "the architecture to verify: the rootfs_<arch> directory inside the stage")
	manifest := flag.String("manifest", "", "path to container/release-manifest.json")
	flag.Parse()

	missing := ""
	switch {
	case *stage == "":
		missing = "-stage"
	case *arch == "":
		missing = "-arch"
	case *manifest == "":
		missing = "-manifest"
	}
	if missing != "" {
		fmt.Fprintf(os.Stderr, "upkverify: %s is required\n", missing)
		flag.Usage()
		os.Exit(2)
	}

	raw, err := os.ReadFile(*manifest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "upkverify: read %s: %v\n", *manifest, err)
		os.Exit(2)
	}
	m, err := packaging.ParseReleaseManifest(raw)
	if err != nil {
		fmt.Fprintf(os.Stderr, "upkverify: parse %s: %v\n", *manifest, err)
		os.Exit(2)
	}

	c, err := packaging.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "upkverify: load canonical.json: %v\n", err)
		os.Exit(2)
	}

	report := packaging.VerifyUPKStage(*stage, *arch, c, m)
	fmt.Print(report)
	if !report.OK() {
		os.Exit(1)
	}
}
