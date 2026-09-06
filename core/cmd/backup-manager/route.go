package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spdrman/rclone-manager/core/apicontract"
	"github.com/spdrman/rclone-manager/core/internal/apiclient"
	"github.com/spdrman/rclone-manager/core/service"
)

// Issue #543, Phase 2 of #536: where a mutating backup-set command's work
// actually goes, and the one seam both destinations arrive through.
//
// # Why this is an interface and not a branch inside each verb
//
// There are two request vocabularies for the same three operations.
// core/service speaks in service.CreateBackupSetRequest, with a
// time.Duration StableFor and an Actor the caller supplies.
// core/apicontract speaks in apicontract.CreateBackupSetRequest, which
// embeds BackupSetSpec, spells the same window stable_for_seconds, and
// takes the actor from the session rather than from the body. Nothing
// converted between them anywhere until this file.
//
// Left to themselves, create, patch and remove would each grow their own
// conversion, which is five copies of one mapping (counting the two create
// paths), and five copies of a mapping is how two modes drift apart while
// every test stays green. #543's own acceptance criterion - that a refusal
// is the same on both routes - would then be held up by nothing except
// somebody remembering to write a test for it.
//
// So the seam is an interface in core/service's vocabulary. That direction
// is chosen rather than arbitrary: it makes the DIRECT route the identity
// implementation, so *service.BackupService satisfies backupSetRoute as it
// already stands, with no wrapper and no adapter of its own to go wrong.
// Exactly one type does any translating (engineRoute, engineroute.go), and
// every field the two vocabularies spell differently is visible in one
// file, next to every other one.
//
// # What "engine-attached" now means, and what it still does not
//
// #542 decided the mode; it could not carry one of them out, because this
// build had no route. It has one now for `backup-set create`, `patch` and
// `remove` and for `settings patch`, and only when an operator has said
// where the engine is. Two shapes still refuse, and both are refusals
// rather than gaps:
//
// A `backup-manager daemon` announces that it serves this deployment and
// serves no HTTP at all (core/service's liveengine.go says so in as many
// words). There is nothing to route to, so the change is refused exactly
// as it was before.
//
// A serving process this command has not been told how to reach is the
// same case from the other side. Guessing an address would be worse than
// refusing: the engine's own listener publishes no port on the deployment
// this product ships (container/compose.yaml), the published port belongs
// to the UI host in front of it, and a CLI that guessed wrong would either
// fail obscurely or, far worse, reach a DIFFERENT deployment's engine and
// write there. So the address is told, never inferred.
//
// # And why the address and the credentials come from the environment
//
// Not from flags. `backup-manager --help` is pinned line for line by
// core/tests/compat under FR-35's fourth clause, and a password on a
// command line is in every process listing on the host and in the shell
// history of whoever typed it. Not from a file either: this binary writes
// no credential anywhere and reads none (apiclient's doc explains why
// /data/state/local-auth.json is deliberately not read), so an environment
// this process was started with is the one place left that a script can
// fill in and a terminal does not keep.
//
// The credentials are the local administrator's, the same pair the Web UI
// takes. A CLI-only credential would be a second thing deciding who may
// act on one deployment, which is the defect #536 exists to remove.
//
// Environment rather than flags, and rather than a block in config.yaml.
// A flag would be operator-visible surface on six commands' usage text at
// once, and that text is pinned both by core/tests/compat and by
// spdrman/rclone-manager-tests. The address also belongs to the HOST a
// command is typed on rather than to the deployment: the same config.yaml
// is read from inside the container, where the engine is on loopback, and
// from a NAS shell, where it is a published port, so one field could not
// be right in both places.
//
// These three names are declared here and nowhere else. They were briefly
// declared twice, once for the write path and once for the read path, with
// nothing comparing the two, which is the drift this package spends a
// contract test preventing one layer down.

const (
	// apiURLEnv names the engine's address: the scheme, host and port
	// only, without /api/v1, which the client appends itself. Either the
	// engine's own listener from inside its container
	// (http://127.0.0.1:8080) or the published UI port from a host
	// (http://nas.local:8080), which proxies to the same engine.
	//
	// Setting it is what makes engine-attached mode carryable. Leaving it
	// unset is not an error: a host with nothing running never reaches the
	// question, and one with an engine gets the refusal it got before.
	apiURLEnv = "BACKUP_MANAGER_API_URL"

	// apiUsernameEnv and apiPasswordEnv are the local administrator's, the
	// same pair the Web UI's login page takes. They are read into memory,
	// used to sign in, and written nowhere.
	apiUsernameEnv = "BACKUP_MANAGER_API_USERNAME"
	apiPasswordEnv = "BACKUP_MANAGER_API_PASSWORD"
)

