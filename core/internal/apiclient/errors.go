package apiclient

import (
	"errors"
	"fmt"
	"net/url"

	"github.com/spdrman/rclone-manager/core/apicontract"
)

// Five things happen to a call, and they must never be reported as one.
//
// It worked. The caller is not signed in. Nothing is listening. Something
// answered that is not the engine this build was written against. Or this
// client was never given a usable address to try. Those have five
// different remedies - do nothing, sign in, start the engine, look at what
// is on that port, fix the setting - and a client that returns one opaque
// error for all of them sends an operator to the wrong one. ui/shared's
// single "The backup service returned an unexpected response." is what
// that reads like from the outside, and issue #211 is what it cost.
//
// So each is its own type, each names the operation it was doing, and the
// two an operator most often has to distinguish get a sentinel to match on
// with errors.Is. The types stay open (exported fields, no accessors)
// because the command layer in #543/#544 has to be able to print a
// deployment-specific remedy, not just a category.
//
// # The list is exhaustive, and that is the property worth defending
//
// A taxonomy is only useful to a caller if EVERY failure is in it: a
// command writing the obvious switch and falling through to a default
// branch is back to ui/shared's one sentence. So the five below are
// everything this package returns, and taxonomy_test.go holds it, by
// walking the source for a return that escapes them and by driving one
// instance of each against a real server. Adding a sixth is a change to
// that list, to this comment and to doc.go, in that order.
//
//	*ConfigError       nothing was tried; the address is unusable.
//	*Unreachable       nothing answered.
//	*NoCredentials     nothing was tried; there is nothing to sign in with.
//	*Error             the engine answered, and refused, for a named reason.
//	*ContractViolation the answer is not one api/v1/openapi.json describes,
//	                   or the request was not one it could have made.

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
	// BaseURL is the address the request went to, with any userinfo
	// redacted. New refuses a base URL carrying credentials, so this is
	// already clean when the client built it; Error() redacts again anyway,
	// because this is the string that ends up in a support ticket and a
	// field anybody can fill in.
	BaseURL string
	// Err is the underlying transport failure, kept so a caller can still
	// reach context.DeadlineExceeded or a *net.OpError through
	// errors.Is/errors.As.
	Err error
}

func (e *Unreachable) Error() string {
	return fmt.Sprintf("apiclient: %s: no engine answered at %s: %v", e.Operation, redactURL(e.BaseURL), e.Err)
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

// NoCredentials is this client refusing before the wire: it holds neither
// a live session nor a username and password, so there is nothing to sign
// in with.
//
// It is deliberately NOT an *Error. Reaching *Error means an engine
// answered; this means nothing was asked, and the base URL may point at
// nothing at all. A command that reported this as "the engine refused your
// login" would send an operator to check a password when the thing to
// check is whether an engine is there. So it claims no status and names no
// path, because it has neither, while still matching ErrUnauthenticated:
// "you are not signed in" is exactly what happened.
type NoCredentials struct {
	// Operation is the contract operation id that could not proceed.
	Operation string
	// Reason says which of the two was missing.
	Reason string
}

func (e *NoCredentials) Error() string {
	return fmt.Sprintf("apiclient: %s: %s", e.Operation, e.Reason)
}

func (e *NoCredentials) Is(target error) bool { return target == ErrUnauthenticated }

// redactURL replaces a password in a URL with "xxxxx".
//
// url.URL.String() renders userinfo verbatim; only Redacted() hides it.
// Every place this package prints an address goes through here, because
// the whole point of printing one is that a human reads it, and a password
// on a terminal has been copied into a scrollback buffer, a screenshot and
// a support ticket before anybody notices.
func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return raw
	}
	return parsed.Redacted()
}

// ConfigError is New refusing a Config it cannot build a client from.
//
// It is a category rather than a bare fmt.Errorf because doc.go's claim is
// that every failure this package produces is one a caller can switch on,
// and "the address you were given is not usable" is a remedy of its own:
// nobody has been contacted, no credential has been used, and the fix is
// in whatever supplied the setting rather than in the deployment.
type ConfigError struct {
	// Field names the Config field at fault, empty when the failure is not
	// about one field's value.
	Field string
	// Value is what that field held, already redacted when it held a
	// password: refusing a credential by echoing it would be the very
	// defect the refusal exists for, and a command printing a structured
	// refusal reads this rather than Error(). Error() redacts again, for
	// anybody who fills this in themselves.
	Value string
	// Reason says what was wrong with it.
	Reason string
	// Err is the underlying failure, when there was one, so a caller can
	// still reach a *url.Error through errors.As.
	Err error
}

func (e *ConfigError) Error() string {
	switch {
	case e.Field == "":
		return "apiclient: " + e.Reason
	case e.Value == "":
		return fmt.Sprintf("apiclient: %s: %s", e.Field, e.Reason)
	default:
		return fmt.Sprintf("apiclient: %s %q: %s", e.Field, redactURL(e.Value), e.Reason)
	}
}

func (e *ConfigError) Unwrap() error { return e.Err }
