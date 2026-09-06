// Package webui embeds ui/shared's built static bundle into the
// backup-manager-web binary via go:embed.
//
// go:embed cannot reach outside its own package's module tree with a
// `..` path element, so this package cannot embed ui/shared/dist
// directly (three directories up, outside apps/generic's own module).
// Instead, dist/ here is a local staging directory: it is checked into
// git with a placeholder index.html (see dist/index.html's own comment)
// so a plain `go build`/`go test` always succeeds, and
// container/Dockerfile's frontend-build stage overwrites it with
// ui/shared's real `npm run build` output before compiling this
// module's binary - see that Dockerfile stage and
// docs/deployment.md's "The generic Web host" section.
package webui

import "embed"

// Assets is ui/shared's built static bundle (or, outside a real image
// build, the placeholder in dist/index.html).
//
//go:embed all:dist
var Assets embed.FS
