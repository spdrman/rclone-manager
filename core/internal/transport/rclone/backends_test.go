package rclone

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/rclone/rclone/fs"
)

// TestRegisteredBackendsExactSet is the enforcement FR-4 asks for: the set
// of rclone backends this binary registers at runtime must match
// ExpectedBackends() exactly, not "at least" or "at most". A new blank
// import anywhere in this package that registers another backend, whether
// directly or transitively the way crypt currently arrives through
// fs/operations, changes fs.Registry and fails this test. That turns a
// silent widening of the configuration/dependency surface into a build
// failure someone has to look at and either revert or consciously accept
// in backends.go.
func TestRegisteredBackendsExactSet(t *testing.T) {
	got := RegisteredBackendNames()
	want := ExpectedBackends()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("registered rclone backends changed:\n  got:  %v\n  want: %v\n"+
			"if this widening is intentional, add the new name (with a reason, "+
			"if it's transitive) to RequiredBackends or AcceptedTransitiveBackends "+
			"in backends.go; if it's not intentional, find and remove whatever "+
			"import pulled it in",
			got, want)
	}
}

// TestAcceptedTransitiveBackendsAreDocumented makes sure nothing gets added
// to AcceptedTransitiveBackends without a real reason. An empty or
// whitespace-only entry would let a future backend widen the registered set
// silently, which is exactly what TestRegisteredBackendsExactSet exists to
// prevent.
func TestAcceptedTransitiveBackendsAreDocumented(t *testing.T) {
	for name, reason := range AcceptedTransitiveBackends {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("AcceptedTransitiveBackends[%q] has no reason recorded", name)
		}
	}
}

// TestRequiredBackendsResolve checks that every backend FR-4 actually asks
// for resolves through rclone's own lookup, the same fs.Find call fsFor
// uses to turn a configured source type into a backend. This is a
// registration check, not a behavioral one: it does not exercise fsFor or
// build a working Fs, it only confirms the name is known to the registry.
func TestRequiredBackendsResolve(t *testing.T) {
	for _, name := range RequiredBackends {
		if _, err := fs.Find(name); err != nil {
			t.Errorf("required backend %q did not resolve: %v", name, err)
		}
	}
}

// repoRoot is where TestNoCloudProviderSDKIsImportedDirectly walks from,
// relative to this package's own directory, which is where `go test` runs a
// package's tests from.
const repoRoot = "../../../.."

// cloudSDKImportPrefixes are the provider SDK module paths FR-28 says must
// not enter this tree. Prefixes, so a submodule (aws-sdk-go-v2/service/s3)
// is caught by its parent's entry.
var cloudSDKImportPrefixes = []string{
	"github.com/aws/aws-sdk-go",
	"github.com/aws/smithy-go",
	"github.com/Azure/azure-sdk-for-go",
	"github.com/Azure/azure-storage-blob-go",
	"cloud.google.com/go/storage",
	"google.golang.org/api/storage",
	"github.com/minio/minio-go",
}

