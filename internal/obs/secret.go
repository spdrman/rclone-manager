package obs

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// redacted is what a Secret renders as, no matter which of the interfaces
// below is the one that ends up asking.
const redacted = "[REDACTED]"

// Secret wraps a value that must never reach a log line, an error string, a
// JSON blob, or a debugger's Printf, in the clear.
//
// # Why a wrapper type instead of a naming convention
//
// FR-23 says secrets must never be logged, but "never do X" is not a
// property of a codebase, it is a property of every call site in that
// codebase, forever, including the ones nobody has written yet. This
// project handles SSH key paths, known_hosts paths, and config sections
// that may carry credentials (see internal/config's Remote.KeyFile and
// Remote.KnownHosts), and a stray %v in a log statement is the easiest way
// for one of those to leave a system that otherwise guards them carefully.
// A convention ("remember not to log cfg.KeyFile directly") only holds
// until the one time someone is debugging a host-verification failure at
// 2am and reaches for fmt.Printf("%+v", cfg). Secret exists so that reflex
// prints a placeholder instead of the path.
//
// # Why a struct, not a defined string type
//
// An earlier draft of this considered `type Secret string`. That fails the
// actual goal: converting a defined string type back to plain string is a
// one-line, compiler-blessed operation (string(mySecret)) that needs no
// special knowledge and looks like nothing in review. Wrapping the value in
// a struct with an unexported field closes that door: there is no
// conversion from Secret to string at all, plain or otherwise. The only way
// to get the raw value back out is to call Reveal, a name chosen to be
// grep-able and to look deliberate rather than incidental in a diff. Search
// this repository for ".Reveal(" to audit every place a secret is allowed
// to leave its wrapper.
//
// # What is actually neutralized
//
// Every rendering path Go's standard library offers a value is covered
// below, each redirected to the same redacted constant regardless of what
// the caller asked for:
//
//   - fmt.Stringer (String), for %v, %s and %q, and for anything that just
//     calls .String() directly;
//   - fmt.Formatter (Format), which fmt consults before any other
//     interface and which, once implemented, is handed every verb and flag
//     combination there is (including %#v, %+v, %x and %X). This is the
//     one that actually matters, since it forecloses every fmt verb at
//     once rather than only the common ones;
//   - fmt.GoStringer (GoString), for completeness, though Formatter above
//     already wins that contest;
//   - encoding.TextMarshaler (MarshalText), for anything that renders
//     through the text-marshaling path rather than fmt or JSON;
//   - json.Marshaler (MarshalJSON), so encoding/json, including the path
//     slog's own JSON handler uses for a value it doesn't otherwise
//     recognise, never serializes the underlying bytes;
//   - slog.LogValuer (LogValue), which is log/slog's own purpose-built hook
//     for exactly this problem (see the standard library's docs for
//     LogValuer's Password example) and takes effect before slog ever asks
//     a Formatter or a Stringer anything.
//
// secret_test.go exercises all of the above against a live value and
// asserts the raw bytes never appear in any rendering, rather than trusting
// this comment.
//
// # What this does not claim to defend against
//
// Reveal itself, called by a caller that then does log it anyway. Nothing
// in Go's type system can stop that; Secret's job is to make the accidental
// path (an unwrapped fmt verb, a stray %+v, a debug dump of a config
// struct) render a placeholder, so that logging a secret takes a deliberate
// act (calling Reveal and then choosing to print the result) rather than a
// reflex. It also does not defend against unsafe/reflect deliberately
// reaching past the unexported field; that is a different threat model
// than "an engineer under time pressure formats a struct".
type Secret struct {
	v string
}

// NewSecret wraps v so it can travel through the rest of this package (and
// anything downstream that respects fmt, encoding/json or log/slog) without
// rendering in the clear. Wrap at the point the sensitive value is first
// read (a config field, an SSH key path, a credential pulled from the
// environment) so that nothing downstream of that point ever holds the
// bare string at all.
func NewSecret(v string) Secret {
	return Secret{v: v}
}

// Reveal returns the wrapped value in the clear. Call it only at the
// boundary that actually needs the raw secret, for example handing an SSH
// key path to the transport layer's dial code, never to build a log line,
// an error message, or anything else that might be persisted or displayed.
// The name is deliberately loud and easy to grep for; treat every call site
// as worth a second look in review.
func (s Secret) Reveal() string {
	return s.v
}

// String implements fmt.Stringer. See the type doc for why Format below is
// the one that actually forecloses every fmt verb; this exists so a direct
// call to .String(), or a formatting path that only consults Stringer,
// still gets the placeholder rather than a compile error or a zero value.
func (s Secret) String() string {
	return redacted
}

// GoString implements fmt.GoStringer, covering %#v for any caller that
// somehow reaches this before Format (Format, implemented below, already
// wins that race per fmt's documented precedence, but belt and suspenders
// costs nothing here).
func (s Secret) GoString() string {
	return redacted
}

// Format implements fmt.Formatter. fmt consults this interface before any
// other special-case interface and before applying verb-specific behaviour,
// so implementing it is what actually closes off %v, %s, %q, %x, %X, %#v,
// %+v and everything else fmt understands: this method is asked
// unconditionally, regardless of verb or flags, and it writes the same
// placeholder no matter what it's asked for.
func (s Secret) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(redacted))
}

// MarshalText implements encoding.TextMarshaler.
func (s Secret) MarshalText() ([]byte, error) {
	return []byte(redacted), nil
}

// MarshalJSON implements json.Marshaler, so encoding/json, including the
// path slog's JSON handler falls back to for a value with no more specific
// handling, renders the placeholder rather than the wrapped string. Secret
// has no exported fields, so without this method json.Marshal would
// actually already emit "{}" rather than the raw value; this method trades
// that accidental safety for an explicit, self-documenting one.
func (s Secret) MarshalJSON() ([]byte, error) {
	return json.Marshal(redacted)
}

// LogValue implements slog.LogValuer, the standard library's own
// purpose-built hook for exactly this problem (see log/slog's docs for its
// own Password example). A Handler resolves LogValuer before it does
// anything else with a value, so this is what keeps a Secret handed
// straight to slog.Any, slog.Group or a struct field slog decides to
// reflect over safe by construction, independent of the fmt/json defenses
// above.
func (s Secret) LogValue() slog.Value {
	return slog.StringValue(redacted)
}
