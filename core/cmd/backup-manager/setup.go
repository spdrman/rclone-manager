package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/config"
	"github.com/spdrman/rclone-manager/core/internal/obs"
	"github.com/spdrman/rclone-manager/core/internal/transport"
	"github.com/spdrman/rclone-manager/core/internal/transport/rclone"
	"github.com/spdrman/rclone-manager/core/service"
)

// defaultConfigPath matches container/compose.yaml's mount point and
// docs/deployment.md's documented layout: /etc/backup-manager/config.yaml.
const defaultConfigPath = "/etc/backup-manager/config.yaml"

// newFlagSet builds a flag.FlagSet every subcommand but `version` shares:
// a name (for its own usage/error output) and the one flag they all take,
// --config. flag.ContinueOnError, not the package default, is deliberate:
// it lets this command return its own exit code and still run every
// deferred cleanup between here and main, rather than flag.Parse calling
// os.Exit out from under a subcommand that has already opened a journal.
func newFlagSet(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	cfgPath := fs.String("config", defaultConfigPath, "path to the manager's YAML config file")
	return fs, cfgPath
}

// openService loads and validates configPath, opens its state journal
// (both via core/service.OpenConfigAndJournal — see that function's own
// doc for why this no longer reimplements that sequence itself), and
// builds an internal/app.Service ready for whichever use case the calling
// subcommand needs. withTransport controls whether the service is given a
// real transport.Transport (internal/transport/rclone.Adapter): every
// subcommand that can reach a remote (run, daemon, fetch, reconcile) needs
// one; every purely local one (check, status, sources, artifacts,
// retention, validate) does not, and leaves Service.Transport nil rather
// than pay for constructing an adapter it will never call.
//
// The returned cleanup func closes the journal; callers should always
// `defer cleanup()` immediately.
func openService(ctx context.Context, configPath string, withTransport bool) (*app.Service, *config.Config, func(), error) {
	cfg, journal, err := service.OpenConfigAndJournal(ctx, configPath)
	if err != nil {
		return nil, nil, func() {}, err
	}
	cleanup := func() {
		if err := journal.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "backup-manager: closing state database: %v\n", err)
		}
	}

	var tr transport.Transport
	if withTransport {
		tr = rclone.New()
	}

	svc := app.New(cfg, journal, tr, logger())
	return svc, cfg, cleanup, nil
}

// logger builds the FR-23 structured-observability sink every Service
// this binary constructs shares: newline-delimited JSON on stdout, so the
// process's own supervisor (systemd, a container runtime) owns rotation
// and shipping, exactly as internal/obs's package doc describes.
func logger() *obs.Logger {
	return obs.New(os.Stdout, obs.LevelInfo)
}

// logStartup emits FR-23's two mandatory startup log lines (binary
// version/commit/Go version, and the embedded rclone version) once, right
// after a Service exists. Every subcommand that can mutate anything
// (run, daemon, fetch, reconcile, validate) calls this; the purely
// read-only ones (status, sources, artifacts, retention, check) do not,
// since FR-23's "startup" event is about a processing run starting, not
// about a query being answered.
func logStartup(ctx context.Context, l *obs.Logger, info app.VersionInfo) {
	l.Startup(ctx, info.BinaryVersion, info.Commit, info.GoVersion)
	l.RcloneVersion(ctx, info.RcloneVersion)
}

// fail prints err to stderr in a consistent shape and returns the exit
// code every subcommand's own failure path returns.
func fail(err error) int {
	fmt.Fprintln(os.Stderr, "backup-manager:", err)
	return 1
}

// usageError prints a usage complaint straight to stderr (flag.Parse
// already prints its own error under flag.ContinueOnError, so this is for
// the errors this package's own subcommands catch afterward, like a
// missing required flag or a wrong argument count) and returns the
// argument-error exit code.
func usageError(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "backup-manager: "+format+"\n", args...)
	return 2
}
