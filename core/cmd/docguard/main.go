package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"go/ast"
	"go/doc"
	"go/parser"
	"go/scanner"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// This file is the whole of docguard: a reader for two facts about Go
// source that this repository's gate had no way to see, and issue #526 is
// what a blind spot there costs.
//
// # Why go/doc rather than reading the file
//
// A comment adjacent to `package` IS the package doc, and go/doc
// concatenates every one of them across a package in sorted file order.
// Six documentation lanes put a per-file opener there, so
// `go doc ./core/service` opened with "This file is the operator's
// activity feed", which is activity.go describing itself, and the real
// overview was somewhere down the list. Nothing noticed, because nothing
// was reading the assembled overview: `go build`, `go vet` and every
// linter .golangci.yml enables are all indifferent to where a comment sits.
//
// So this asks go/doc for Package.Doc, the same value `go doc` prints, and
// never shapes text of its own. That distinction is not pedantry. One lane
// in that campaign measured header length with
// `awk '/^(func|type|var|const) /{exit}'` and the awk exited on the lane's
// own prose, where the word "type" had wrapped to column 0, inventing a
// 283-to-9-line "finding" out of a paragraph break. A checker that parses
// what the compiler parses cannot be fooled by its own subject matter.
//
// # Why the token mode lives in the same binary
//
// The fix for #526 moves comments and nothing else, and "nothing else" has
// to be provable rather than asserted. `tokens` prints the go/scanner
// stream with comments dropped, so two revisions of a file that differ
// only in comments produce identical output.
//
// On its own that would be too weak in one specific way: a `//go:build`
// line is a comment to the scanner and a build constraint to the
// toolchain, so a header moved across one changes which platforms compile
// the file while the token stream stays byte-identical. core/service has
// two such files (lock_unix.go, lock_other.go) and they are exactly the
// ones #526's fix had to reach past. So every `//go:` directive and
// `// +build` line is printed too, with whether it sits before or after
// the package clause, which is the half of a build constraint's meaning
// that position carries.

func main() {
	full := flag.Bool("full", false, "print each package's assembled doc text instead of one digest line per package")
	flag.Usage = usage
	flag.Parse()

	args := flag.Args()
	if len(args) < 2 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "packages":
		if err := packages(args[1], *full); err != nil {
			fmt.Fprintln(os.Stderr, "docguard:", err)
			os.Exit(2)
		}
	case "tokens":
		for _, path := range args[1:] {
			if err := tokens(path); err != nil {
				fmt.Fprintln(os.Stderr, "docguard:", err)
				os.Exit(2)
			}
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: docguard [-full] packages <root>
       docguard tokens <file>...

packages walks every Go package under <root> and prints one line per
package: the path, the package name, a sha256 of the doc go/doc assembles
for it, and every file carrying a comment adjacent to `+"`package`"+`. With
-full it prints the doc text itself, delimited, for diffing two trees.

tokens prints one file's go/scanner token stream with comments dropped,
then every //go: directive and // +build line with its side of the package
clause. Two revisions that differ only in comments print the same thing;
a build constraint that changed side does not.
`)
}

// skipDir is the set of directory names that hold no package this
// repository documents: version control, dependency trees, build output,
// and the testdata fixtures whose whole point is to be broken or minimal.
func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", "dist", "testdata", ".venv", ".ci-local-gate-test":
		return true
	}
	return false
}

func packages(root string, full bool) error {
	root = filepath.Clean(root)
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if p != root && skipDir(d.Name()) {
			return fs.SkipDir
		}
		names, err := goFiles(p)
		if err != nil {
			return err
		}
		if len(names) > 0 {
			dirs = append(dirs, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	sort.Strings(dirs)

	for _, dir := range dirs {
		rel, err := filepath.Rel(root, dir)
		if err != nil {
			return err
		}
		names, err := goFiles(dir)
		if err != nil {
			return err
		}
		// Sorted, because that is the order go/doc concatenates package
		// comments in and the order is the whole reason a promoted opener
		// can lead the real overview rather than trail it.
		sort.Strings(names)

		fset := token.NewFileSet()
		byPkg := map[string][]*ast.File{}
		carriers := map[string][]string{}
		for _, n := range names {
			f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, parser.ParseComments)
			if err != nil {
				return err
			}
			name := f.Name.Name
			byPkg[name] = append(byPkg[name], f)
			if f.Doc != nil {
				carriers[name] = append(carriers[name], n)
			}
		}

		var pkgNames []string
		for name := range byPkg {
			pkgNames = append(pkgNames, name)
		}
		sort.Strings(pkgNames)

		for _, name := range pkgNames {
			// doc.AllDecls is deliberate: the package overview is the same
			// text whether or not a reader asked for unexported
			// declarations, and this must not depend on that flag.
			d, err := doc.NewFromFiles(fset, byPkg[name], rel, doc.AllDecls)
			if err != nil {
				return err
			}
			if full {
				fmt.Printf("=== %s %s\n", rel, name)
				fmt.Print(d.Doc)
				fmt.Printf("=== end %s %s\n", rel, name)
				continue
			}
			sum := sha256.Sum256([]byte(d.Doc))
			held := carriers[name]
			if len(held) == 0 {
				held = []string{"-"}
			}
			fmt.Printf("%s\t%s\t%s\t%s\n", rel, name, hex.EncodeToString(sum[:]), strings.Join(held, ","))
		}
	}
	return nil
}

// goFiles lists the non-test Go files in one directory. _test.go files are
// left out because go/doc leaves them out: they cannot contribute to a
// package overview, so an opener in one is already a file comment and
// #526's rule does not reach it.
func goFiles(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			names = append(names, n)
		}
	}
	return names, nil
}

func tokens(path string) error {
	src, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	fset := token.NewFileSet()
	// Parsing rather than scanning straight, because the package clause's
	// position is what gives a build constraint its meaning, and only a
	// parse knows where that is. ParseComments so the directives survive.
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		return err
	}
	pkgLine := fset.Position(f.Package).Line

	// The token stream, comments dropped. go/scanner in its default mode
	// does not emit them at all, so this is the compiler's own view of the
	// file with the prose taken out. The scanner rather than the ast,
	// because the ast would elide punctuation and this has to be a
	// statement about the file, not about its shape.
	var s scanner.Scanner
	sfset := token.NewFileSet()
	file := sfset.AddFile(path, sfset.Base(), len(src))
	var scanErr error
	s.Init(file, src, func(pos token.Position, msg string) {
		if scanErr == nil {
			scanErr = fmt.Errorf("%s: %s", pos, msg)
		}
	}, 0)
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if lit == "" {
			fmt.Printf("T %s\n", tok)
		} else {
			fmt.Printf("T %s %s\n", tok, lit)
		}
	}
	if scanErr != nil {
		return scanErr
	}

	for _, group := range f.Comments {
		for _, c := range group.List {
			text := c.Text
			body := strings.TrimPrefix(text, "//")
			if !strings.HasPrefix(body, "go:") && !strings.HasPrefix(strings.TrimSpace(body), "+build") {
				continue
			}
			side := "post-package"
			if fset.Position(c.Pos()).Line < pkgLine {
				side = "pre-package"
			}
			fmt.Printf("D %s %s\n", side, text)
		}
	}
	return nil
}
