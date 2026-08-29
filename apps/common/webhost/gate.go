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
