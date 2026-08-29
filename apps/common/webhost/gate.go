package webhost

// DestructiveGate reports whether the trusted-proxy identity verification
// required before any destructive/mutating operation may run has actually
// been established for this deployment (docs/EPIC-B-multi-nas.md §13.3,
// §13.5). #92 (B1.3) is the work package that performs that verification
// on real hardware and is expected to add the implementation that flips
// this to true once it has; until it does, every implementation of this
// interface this repository ships MUST report false.
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