// backupSetRoute is every operation the mutating backup-set commands
// perform, in the one vocabulary both destinations are expressed in.
//
// It is assembled out of the two interfaces this package already had
// rather than restating their methods, so the seams that exist to make one
// awkward failure testable (backupSetCreatePrereqs, backupSetRemover)
// stay exactly what they were and cannot drift from this.
//
// *service.BackupService satisfies this today, unchanged. That is the
// property to preserve if it ever grows a method here: a route interface
// the direct implementation has to be wrapped to satisfy is a route
// interface that has started describing the HTTP one.
type backupSetRoute interface {
	backupSetCreatePrereqs
	backupSetRemover

	CreateBackupSet(ctx context.Context, req service.CreateBackupSetRequest) (service.CreateBackupSetResult, error)
	UpdateBackupSet(ctx context.Context, id string, req service.UpdateBackupSetRequest) (service.BackupSet, error)
}

// settingsRoute is `settings patch`'s half of the same seam.
//
// It is here rather than in a later issue because of what leaving it out
// would have meant. `settings patch` is refused beside a running engine
// (#538) and no issue in #536's plan routed it, so when the EPIC finished
// it would have been the one configuration write permanently refused with
// nothing able to replace the refusal. The seam is the same; the shapes
// are a different pair in the same adapter.
//
// UpdateSettings is the write. Settings is here beside it because the
// engine answers a patch with the WHOLE resolved policy and the command
// prints that, so a route that could write and not read would be one that
// could not report what it had done.
type settingsRoute interface {
	Settings(ctx context.Context) (service.Settings, error)
	UpdateSettings(ctx context.Context, req service.UpdateSettingsRequest) (service.Settings, error)
}

// configWriteRoute is where a routed configuration write goes: both halves
// together, because both are reached through one door and one claim.
//
// One door rather than one per command family, so that "this write can be
// routed" is settled once, in the same act as the mode. Each command still
// says what it needs by taking the narrower interface (backupSetRemoveWith
// takes backupSetRemover, resolveKeyAndTrust takes backupSetCreatePrereqs),
// which is what keeps a settings patch from being able to reach a
// backup-set verb at all.
type configWriteRoute interface {
	backupSetRoute
	settingsRoute
}

// attachToEngine builds this invocation's route to the serving process, or
// reports that nothing named one.
//
// Three answers, not two, and the middle one is why. A route that was
// never named is (nil, "", nil): the caller refuses with the sentence it
// already had, because an operator who told this command nothing about
// their engine is in exactly the position #538 left them in. A route that
// was named and cannot be built is an error, because they did tell it
// something and it does not work, and quietly ignoring a setting somebody
// wrote is how a command ends up doing the opposite of what it was told.
//
// Nothing is contacted here. Whether the engine answers is the first
// call's business, and it reports that with an address in it
// (apiclient.Unreachable), which is what an operator has to check.
func attachToEngine() (configWriteRoute, string, error) {
	base := strings.TrimSpace(os.Getenv(apiURLEnv))
	if base == "" {
		return nil, "", nil
	}
	client, err := apiclient.New(apiclient.Config{
		BaseURL:  base,
		Username: strings.TrimSpace(os.Getenv(apiUsernameEnv)),
		Password: os.Getenv(apiPasswordEnv),
		// The product and the build, so a request from this command is one
		// an operator can pick out of an access log. #543's claim is that a
		// routed command leaves the same audit trail as the Web UI, and
		// "Go-http-client/1.1" names no product and no version.
		UserAgent: fmt.Sprintf("backup-manager/%s (api %s)", version, apicontract.Version),
	})
	if err != nil {
		return nil, "", fmt.Errorf("$%s does not name an engine this command can reach, so nothing was written: %w", apiURLEnv, err)
	}
	// BaseURL() rather than the raw environment value: it is normalised
	// and, more to the point, redacted, and this string is printed.
	return &engineRoute{client: client}, client.BaseURL(), nil
}

// clearInheritedRouteSettings removes the three route settings from this
// process's environment.
//
// It exists for the test binary, and it is here rather than in a test file
// so the reason sits beside the variables it is about. Every refusal case
// in this package's suite asserts what happens when a serving process is
// found and nothing has said how to reach it, and "nothing has said" is a
// fact about the environment the test binary inherited. A developer with
// $BACKUP_MANAGER_API_URL exported for their own deployment would run a
// different suite from CI, and the difference would be that half the
// refusals under test quietly became routed writes aimed at their engine.
//
// A test that needs the settings sets them itself, through t.Setenv, which
// restores them afterwards.
func clearInheritedRouteSettings() {
	for _, name := range []string{apiURLEnv, apiUsernameEnv, apiPasswordEnv} {
		_ = os.Unsetenv(name)
	}
}
