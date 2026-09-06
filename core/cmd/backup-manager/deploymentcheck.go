package main

import (
	"context"
	"fmt"

	"github.com/spdrman/rclone-manager/core/internal/apiclient"
	"github.com/spdrman/rclone-manager/core/service"
)

// Issue #555: a routed write checks that the engine at the other end is
// the deployment this command was typed at, before it sends anything.
//
// # The asymmetry this removes
//
// A read already compares the engine's config_revision against the one it
// computed from the file it loaded, and refuses when they disagree
// (readmode.go). A write compared nothing. So a read aimed at the wrong
// engine refused honestly and a write aimed at the wrong engine succeeded
// quietly, which is exactly backwards: a stray read shows an operator
// something confusing, a stray write changes a deployment they were not
// looking at. It was driven rather than argued about: two deployments on
// one host, a `backup-set create` typed at the first with the second's
// address in $BACKUP_MANAGER_API_URL, and the set landed in the second
// with the first's config.yaml untouched and both surfaces reporting
// success.
//
// # Why the revision is the wrong thing to compare here
//
// A revision is a hash of configuration CONTENT. Two deployments holding
// identical configurations therefore share one, and that is not an exotic
// arrangement: a staging and a production instance built from the same
// template do it, and those two are exactly the pair whose addresses get
// confused. A revision comparison would have closed the case that was
// demonstrated and left the sharper one open.
//
// So what is compared is a deployment IDENTITY: minted once beside each
// journal, stable across every configuration edit, distinct between
// instances, and served on GET /system/version.
// core/service/deploymentidentity.go argues where it comes from, what
// happens to a deployment that is restored or cloned, and the one case it
// cannot see.
//
// # Both sides, and the refusal names both
//
// The near side is read from beside the journal the local configuration
// names, which is the deployment the operator is standing in. The far side
// is read off the wire. An operator who has just mistyped a URL needs to
// see the two together: told only where they ended up, they still do not
// know whether that was the instance they meant.
//
// # Three ways to be unable to confirm, and all three refuse
//
// An engine that does not answer, a deployment with no identity, and an
// engine that reports none are all "this cannot be checked", and a check
// that cannot be performed is a refusal here rather than a shrug. That is
// the same posture liveengine.go's cannotTellError takes about the engine
// probe and the same one lock_other.go states as a rule: a safety
// condition this codebase cannot honestly assess is reported, never
// quietly skipped. It matters most for the empty answers, because two
// processes that both name nothing would compare EQUAL, which is the one
// way this check could pass while proving nothing.
//
// That sub-rule is load-bearing and it is now driven rather than argued
// about. deploymentcheck_test.go runs a routed write against an engine
// that names no deployment, against a deployment that has no identity,
// and against the pair of them at once, and a build with the two empty
// refusals deleted reddens all three: the comparison passes, the write is
// sent, and it lands wherever the address pointed.

// routeRefusal is a route that was named and that this command will not
// send a change through, with the two things the mode announcement needs
// to say about it.
//
// It is one type carrying a reason rather than a type per shape, and it
// is wider than the *wrongDeploymentError it replaces on purpose. That
// one covered only "the route leads to a different deployment", so the
// mode line printed "this build has no route to it" for the other four
// ways a named route is refused: the three below, and route.go's address
// that cannot be made into a client at all. One line above a refusal
// saying the opposite.
// "There is no route to the engine serving this deployment" and "the
// route goes somewhere this command will not write" send an operator to
// two different places: set an address, or correct one they already set.
type routeRefusal struct {
	// address is the engine that answered, already redacted (this is
	// apiclient's BaseURL, not the raw environment value, which can carry
	// userinfo credentials). It is empty when no route could be built at
	// all, which is the one shape here that never reached an engine.
	address string

	// reason is the short clause the mode line carries, written to follow
	// "Another process is serving this deployment (state database X) and".
	// It does not repeat the state database, which that line already has.
	reason string

	// detail is the whole sentence the refusal prints, which says what to
	// do about it as well as what happened.
	detail string

	// cause is whatever this was made from, kept so errors.Is and
	// errors.As reach it exactly as they did when these were
	// fmt.Errorf("...%w"). Nothing here reads it; what reads it is
	// whatever decides an exit status, and a refusal that quietly stopped
	// carrying a sentinel would change one without saying so.
	cause error
}

func (e *routeRefusal) Error() string { return e.detail }
func (e *routeRefusal) Unwrap() error { return e.cause }

