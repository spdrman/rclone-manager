package webhost

// The refusal that every deployment of this product is currently living
// under, what it actually withholds, and the reason it is not a bug.
//
// # What it does NOT mean, corrected (issue #597)
//
// This file used to open with "nothing shipped in this repository can
// make a destructive operation run", and that sentence was wrong in the
// way that matters most: it reads as "destructive operations are off",
// and they are not. `serve` always starts the scheduler
// (serve.RunEngine), and core/service's runScheduledCycle calls the
// identical internal/app.Service.RunCycle, taking the same runOnce lock,
// reaching the identical FR-15 remote delete, on every poll interval,
// with this gate shut. A deployment with the gate closed is deleting
// remote sources unattended right now.
//
// So what the gate withholds is not destruction. It is an OPERATOR's
// ability to start work on demand over HTTP: POST /operations (a run
// cycle, a per-set run, or a restore), the run_immediately tier of
// creating a backup set, and POST .../retention/apply. Two of those three
// do work the scheduler already does by itself on a timer. The third,
// retention apply, is the one the shut gate genuinely prevents, and it
// has therefore never run in any shipped deployment: local restore points
// accumulate without bound until FR-21's capacity refusal starts refusing
// transfers (issue #602).
//
// That is worth stating at the top rather than leaving to be worked out,
// because the wrong reading makes the gate look like a safety property it
// does not deliver, and makes opening it look more dangerous than it is
// in one direction and much less dangerous than it is in the other.
//
// # Why it is still shut
//
// Turning it on means writing an implementation that has actually
// verified #92's trusted-proxy identity check against real hardware, or
// established that the running profile has no identity header to spoof in
// the first place. Until somebody has done one of those, refusing is the
// honest answer.
//
// The shape is chosen to make that hard to undo by accident. A bool on
// RouterConfig would be flipped by whoever next wires a router and would
// look, in a diff, like configuration. An interface with no way to
// construct a passing implementation from outside cannot be flipped
// without somebody writing code and naming it, which is exactly the amount
// of friction this deserves.
//
// The other thing to not get wrong is the scope. This answers one question
// once for the whole deployment, and it must never grow a request
// argument: per-request trust belongs on capabilities.Authenticator, which
// is already given the headers and the peer address it would need.

// DestructiveGate reports whether the trusted-proxy identity verification
// required before an operator may START destructive work over HTTP has
// actually been established for this deployment
// (docs/EPIC-B-multi-nas.md §13.3, §13.5). #92 (B1.3) is the work package
// that performs that verification on real hardware and is expected to add
// the implementation that flips this to true once it has; until it does,
// every implementation of this interface this repository ships MUST
// report false.
//
// "START over HTTP" rather than "run", deliberately: the scheduler runs
// the same destructive cycle on a timer whatever this reports. See this
// file's own doc for what a shut gate actually withholds, which is a
// narrower and more surprising list than it looks.
//
// This is deliberately a narrow, single-method interface rather than a
// bool field on RouterConfig: a bool the caller passes in can be flipped
// by anything that constructs a RouterConfig, which is exactly the "TODO
// enable later" shape issue #94 was explicit about not wanting. A gate
// implementation, by contrast, is a piece of code #92 has to actually
// write and prove, and NewRouter's default (see router.go) never accepts
// a caller-supplied "true" without one.
//
// # DestructiveGate is static; per-request verification is Authenticator's job
//
// Passed() takes no arguments and answers one question for the whole
// deployment, once: "has #92's trusted-proxy check been proven against
// THIS deployment's actual network topology at all" — a static,
// deployment-level attestation, evaluated the same way regardless of which
// request is asking. It is NOT, and must never become, a place that
// re-derives per-request trust (which specific peer sent THIS request,
// whether THIS request's identity headers actually came from the trusted
// proxy). §13.3's per-request spoof detection ("reject UGOS-authenticated
// API requests that did not traverse the trusted gateway") belongs on
// capabilities.Authenticator.Authenticate instead: that method already
// takes a request and its headers (capabilities.AuthRequest), which is
// where #92's per-request verification logic actually has the information
// it needs to run. Concretely: DestructiveGate answers "is this
// deployment even allowed to consider destructive operations from
// anyone", Authenticator answers "is this specific request from someone
// this deployment should trust" — two different questions, checked in
// that order (see requireDestructiveGate's own doc), and #92 needs to
// implement both, not conflate them into this one method. This is a
// documentation clarification, not a signature change: settling the
// ambiguity here, where #92's author will look first, is cheaper than
// #92 discovering it mid-implementation.
type DestructiveGate interface {
	Passed() bool
}

// NotYetImplementedGate is the only DestructiveGate implementation this
// repository ships today. It always reports false. There is deliberately
// no constructor parameter, environment variable, or config flag that can
// make it report true: flipping it is #92's job, and #92's job alone. Any
// production wiring of this package that does not explicitly supply a
// different, #92-authored DestructiveGate gets this one (see
// RouterConfig.Gate's doc in router.go), so destructive operations fail
// closed by construction rather than by a flag nobody remembered to leave
// off.
type NotYetImplementedGate struct{}

// Passed always reports false.
func (NotYetImplementedGate) Passed() bool { return false }
