package obs

import (
	"sort"
	"strconv"
	"strings"
)

// Endpoint identifies one configured remote's network location and account
// identity: exactly the three values issue #295 says a deployment must be
// able to ask never appear in a log line or a journal detail, once that
// remote is marked sensitive (see config.Remote.Sensitive). Port zero
// means "no port configured" (config.Remote's own convention: 0 selects
// the backend's default) and contributes no port-shaped needle below.
type Endpoint struct {
	Host string
	Port int
	User string
}

// Redactor filters a RENDERED string, substituting redacted for every
// occurrence of an Endpoint's host, port and account identity it was built
// from.
//
// # Why the rendered string, not the code path
//
// Issue #295's whole argument is that the strings that leak a non-default
// port are not built by this project at all: they arrive pre-formatted
// from Go's own net dialer (by way of rclone) or from rclone's own copy
// verification, already containing "host:port" or "user@host:port"
// wherever the underlying library chose to put it. There is no call site
// in this repository to wrap, because the bytes that need to come out
// were never assembled by a call site here in the first place. A Redactor
// therefore doesn't know or care where a string came from: it takes
// whatever obs.Logger.emit or state.Journal.RecordTransition is about to
// write down and blanks every substring that matches one of the endpoints
// it was built from, wherever in that string it happens to appear.
//
// # Why substring replacement, not a regexp
//
// Every needle here is a literal value this process already holds
// (config.Remote's own Host/Port/User), not a pattern inferred from the
// error text. A regexp would have to guess at the shape of an error this
// project does not control and does not want to (see this package's own
// containment argument in doc.go); a literal substring match only ever
// removes bytes provably belonging to this endpoint, in whatever
// arrangement the underlying library happened to print them (bare host,
// host:port, user@host, user@host:port — all registered below).
//
// # What is deliberately NOT redacted
//
// The artifact/source identifier a caller logs alongside the message (for
// example "cicd-pipeline/gitea-forge-dump") is never touched: #295's
// acceptance criteria requires the line to still say what failed, only not
// where to. Nothing about a source's NAME is ever registered as a needle,
// only its endpoint's Host, Port and User.
type Redactor struct {
	needles []string
}

// NewRedactor builds a Redactor from every endpoint an operator has opted
// into redacting (config.Remote.Sensitive). It returns nil, not an empty
// Redactor, when the given endpoints contribute no needle at all (an
// empty slice, or every field on every Endpoint empty): a nil *Redactor
// is deliberately the same "do nothing" value Filter already treats
// specially, so a deployment that has not marked any remote sensitive
// pays no cost beyond a nil check and behaves exactly as it did before
// this type existed.
func NewRedactor(endpoints ...Endpoint) *Redactor {
	r := &Redactor{}
	for _, e := range endpoints {
		r.add(e)
	}
	if len(r.needles) == 0 {
		return nil
	}
	// Longest first: "host" is always a substring of "user@host", and
	// "host:port" of "user@host:port". Replacing a shorter needle before a
	// longer one that contains it would leave the longer form only
	// partially blanked (a mangled "[REDACTED]:port" fragment) instead of
	// one clean placeholder, since the shorter needle's later pass would
	// find nothing left of itself to match.
	sort.Slice(r.needles, func(i, j int) bool { return len(r.needles[i]) > len(r.needles[j]) })
	return r
}

// add registers every needle shape e's fields can build: user@host:port,
// user@host, host:port, host, and the bare user, skipping whichever of
// those a missing field makes impossible to form. Duplicate needles
// (two endpoints sharing a host, say) are not registered twice.
func (r *Redactor) add(e Endpoint) {
	host := strings.TrimSpace(e.Host)
	user := strings.TrimSpace(e.User)
	if host == "" && user == "" {
		return
	}

	var hostPort string
	if host != "" && e.Port != 0 {
		hostPort = host + ":" + strconv.Itoa(e.Port)
	}

	if user != "" && hostPort != "" {
		r.addNeedle(user + "@" + hostPort)
	}
	if user != "" && host != "" {
		r.addNeedle(user + "@" + host)
	}
	r.addNeedle(hostPort)
	r.addNeedle(host)
	r.addNeedle(user)
}

func (r *Redactor) addNeedle(s string) {
	if s == "" {
		return
	}
	for _, existing := range r.needles {
		if existing == s {
			return
		}
	}
	r.needles = append(r.needles, s)
}

// Filter returns s with every needle this Redactor was built from replaced
// by the redacted placeholder ("[REDACTED]", the same value obs.Secret
// renders as). A nil Redactor, or one built from no endpoints, returns s
// completely unchanged: no allocation, no scan, so a deployment that has
// not opted any remote into this stays byte-for-byte what it was before
// this package grew this type at all.
//
// Filter is nil-receiver-safe by design: callers throughout this package
// and internal/state call it unconditionally (l.redact.Filter(...),
// j.redact.Load().Filter(...)) rather than branching on whether a Redactor
// is actually configured, which is what keeps redaction a property of the
// one seam it is applied at rather than something every caller has to
// remember to check for first.
func (r *Redactor) Filter(s string) string {
	if r == nil || len(r.needles) == 0 || s == "" {
		return s
	}
	out := s
	for _, n := range r.needles {
		out = strings.ReplaceAll(out, n, redacted)
	}
	return out
}
