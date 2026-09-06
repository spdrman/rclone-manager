package apiclient

import (
	"errors"
	"fmt"

	"github.com/spdrman/rclone-manager/core/apicontract"
)

// Four things happen to a call, and they must never be reported as one.
//
// It worked. The caller is not signed in. Nothing is listening. Something
// answered that is not the engine this build was written against. Those
// have four different remedies - do nothing, sign in, start the engine,
// look at what is on that port - and a client that returns one opaque
// error for all four sends an operator to the wrong one. ui/shared's
// single "The backup service returned an unexpected response." is what
// that reads like from the outside, and issue #211 is what it cost.
//
// So each is its own type, each names the operation it was doing, and the
// two an operator most often has to distinguish get a sentinel to match on
// with errors.Is. The types stay open (exported fields, no accessors)
// because the command layer in #543/#544 has to be able to print a
// deployment-specific remedy, not just a category.

// ErrUnreachable matches any failure that happened before an HTTP response
// existed: a refused connection, a name that does not resolve, a timeout,
// a TLS handshake that failed. Match it with errors.Is.
//
// It deliberately does NOT mean "the engine said no". An engine that
// answers at all, even to refuse, is running.
var ErrUnreachable = errors.New("apiclient: the engine did not answer")

// ErrUnauthenticated matches a refusal to accept, or a failure to
// establish, a session: the engine answered UNAUTHENTICATED, or no
// credentials were supplied to establish one with. Match it with
// errors.Is.
var ErrUnauthenticated = errors.New("apiclient: not authenticated")

// Unreachable is a failure with no HTTP response behind it. It names the
// base URL because that, not the path, is what an operator has to check:
// the wrong port, a container that is not running, a host that is not this
// one.
type Unreachable struct {
	// Operation is the contract operation id that was being attempted.
	Operation string
	// BaseURL is the address the request went to.
	BaseURL string
	// Err is the underlying transport failure, kept so a caller can still
	// reach context.DeadlineExceeded or a *net.OpError through
	// errors.Is/errors.As.
	Err error
}

func (e *Unreachable) Error() string {
	return fmt.Sprintf("apiclient: %s: no engine answered at %s: %v", e.Operation, e.BaseURL, e.Err)
}

func (e *Unreachable) Unwrap() error { return e.Err }

// Is reports Unreachable as ErrUnreachable, so a caller can ask the
// question it actually has ("is the engine down?") without type-asserting.
func (e *Unreachable) Is(target error) bool { return target == ErrUnreachable }

// Error is a refusal the contract describes: a status this operation
// declares, carrying an error code that operation declares for it.
//
// Reaching this type is good news about the deployment. It means an engine
// is running, it understood the request, and it said no for a reason the
// published contract already names, which is a reason a command can act on
// rather than merely report.
type Error struct {
	// Operation is the contract operation id.
	Operation string
	// Method and Path are what was requested, for a message an operator
	// can match against a log line.
	Method string
	Path   string
	// Status is the HTTP status the engine answered with.
	Status int
	// Code is the stable, machine-readable token from the contract's own
	// registry. This is the field to branch on; Message may change without
	// notice.
	Code apicontract.ErrorCode
	// Message is the engine's human-readable text.
	Message string
	// CorrelationID is what the engine put in X-Correlation-Id, so an
	// operator quoting an error can be matched to a log line.
	CorrelationID string
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("apiclient: %s: the engine refused %s %s with %d %s: %s",
		e.Operation, e.Method, e.Path, e.Status, e.Code, e.Message)
	if e.CorrelationID != "" {
		msg += " (correlation id " + e.CorrelationID + ")"
	}
	return msg
}

// Is reports an UNAUTHENTICATED refusal as ErrUnauthenticated. Only that
// one code: a CSRF refusal is a 403 about a token, not about who the
// caller is, and folding the two together would send an operator looking
// for a password problem they do not have.
func (e *Error) Is(target error) bool {
	return target == ErrUnauthenticated && e.Code == apicontract.ErrorCodeUnauthenticated
}

// ContractViolation is an answer api/v1/openapi.json does not describe.
//
// This is the case worth having a name for. Everything else on this page
// is a thing that went wrong inside a shared understanding; this one is
// the understanding itself being absent, and it is what a CLI meets when
// it is pointed at a newer engine, an older engine, a reverse proxy that
// answered on the engine's behalf, or something on that port that is not
// this product at all. Decoding it into a zero value and returning success
// is the failure this type exists to prevent: an empty list of backup sets
// and "there are no backup sets" are not the same sentence.
type ContractViolation struct {
	// Operation is the contract operation id that was attempted.
	Operation string
	// Method and Path are what was requested.
	Method string
	Path   string
	// Status is the HTTP status, or 0 when the violation was found before
	// a request was made at all.
	Status int
	// Reason says what the contract expected and what arrived instead.
	Reason string
}

func (e *ContractViolation) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("apiclient: %s: %s", e.Operation, e.Reason)
	}
	return fmt.Sprintf("apiclient: %s: %s %s answered %d, which api/v1/openapi.json does not describe: %s",
		e.Operation, e.Method, e.Path, e.Status, e.Reason)
}
