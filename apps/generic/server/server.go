// Package server composes the generic Web host's whole HTTP surface: not
// yet implemented (issue #82/B4.1). See server_test.go for the intended
// contract. This stub exists only so this package compiles while that
// contract is pinned down by a failing test first.
package server

import (
	"io/fs"
	"net/http"

	"github.com/spdrman/rclone-manager/apps/common/auth/local"
	"github.com/spdrman/rclone-manager/apps/common/webhost"
)

// Config is everything New needs to build the composed handler.
type Config struct {
	Backend       webhost.BackupServiceClient
	Auth          *local.Service
	Gate          webhost.DestructiveGate
	BinaryVersion string
	Commit        string
	StaticFS      fs.FS
}

// New is not yet implemented.
func New(cfg Config) http.Handler {
	return http.NotFoundHandler()
}
