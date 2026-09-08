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
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
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
	// "backup-manager", unquoted. Nil when there is no equivalent. Text
	// below is what gets printed; this is what a parser is fed.
	Command []string

	// Gap and GapDetail are why there is no equivalent. Gap is the short
	// form and is empty exactly when Command is not.
	Gap       string
	GapDetail string

	// Placeholder says the command carries a <value in angle brackets>
	// standing in for something this request cannot name on a command
	// line, so the line is not runnable as printed.
	Placeholder bool
}

// Text is the line as it appears in the terminal and in an exported file.
//
// A command is prefixed "$ " and shell-quoted so it is copy-pasteable as
// typed. A gap is prefixed "# " so a filtered export is a shell script
// with its gaps sitting in it as comments, and so somebody can grep for
// them.
func (l Line) Text() string {
	if len(l.Command) == 0 {
		out := "# " + l.Gap + " · " + l.Route
		if l.GapDetail != "" {
			out += "\n#   " + l.GapDetail
		}
		return out
	}
	quoted := make([]string, 0, len(l.Command))
	for _, arg := range l.Command {
		quoted = append(quoted, shellQuote(arg))
	}
	out := "$ " + strings.Join(quoted, " ")
	if l.Placeholder {
		out += "\n#   not runnable as printed: fill in the value in angle brackets"
	}
	return out
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
	if c.gap != "" {
		line.Command = nil
		line.Placeholder = false
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

	// gap turns this build into a gap after the fact, for the case where
	// the request itself is what has no equivalent rather than the route.
	gap string
}

func newCmd(words ...string) *cmd {
	return &cmd{argv: append([]string{"backup-manager"}, words...)}
}

func (c *cmd) arg(v string) *cmd {
	c.argv = append(c.argv, v)
	return c
}

// flag appends --name value.
func (c *cmd) flag(name, value string) *cmd {
	c.argv = append(c.argv, "--"+name, value)
	return c
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
func seconds(v int) string {
	s := (time.Duration(v) * time.Second).String()
	// Duration.String always spells every unit down to seconds, so 48h
	// arrives as "48h0m0s". Trimming the trailing zero units in order
	// leaves "48h", "5m" and "30s" and leaves "1h30m" alone, and every
	// one of those is a value time.ParseDuration takes back.
	if strings.HasSuffix(s, "0s") && s != "0s" {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "0m") && s != "0m" {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// shellQuote makes one argument safe to paste into a shell, and leaves
// alone the ones that need nothing.
//
// A placeholder is deliberately left unquoted: the line carrying one
// already says it is not runnable as printed, and '<the file you chose>'
// reads like a filename somebody could use.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">") {
		return s
	}
	if strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune("_@%+=:,./-", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// EnvironmentPreamble is the one thing every command below needs beyond
// its own argv, said once by the panel rather than repeated on every line.
//
// The password is a placeholder rather than a value for the reason
// usage() already gives for these being environment variables at all: a
// password on a command line is in every process listing on the host. And
// this panel is exportable and copy-to-clipboard by design, which is the
// same argument one step further along.
func EnvironmentPreamble(apiURL, username string) string {
	if apiURL == "" {
		apiURL = "http://127.0.0.1:8080"
	}
	if username == "" {
		username = "<the administrator you sign in as>"
	}
	return fmt.Sprintf("export BACKUP_MANAGER_API_URL=%s BACKUP_MANAGER_API_USERNAME=%s BACKUP_MANAGER_API_PASSWORD=<your password>",
		shellQuote(apiURL), shellQuote(username))
}