// misaimedRoute is the refusal an operator is most likely to cause: a
// route that was built, answered, and turned out to lead to a different
// deployment on the same host.
//
// Both identities, always, and the state database beside them: an
// operator who has just mistyped a URL needs to see the deployment they
// meant next to the one they reached, and the journal path is what lets
// them match "this deployment" against a container in front of them.
func misaimedRoute(address, stateDatabase, local, served string) *routeRefusal {
	return &routeRefusal{
		address: address,
		reason:  fmt.Sprintf("the engine at %s serves a different deployment", address),
		detail: fmt.Sprintf(
			"the engine at %s serves a different deployment from the one this command was typed at, so nothing was written and nothing was sent: it reports deployment %s, and this deployment (state database %s) is %s. A write sent there would have changed a deployment you are not looking at and left this one exactly as it is, so check $%s",
			address, served, stateDatabase, local, apiURLEnv),
	}
}

// confirmDeployment asks the engine which deployment it serves and refuses
// unless it is this one.
//
// It runs before the first mutation and after the route is built, inside
// the claim enterConfigWriteMode holds, so nothing can finish starting
// between the answer and the write it authorises.
//
// The near side is read and never minted, and that is now a fact about
// the shape of the code rather than about this function's manners.
// core/service mints in AnnounceServing and nowhere else, so the only
// processes that can name a deployment are the ones about to serve it;
// this command opens the same journal through the same service.Open and
// reads whatever is there. It used to be a promise this file made and
// setup.go broke two calls later: openConfigWriteRoute calls service.Open,
// which ran runStartupSequence, which minted. On a deployment restored
// from its .db alone, beside an engine still holding the old identity,
// one `backup-manager status` renamed the deployment and every routed
// write afterwards refused against its own engine while pointing the
// operator at $BACKUP_MANAGER_API_URL.
//
// The refusal below for a deployment with no identity is reachable
// because of that change. It could not fire before: anything that got
// here had already minted one.
func confirmDeployment(ctx context.Context, client *apiclient.Client, engine *service.RunningEngine) error {
	address := client.BaseURL()
	local, err := service.DeploymentIdentity(engine.StateDatabase)
	if err != nil {
		return &routeRefusal{
			address: address,
			reason:  "this command could not read which deployment it is standing in",
			detail:  fmt.Sprintf("this command could not read which deployment it is standing in (state database %s), so it cannot confirm that the engine at %s is the one serving it and nothing was written: %v", engine.StateDatabase, address, err),
			cause:   err,
		}
	}

	served, err := client.SystemVersion(ctx)
	if err != nil {
		return &routeRefusal{
			address: address,
			reason:  fmt.Sprintf("the engine at %s did not answer when this command asked which deployment it serves", address),
			detail:  fmt.Sprintf("$%s names an engine that did not answer when this command asked which deployment it serves, so nothing was written: %v", apiURLEnv, err),
			cause:   err,
		}
	}

	if local == "" {
		return &routeRefusal{
			address: address,
			reason:  "this deployment has no identity of its own",
			detail:  fmt.Sprintf("this deployment (state database %s) has no identity of its own, so this command cannot confirm that the engine at %s is the one serving it and nothing was written: an identity is minted beside the journal by the process that serves the deployment, so restarting that process gives this one a name", engine.StateDatabase, address),
		}
	}
	if served.DeploymentID == "" {
		// What was observed, and then the likeliest remedy, rather than a
		// diagnosis this command cannot make. This used to say an engine
		// naming no deployment "is one from before this check existed",
		// and it is one of three: apps/common/webhost's
		// handlers_system.go names the other two on the field itself, a
		// process holding no deployment and one whose identity file could
		// not be read, and nothing on the wire tells them apart. Stating
		// one of three as the cause is the defect #557 takes out of
		// gotestwatch, and its doc.go states the rule: report what was
		// observed, offer the likeliest remedy, and do not dress a guess
		// up as a finding.
		return &routeRefusal{
			address: address,
			reason:  fmt.Sprintf("the engine at %s did not say which deployment it serves", address),
			detail:  fmt.Sprintf("the engine at %s answered without naming a deployment, so this command cannot confirm it is the one serving this deployment (state database %s) and nothing was written: a change sent there could land in a different deployment altogether. An engine answers this way when it is running a build from before this check existed, when it could not read its own deployment's identity, and when it is holding no deployment at all, and nothing on the wire tells those apart. Restarting the process that serves this deployment on this build is the likeliest of the three to fix it; failing that, make the change through the Web UI it serves", address, engine.StateDatabase),
		}
	}
	if served.DeploymentID != local {
		return misaimedRoute(address, engine.StateDatabase, local, served.DeploymentID)
	}
	return nil
}
