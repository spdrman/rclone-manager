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
// A `rbm daemon` announces that it serves this deployment and
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
// Being told it is not the same as it being right, which is what #555
// found: an address an operator typed one character wrong reached the
// other instance on the host and the write landed there, quietly. So an
// address that was told is now checked as well, against the deployment the
// command was typed at, before anything is sent (deploymentcheck.go).
//
// # And why the address and the credentials come from the environment
//
// Not from flags. `rbm --help` is pinned line for line by
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

	// The two modes of the connection check (issue #624). They are here
	// beside the writes, and not left to the local service, for exactly
	// mediumRoute's reason: `backup-set create` verifies before it
	// writes, so the check and the write have to happen in the SAME
	// world. Proving a route to a host from this shell and then
	// declaring the set in a process on the other side of a container
	// boundary would be two claims about two networks reported as one.
	//
	// TestBackupSetConnection is here for a second reason of its own: a
	// check that passes now clears that set's unverified mark, which is
	// a configuration write, so it belongs on the door configuration
	// writes go through rather than beside the reads.
	//
	// *service.BackupService satisfies both as it already stands, which
	// is the property this interface's own doc asks to preserve.
	TestConnection(ctx context.Context, req service.ConnectionTestRequest) (service.ConnectionTestResult, error)
	TestBackupSetConnection(ctx context.Context, id string) (service.ConnectionTestResult, error)
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

// mediumRoute is the storage-destination verbs' half of the same seam
// (G2.2, issue #594).
//
// It is here for settingsRoute's reason, restated for a different noun:
// `medium add`, `edit` and `remove` write configuration, so beside a
// running engine a write left in the file is a change that process would
// never read. Without this interface those three would be permanently
// refused next to a live deployment, which is exactly the position
// `settings patch` was in before #543 and exactly what that issue decided
// was not good enough.
//
// The reads and the probe are here beside the writes rather than left to
// the local service, and that is deliberate rather than convenient.
// `medium add` verifies before it writes, so the probe and the write have
// to happen in the SAME world: probing from this host and writing to the
// engine would mean proving one deployment's route to a bucket and then
// declaring the destination on another. `list` and `show` are here for the
// same reason `settingsRoute` carries Settings beside UpdateSettings,
// which is that a route that could write and not read could not report
// what it had done.
//
// ImportStorageCredentials belongs to this set because the id it mints is
// only meaningful to whoever wrote the file behind it. An id minted here
// and used in a write routed to the engine would name a file on the wrong
// host.
type mediumRoute interface {
	ImportStorageCredentials(ctx context.Context, accessKeyID, secretAccessKey, sessionToken string) (service.MediumCredentialRef, error)
	ListStorageMediums(ctx context.Context) ([]service.StorageMediumSummary, error)
	GetStorageMedium(ctx context.Context, id string) (service.StorageMediumSummary, error)
	StorageMediumUsage(ctx context.Context, id string) (service.StorageMediumUsage, error)
	PreflightStorageMediumCandidate(ctx context.Context, spec service.StorageMediumSpec) (service.MediumPreflight, error)

	// PreflightStorageMedium is the by-id check, and it is here as of
	// #636 for the reason TestBackupSetConnection is here as of #624: a
	// check that passes now clears that destination's unverified mark,
	// which is a configuration write, so it belongs on the door
	// configuration writes go through.
	//
	// That is a real change for this verb and worth naming rather than
	// letting somebody discover it. `medium preflight <id>` used to be a
	// read of THIS host's configuration and was never refused beside a
	// running engine (cmdMedium's own doc said so). It is now refused
	// beside a serving engine that this command has not been told how to
	// reach, exactly as `medium add`, `edit` and `remove` already are.
	// The alternative was worse in a way that is hard to see and hard to
	// undo: a mark cleared in the file beside a running engine is a change
	// that process never reads, and its next configuration write would put
	// the mark back from a stale copy. `list` and `show` stay on the read
	// door, so looking at your own destinations still works with nothing
	// configured.
	PreflightStorageMedium(ctx context.Context, id string) (service.MediumPreflight, error)

	CreateStorageMedium(ctx context.Context, spec service.StorageMediumSpec) (service.StorageMediumSummary, error)
	UpdateStorageMedium(ctx context.Context, spec service.StorageMediumSpec) (service.StorageMediumSummary, error)
	RemoveStorageMedium(ctx context.Context, id string) error

	// SetDefaultStorageMedium moves the destination a newly created
	// retention tier starts on (H2.2, issue #622). It is a configuration
	// write like the three above it and is here for the same #538 reason:
	// a default changed in the file beside a running engine is a change
	// that process would never read, and the next tier added through its
	// web UI would start somewhere else.
	SetDefaultStorageMedium(ctx context.Context, id string) (service.StorageMediumSummary, error)
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
	mediumRoute
}

// attachToEngine builds this invocation's route to the serving process,
// confirms it leads to THIS deployment, or reports that nothing named one.
//
// Three answers, not two, and the middle one is why. A route that was
// never named is (nil, "", nil): the caller refuses with the sentence it
// already had, because an operator who told this command nothing about
// their engine is in exactly the position #538 left them in. A route that
// was named and cannot be built, or that leads somewhere else, is an
// error, because they did tell it something and it does not do what they
// meant, and quietly ignoring a setting somebody wrote is how a command
// ends up doing the opposite of what it was told.
//
// The engine IS contacted here, and that changed with #555. It used to be
// left to the first call, on the reasoning that an unreachable engine
// reports itself with an address in the message. What that could not do is
// notice a REACHABLE engine that is a different deployment, which is a
// mistyped address rather than a broken one and which used to succeed
// silently. So one round trip is spent before anything is sent, asking the
// engine which deployment it serves; deploymentcheck.go holds the
// reasoning and the refusals.
func attachToEngine(ctx context.Context, engine *service.RunningEngine) (configWriteRoute, string, error) {
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
		UserAgent: fmt.Sprintf("rclone-manager/%s (api %s)", version, apicontract.Version),
	})
	if err != nil {
		// A routeRefusal like every other way this function refuses a
		// route that was named, and with no address, because there is
		// none: nothing was dialled. It carries a reason so the mode line
		// says an address was given and could not be used, rather than
		// saying this build has no route at all (mode.go's announce).
		return nil, "", &routeRefusal{
			reason: fmt.Sprintf("$%s does not name an engine this command can reach", apiURLEnv),
			detail: fmt.Sprintf("$%s does not name an engine this command can reach, so nothing was written: %v", apiURLEnv, err),
			cause:  err,
		}
	}
	// Before the route is handed back, never after. A route returned and
	// then checked would be a route some later caller could use without
	// checking, and there is no version of this that is safe to skip.
	if err := confirmDeployment(ctx, client, engine); err != nil {
		return nil, "", err
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