// TestNoCloudProviderSDKIsImportedDirectly is FR-28's "no AWS SDK, no Azure
// SDK, no GCS SDK enters the tree" made mechanical, and it is worth being
// precise about what it can and cannot claim, because the honest reading is
// narrower than the sentence sounds.
//
// rclone's own s3 backend is built on aws-sdk-go-v2. Registering that
// backend therefore DOES pull the AWS SDK into go.mod and go.sum as an
// indirect dependency, and no test can make that untrue. The landing PR
// records the measured cost of it. What FR-28 is actually protecting
// against is the thing that would still be a mistake even so: a SECOND S3
// implementation in this repository, written against the SDK directly,
// living outside the FR-3 transport boundary and upgrading on its own
// schedule. That is what this test forbids, by walking every Go file this
// repository owns and failing on a direct import of a provider SDK from
// any of them.
//
// The rule has no exception for this package. transport/rclone is the only
// package allowed to import RCLONE; it is not allowed to reach past rclone
// to the SDK underneath it, because doing so would mean this repository had
// opinions about an AWS API surface that rclone's upgrade contract does not
// cover.
func TestNoCloudProviderSDKIsImportedDirectly(t *testing.T) {
	scanned := 0
	err := filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "vendor", "dist", "build", ".run", ".claude":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, perr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if perr != nil {
			// A file this repository owns that does not parse is a
			// problem, but it is not THIS test's problem to report, and
			// failing here would make an unrelated syntax error look
			// like an SDK violation.
			return nil
		}
		scanned++
		for _, spec := range file.Imports {
			imported, uerr := strconv.Unquote(spec.Path.Value)
			if uerr != nil {
				continue
			}
			for _, banned := range cloudSDKImportPrefixes {
				if namesBannedModule(imported, banned) {
					t.Errorf("%s imports %q directly.\n"+
						"FR-28: rclone's s3 backend is the entire S3 implementation this product has. A second one, "+
						"written against a provider SDK directly, sits outside the FR-3 transport boundary and outside "+
						"the rclone upgrade contract. If this is deliberate it needs its own architecture decision, not "+
						"an import line",
						path, imported)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", repoRoot, err)
	}
	// A walk that found nothing would pass this test while proving
	// nothing, which is the failure mode a negative check is most prone
	// to. This repository has well over a thousand Go files; a number
	// this low means the walk broke, not that the tree shrank.
	if scanned < 200 {
		t.Fatalf("only %d Go files were scanned from %s; the walk is not reaching the tree it is meant to check, so its silence is not evidence", scanned, repoRoot)
	}
}

// moduleMajorVersionSuffix matches the two spellings Go gives a module's
// major version when it is part of the module PATH: the "/v2" element the
// module system defines, and the "-v2" the AWS SDK happens to use in its
// repository name. Both have to be understood here, and finding that out
// is the whole reason the mutation check on this guard was worth running:
// the first version of namesBannedModule required a "/" after the banned
// prefix, so "github.com/aws/aws-sdk-go" did not match
// "github.com/aws/aws-sdk-go-v2/service/s3", and a planted direct import
// of the exact SDK this change pulls in walked straight past a green test.
var moduleMajorVersionSuffix = regexp.MustCompile(`^[-/]v[0-9]+`)

// namesBannedModule reports whether imported is banned itself, a package
// inside it, or a major-version variant of either.
//
// It is deliberately not a bare strings.HasPrefix. That would also match a
// module whose name merely begins with a banned one (an unrelated
// "github.com/aws/aws-sdk-golang-helper"), and a guard that fires on
// something legitimate gets weakened by whoever hits it next, which is a
// worse outcome than the false negative it was trying to avoid.
func namesBannedModule(imported, banned string) bool {
	rest, ok := strings.CutPrefix(imported, banned)
	switch {
	case !ok:
		return false
	case rest == "":
		return true
	case strings.HasPrefix(rest, "/"):
		return true
	}
	if v := moduleMajorVersionSuffix.FindString(rest); v != "" {
		remainder := rest[len(v):]
		return remainder == "" || strings.HasPrefix(remainder, "/")
	}
	return false
}

// TestNamesBannedModule is the positive control for the walk above. The
// walk can only ever be mutation-checked against a module that actually
// resolves, which is the AWS SDK and nothing else here: planting an import
// of the Azure or GCS SDK fails the BUILD, so it proves the compiler works
// rather than proving this guard does. So the matcher gets exercised
// directly instead, on every shape it is meant to catch and on the ones it
// must not.
func TestNamesBannedModule(t *testing.T) {
	for _, tc := range []struct {
		imported string
		banned   string
		want     bool
	}{
		{"github.com/aws/aws-sdk-go-v2/service/s3", "github.com/aws/aws-sdk-go", true},
		{"github.com/aws/aws-sdk-go-v2", "github.com/aws/aws-sdk-go", true},
		{"github.com/aws/aws-sdk-go/aws/session", "github.com/aws/aws-sdk-go", true},
		{"github.com/aws/aws-sdk-go", "github.com/aws/aws-sdk-go", true},
		{"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob", "github.com/Azure/azure-sdk-for-go", true},
		{"cloud.google.com/go/storage", "cloud.google.com/go/storage", true},
		{"github.com/minio/minio-go/v7", "github.com/minio/minio-go", true},
		{"github.com/aws/smithy-go/transport/http", "github.com/aws/smithy-go", true},

		// Not banned: a different module whose name merely starts with a
		// banned one. A guard that fires on something legitimate is the
		// one that gets weakened by whoever hits it next.
		{"github.com/aws/aws-sdk-golang-helper", "github.com/aws/aws-sdk-go", false},
		{"github.com/aws/smithy-golike", "github.com/aws/smithy-go", false},
		{"cloud.google.com/go/storagetransfer", "cloud.google.com/go/storage", false},
		{"github.com/rclone/rclone/backend/s3", "github.com/aws/aws-sdk-go", false},
	} {
		if got := namesBannedModule(tc.imported, tc.banned); got != tc.want {
			t.Errorf("namesBannedModule(%q, %q) = %v, want %v", tc.imported, tc.banned, got, tc.want)
		}
	}
}
