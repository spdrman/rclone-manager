// Command spkctl builds and conformance-checks the Synology DSM package
// (issue #85/B4.4).
//
// It compiles nothing. `build` wraps release binaries somebody else
// produced, and `verify` re-derives their SHA-256 out of a finished .spk
// and compares it against container/release-manifest.json - which is what
// docs/EPIC-B-multi-nas.md §3.7 means by the SPK carrying "the exact same
// core binary digest".
//
// Where the release binaries come from: they are the two executables
// inside the canonical OCI image 4.1 builds. Extract them with
//
//	cid=$(docker create --platform linux/amd64 backup-manager:<version> /backup-manager version)
//	docker cp "${cid}:/backup-manager"     ./release/amd64/backup-manager
//	docker cp "${cid}:/backup-manager-web" ./release/amd64/backup-manager-web
//	docker rm "${cid}"
//
// which is the same extraction scripts/release/record-release-hashes.sh
// does to produce the manifest in the first place. Never rebuild them
// here: a rebuild is precisely what verify exists to detect.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/spdrman/rclone-manager/apps/synology/spk"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "build":
		return cmdBuild(args[1:])
	case "verify":
		return cmdVerify(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "spkctl: unknown command %q\n\n", args[0])
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: spkctl <command> [flags]

commands:
  build    wrap already-built release binaries in a .spk
  verify   check a .spk against a release manifest

build flags:
  --arch GOARCH        amd64 or arm64 (required)
  --version VERSION    INFO's version, e.g. 1.0.0-1 (required)
  --binaries DIR       directory holding backup-manager and
                        backup-manager-web for --arch (required)
  --out DIR            where to write the .spk (default .)

verify flags:
  --spk PATH        the package to check (required)
  --manifest PATH   release manifest to check it against
                     (default container/release-manifest.json)
`)
}

func cmdBuild(args []string) int {
	fset := flag.NewFlagSet("build", flag.ContinueOnError)
	arch := fset.String("arch", "", "release architecture (amd64 or arm64)")
	version := fset.String("version", "", "package version written into INFO")
	binaries := fset.String("binaries", "", "directory holding the already-built release binaries")
	out := fset.String("out", ".", "directory to write the .spk into")
	if err := fset.Parse(args); err != nil {
		return 2
	}

	path, err := spk.Build(spk.BuildOptions{
		GOARCH:      *arch,
		Version:     *version,
		BinariesDir: *binaries,
		OutDir:      *out,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "spkctl build:", err)
		return 1
	}
	fmt.Println(path)
	return 0
}

func cmdVerify(args []string) int {
	fset := flag.NewFlagSet("verify", flag.ContinueOnError)
	spkPath := fset.String("spk", "", "the .spk to check")
	manifestPath := fset.String("manifest", "container/release-manifest.json", "release manifest to check against")
	if err := fset.Parse(args); err != nil {
		return 2
	}
	if *spkPath == "" {
		fmt.Fprintln(os.Stderr, "spkctl verify: --spk is required")
		return 2
	}

	manifest, err := spk.LoadReleaseManifest(*manifestPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "spkctl verify:", err)
		return 1
	}
	report, err := spk.Verify(*spkPath, manifest)
	if err != nil {
		fmt.Fprintln(os.Stderr, "spkctl verify:", err)
		return 1
	}

	fmt.Print(report)
	if !report.OK() {
		fmt.Fprintf(os.Stderr, "\nspkctl verify: %d of %d checks failed\n",
			len(report.Failures()), len(report.Checks))
		return 1
	}
	return 0
}
