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

// wrongDeploymentError is a route that was built, answered, and turned out
// to lead somewhere else.
//
// It is a type rather than a sentence because the mode announcement has to
// tell this apart from the other refusals: "there is no route to the
// engine serving this deployment" and "the route goes to a different
// deployment" are different facts about an operator's host, and a command
// that printed the first when the second was true would send them looking
// for a configuration setting they had already made.
type wrongDeploymentError struct {
	// address is the engine that answered, already redacted (this is
	// apiclient's BaseURL, not the raw environment value, which can carry
	// userinfo credentials).
	address string

	// stateDatabase is the journal of the deployment this command was
	// typed at, spelled as its configuration spells it, so an operator can
	// match it against their own container.
	stateDatabase string

	// local and served are the two identities. Both, always: see this
	// file's own doc.
	local, served string
}

func (e *wrongDeploymentError) Error() string {
	return fmt.Sprintf(
		"the engine at %s serves a different deployment from the one this command was typed at, so nothing was written and nothing was sent: it reports deployment %s, and this deployment (state database %s) is %s. A write sent there would have changed a deployment you are not looking at and left this one exactly as it is, so check $%s",
		e.address, e.served, e.stateDatabase, e.local, apiURLEnv)
}

// confirmDeployment asks the engine which deployment it serves and refuses
// unless it is this one.
//
// It runs before the first mutation and after the route is built, inside
// the claim enterConfigWriteMode holds, so nothing can finish starting
// between the answer and the write it authorises.
//
// The near side is read and never minted (service.DeploymentIdentity is
// the reading half on purpose). A command that is about to hand a change
// to somebody else's process has no business naming the deployment it is
// standing in: an identity invented here would be compared against the
// engine's and would refuse for a reason this command had created.
func confirmDeployment(ctx context.Context, client *apiclient.Client, engine *service.RunningEngine) error {
	local, err := service.DeploymentIdentity(engine.StateDatabase)
	if err != nil {
		return fmt.Errorf("this command could not read which deployment it is standing in (state database %s), so it cannot confirm that the engine at %s is the one serving it and nothing was written: %w", engine.StateDatabase, client.BaseURL(), err)
	}

	served, err := client.SystemVersion(ctx)
	if err != nil {
		return fmt.Errorf("$%s names an engine that did not answer when this command asked which deployment it serves, so nothing was written: %w", apiURLEnv, err)
	}

	if local == "" {
		return fmt.Errorf("this deployment (state database %s) has no identity of its own, so this command cannot confirm that the engine at %s is the one serving it and nothing was written: an identity is minted beside the journal the first time a process opens it, so restarting the process that serves this deployment gives it one", engine.StateDatabase, client.BaseURL())
	}
	if served.DeploymentID == "" {
		return fmt.Errorf("the engine at %s did not say which deployment it serves, so this command cannot confirm it is the one serving this deployment (state database %s) and nothing was written: an engine that names no deployment is one from before this check existed, and a change sent to it could land in a different deployment altogether. Restart that process on this build, or make the change through the Web UI it serves", client.BaseURL(), engine.StateDatabase)
	}
	if served.DeploymentID != local {
		return &wrongDeploymentError{
			address:       client.BaseURL(),
			stateDatabase: engine.StateDatabase,
			local:         local,
			served:        served.DeploymentID,
		}
	}
	return nil
}
