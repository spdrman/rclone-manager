package spk

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// assetFS holds every file this package ships verbatim inside the `.spk`:
// the lifecycle scripts, conf/privilege and conf/resource, the DSM
// desktop UI directory, and the starter configuration.
//
// They are real files rather than Go string literals so that what ships
// is reviewable as the shell and JSON it actually is, and so a reviewer
// reading conf/privilege is reading the exact bytes DSM will read.
//
//go:embed assets
var assetFS embed.FS

// LifecycleScripts returns the shipped lifecycle scripts by name.
//
// Only the stages LifecycleScriptNames lists are returned. scripts/ also
// ships common.sh, which is sourced by three of them and is not itself a
// stage; Verify scans it along with everything else in scripts/, but it
// is not a lifecycle script and counting it as one would make "does this
// package implement every stage?" unanswerable.
func LifecycleScripts() (map[string]string, error) {
	out := make(map[string]string, len(LifecycleScriptNames))
	for _, name := range LifecycleScriptNames {
		body, err := assetFS.ReadFile("assets/scripts/" + name)
		if err != nil {
			return nil, fmt.Errorf("read lifecycle script %s: %w", name, err)
		}
		out[name] = string(body)
	}
	return out, nil
}

// SharedScriptName is the shell library three lifecycle stages source.
// It is not itself a stage, so it is absent from LifecycleScriptNames,
// but it is where every package path those stages use is assigned.
const SharedScriptName = "common.sh"

// SharedScript returns that library's body.
func SharedScript() (string, error) {
	body, err := assetFS.ReadFile("assets/scripts/" + SharedScriptName)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", SharedScriptName, err)
	}
	return string(body), nil
}

// assetFiles walks one asset subdirectory and returns its files, keyed by
// the path they take inside the package, in sorted order.
func assetFiles(dir string) ([]assetFile, error) {
	var out []assetFile
	root := "assets/" + dir
	err := fs.WalkDir(assetFS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := assetFS.ReadFile(p)
		if err != nil {
			return err
		}
		out = append(out, assetFile{
			Name: path.Join(dir, strings.TrimPrefix(p, root+"/")),
			Body: body,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read %s assets: %w", dir, err)
	}
	return out, nil
}

type assetFile struct {
	Name string
	Body []byte
}
