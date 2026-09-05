package spk

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Proving that no shipped lifecycle script can delete anything outside the
// package's own footprint.
//
// This is a shell reader, which is an uncomfortable thing to have in a Go
// package, and the alternative was worse. These scripts run as root on
// somebody's NAS during install, upgrade and uninstall, and a wrong path
// in one of them removes a shared folder. Nothing else in the toolchain
// looks at them at all, so the choice was between a partial static
// analysis and nothing.
//
// It is partial on purpose and errs towards refusing. Anything it cannot
// resolve to a path inside the allowed prefixes is a finding rather than
// an assumed-safe line, so a script written in a way this reader does not
// understand fails the check and has to be rewritten in a way it does.
// That is the correct direction for a check whose false negative is a
// deleted volume.
//
// The prefix list is short and each absence from it is argued at the list
// itself, because the tempting additions are the dangerous ones: a
// variable Synology does not document expands to empty on a build that
// does not export it, and a delete rooted at an empty string is a delete
// rooted at /.

// footprintPrefixes are the only path roots a shipped script may delete
// inside: the directories DSM replaces on upgrade or removes on
// uninstall, plus the temporary directories the package framework hands
// the script.
//
// Deliberately absent, each for a stated reason:
//
//   - ${SYNOPKG_PKGVAR} and ${SYNOPKG_PKGETC}. Synology documents neither
//     (layout.go, common.sh), which is why the scripts spell the FHS
//     paths out instead of reading them. A package that refuses to trust
//     those variables for reading must not bless them for deleting: on a
//     DSM build that does not export SYNOPKG_PKGVAR,
//     `rm -rf "${SYNOPKG_PKGVAR}/run"` is `rm -rf /run`.
//   - /var/packages/${SYNOPKG_PKGNAME} as a whole. It covers etc/ and
//     var/ by prefix, and postuninst names both as directories that must
//     survive an uninstall, so only its replaceable subtrees are listed.
//   - Any DSM volume, and any shared folder.
var footprintPrefixes = []string{
	"${SYNOPKG_PKGDEST}",
	"${SYNOPKG_PKGINST_TEMP_DIR}",
	"${SYNOPKG_TEMP_UPGRADE_FOLDER}",
	"${SYNOPKG_TEMP_LOGFILE}",
	PkgTargetPath,
	PkgTmpPath,
	PkgVarPath + "/run",
	PkgVarPath + "/log",
}

// mustSurvivePrefixes are inside the package and still never deletable.
// They hold the three things postuninst's comment names as having to
// outlive an uninstall - the configuration, the SQLite lifecycle journal
// and the local-auth administrator record - so a deletion aimed at any
// of them is reported even though it is technically "inside the
// package", and reported before the footprint list is consulted so no
// future prefix can accidentally re-bless one.
//
// The two undocumented variables are listed here as well: written in the
// fail-fast form they clear the leading-reference rule below, and this
// is what still refuses them.
var mustSurvivePrefixes = []string{
	PkgEtcPath,
	PkgHomePath,
	PkgVarPath + "/state",
	"${SYNOPKG_PKGVAR}",
	"${SYNOPKG_PKGETC}",
}

// allowedCommands is every command a shipped lifecycle script may run.
//
// An allowlist rather than a denylist of destructive verbs, for the same
// reason #175's ScanLifecycle refused one: a denylist is defeated by the
// first spelling nobody thought of, and there are many. `find ... |
// xargs rm -f`, `sh -c 'rm -rf ...'`, `: > file`, `dd of=`, `truncate`,
// `mv`, and `synoshare --del` all remove or destroy data, and only two
// of them contain a verb any denylist would carry. These scripts run at
// install and uninstall time with root-adjacent privilege, so the
// friction of having to extend this list is the intended cost.
//
// $@ is here because start-stop-status' start_daemon dispatches the
// argument vector its own start branch built, which names
// ${PKG_BIN}/backup-manager-web literally.
var allowedCommands = map[string]bool{
	".": true, "source": true, ":": true, "[": true, "test": true,
	"cat": true, "chmod": true, "chown": true, "cp": true, "dirname": true,
	"echo": true, "exit": true, "hostname": true, "id": true, "kill": true,
	"mkdir": true, "mv": true, "printf": true, "ps": true, "read": true,
	"return": true, "rm": true, "rmdir": true, "set": true, "shift": true,
	"sleep": true, "tail": true, "tr": true, "true": true, "false": true,
	"unlink": true, "wc": true, "$@": true,
}

// shellKeywords are the words that introduce a compound command rather
// than name a program.
var shellKeywords = map[string]bool{
	"if": true, "then": true, "elif": true, "else": true, "fi": true,
	"while": true, "until": true, "do": true, "done": true,
	"esac": true, "{": true, "}": true, "!": true, "time": true,
}

// blockOpeners take a word operand that is not a command, so the whole
// command is skipped rather than mis-read as running "$1" or "x".
var blockOpeners = map[string]bool{"case": true, "for": true, "select": true}

// deletingCommands are the allowed verbs whose operands are paths they
// remove or overwrite, so every operand has to resolve into the
// footprint.
var deletingCommands = map[string]bool{
	"rm": true, "rmdir": true, "unlink": true, "mv": true,
}

// recursiveFlags mark the chmod/chown forms that are destructive on a
// wrong path rather than merely wrong.
var recursiveFlags = map[string]bool{"-R": true, "-r": true, "--recursive": true}

// The shell patterns the scanner recognises.
//
// This is deliberately a small set of regexps rather than a parser. A real
// shell grammar would be more accurate and would also be a second
// implementation of sh living in this repository, which is far more code
// to get wrong than the scripts it is checking. What makes the trade-off
// safe is the direction of failure: a line these do not decompose into a
// verb and its operands is reported rather than skipped, so the inaccuracy
// costs a rewrite of a script and never a missed deletion.
//
// Several of them exist to REMOVE structure rather than to find it.
// Arithmetic expansions, command substitutions and file-descriptor
// duplications are stripped before the line is split into words, because
// each of them contains characters that would otherwise be read as
// operands, and an operand this scanner invents is a finding nobody can
// act on.
var (
	assignmentRE   = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)
	fieldAssignRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	funcDefRE      = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_]*)\s*\(\s*\)\s*\{?\s*$`)
	caseLabelRE    = regexp.MustCompile(`^\s*[^()\s]*\)\s*`)
	arithmeticRE   = regexp.MustCompile(`\$\(\([^()]*\)\)`)
	substitutionRE = regexp.MustCompile("\\$\\(([^()]*)\\)|`([^`]*)`")
	fdDupRE        = regexp.MustCompile(`[0-9]?>&[0-9-]+`)

	// varRefRE matches one parameter expansion: ${NAME}, ${NAME:?why},
	// ${NAME:-default} or the unbraced $NAME.
	varRefRE = regexp.MustCompile(`\$(?:\{([A-Za-z_][A-Za-z0-9_]*)(:[-=?+][^}]*)?\}|([A-Za-z_][A-Za-z0-9_]*))`)
)

// ScanForUnsafeDeletes reads a shell script and returns one finding per
// command that is not on the allowlist, per deletion whose target is not
// provably inside the package's own footprint, and per redirection that
// truncates a file outside it.
//
// This is the static half of issue #85's uninstall/retained-data safety
// criterion. The hardware half puts a canary in the backup share and
// diffs the share across an uninstall
// (docs/acceptance/synology-dsm-package-lifecycle.md step 5); this half
// runs on every commit and answers a narrower question that is still
// worth answering: is there any deletion written down here at all whose
// target could be outside the package?
//
// It is deliberately conservative rather than clever. A target it cannot
// resolve to a footprint path is reported, including one hidden behind a
// variable this scanner never saw assigned - "I could not tell" and "it
// is fine" must not produce the same answer in a check about deleting
// somebody's backups.
//
// The shared library's assignments come from the embedded copy, which is
// the right source at build time. Verify uses ScanShippedScript instead,
// with the copy out of the archive it is checking.
func ScanForUnsafeDeletes(scriptName, body string) []string {
	shared, err := assetFS.ReadFile("assets/scripts/" + SharedScriptName)
	if err != nil {
		shared = nil
	}
	return ScanShippedScript(scriptName, body, string(shared))
}

// ScanShippedScript is ScanForUnsafeDeletes with common.sh's body passed
// in explicitly.
//
// Three of the lifecycle stages source scripts/common.sh, so a deletion
// written as "${ENGINE_PID}" is only resolvable with that file's
// assignments in hand. They are merged in first and the script's own
// assignments win, so a stage that redefines a path is read the way the
// shell would read it - and a package whose common.sh was replaced is
// read the way THAT file would run, which is the whole reason this takes
// the body rather than reading the embedded one.
//
// If sharedBody is empty the scan still runs, just with fewer resolvable
// names, which makes it report more and never less.
func ScanShippedScript(scriptName, body, sharedBody string) []string {
	vars := shellAssignments(sharedBody)
	for name, value := range shellAssignments(body) {
		vars[name] = value
	}
	defined := shellFunctions(sharedBody)
	for name := range shellFunctions(body) {
		defined[name] = true
	}

	var findings []string
	report := func(lineNo int, format string, args ...any) {
		findings = append(findings, fmt.Sprintf("%s:%d: %s", scriptName, lineNo, fmt.Sprintf(format, args...)))
	}

	for _, logical := range logicalLines(body) {
		if funcDefRE.MatchString(logical.text) {
			continue
		}
		for _, cmd := range splitShellCommands(logical.text) {
			verb, words, truncated := parseCommand(cmd)

			for _, target := range truncated {
				if !insideFootprint(expandShellVars(target, vars)) {
					report(logical.number, "a redirection truncates %q, which is not provably inside the package footprint (resolved to %q)",
						target, expandShellVars(target, vars))
				}
			}
			if verb == "" {
				continue
			}
			if !allowedCommands[verb] && !defined[verb] {
				report(logical.number, "runs %q, which is not one of the commands a shipped lifecycle script may run; add it to allowedCommands in lifecycle.go if it belongs there", verb)
				continue
			}

			for _, target := range deletionTargets(verb, words) {
				resolved := expandShellVars(target, vars)
				if insideFootprint(resolved) {
					continue
				}
				report(logical.number, "%s deletes %q, which is not provably inside the package footprint (resolved to %q)",
					verb, target, resolved)
			}
		}
	}
	return findings
}

// deletionTargets returns the operands a command removes or overwrites.
//
// cp is the one asymmetric case: its destination is deliberately NOT
// checked, because postinst's config seed copies onto ${CONFIG_FILE},
// which is under etc/ and must-survive by design, and the guard that
// makes that safe (`if [ ! -f ... ]`) is a control-flow fact this
// scanner cannot see. What is checked is the one cp form that exists to
// destroy a file rather than to write one.
func deletionTargets(verb string, words []string) []string {
	switch {
	case deletingCommands[verb]:
		return operandsOf(words)
	case verb == "cp":
		operands := operandsOf(words)
		if len(operands) > 1 && operands[0] == "/dev/null" {
			return operands[1:]
		}
		return nil
	case verb == "chmod" || verb == "chown":
		for _, w := range words {
			if recursiveFlags[w] {
				return operandsOf(words)
			}
		}
		return nil
	}
	return nil
}

// insideFootprint reports whether a resolved path is provably under one
// of the package's own replaceable directories.
func insideFootprint(target string) bool {
	if target == "" || strings.Contains(target, "..") {
		return false
	}
	// A target that BEGINS with a variable this scanner never saw
	// assigned is only safe in the ${NAME:?why} form, where the shell
	// refuses to run rather than expanding it to nothing. Without that,
	// one unexported variable turns a deletion under the package into a
	// deletion at the filesystem root.
	if leading, failFast := leadingVarRef(target); leading && !failFast {
		return false
	}
	normalized := normalizeVarRefs(target)
	for _, prefix := range mustSurvivePrefixes {
		if normalized == prefix || strings.HasPrefix(normalized, prefix+"/") {
			return false
		}
	}
	for _, prefix := range footprintPrefixes {
		if normalized == prefix || strings.HasPrefix(normalized, prefix+"/") {
			return true
		}
	}
	return false
}

// leadingVarRef reports whether target starts with a parameter
// expansion, and whether that expansion is the fail-fast form.
func leadingVarRef(target string) (leading, failFast bool) {
	m := varRefRE.FindStringSubmatchIndex(target)
	if m == nil || m[0] != 0 {
		return false, false
	}
	groups := varRefRE.FindStringSubmatch(target)
	return true, strings.HasPrefix(groups[2], ":?")
}

// normalizeVarRefs rewrites every surviving parameter expansion to
// ${NAME}, so $NAME, ${NAME} and ${NAME:?why} all prefix-match the same
// entry rather than needing three spellings in every list.
func normalizeVarRefs(s string) string {
	return varRefRE.ReplaceAllStringFunc(s, func(ref string) string {
		return "${" + refName(ref) + "}"
	})
}

func refName(ref string) string {
	m := varRefRE.FindStringSubmatch(ref)
	if m[1] != "" {
		return m[1]
	}
	return m[3]
}

// shellAssignments collects simple NAME=value assignments so a target
// written as "${PKG_VAR}/run/engine.pid" can be resolved back to the
// documented FHS path common.sh assigned it from.
func shellAssignments(body string) map[string]string {
	vars := map[string]string{}
	for _, logical := range logicalLines(body) {
		m := assignmentRE.FindStringSubmatch(logical.text)
		if m == nil {
			continue
		}
		vars[m[1]] = unquote(strings.TrimSpace(m[2]))
	}
	return vars
}

// shellFunctions collects the names a script defines, so calling one is
// not read as running an unknown program.
func shellFunctions(body string) map[string]bool {
	out := map[string]bool{}
	for _, logical := range logicalLines(body) {
		if m := funcDefRE.FindStringSubmatch(logical.text); m != nil {
			out[m[1]] = true
		}
	}
	return out
}

// expandShellVars substitutes every parameter expansion it has a value
// for, repeatedly, so a variable defined in terms of another resolves.
//
// One regexp pass per round rather than a ReplaceAll per known name: a
// per-name pass makes $_pid and $_pidfile resolve differently depending
// on which key Go's randomised map iteration reached first, and
// start-stop-status assigns both. A safety gate whose verdict changes
// between runs is worse than one that is consistently wrong.
//
// Bounded because a self-referential assignment must not spin.
func expandShellVars(s string, vars map[string]string) string {
	for range 8 {
		before := s
		s = varRefRE.ReplaceAllStringFunc(s, func(ref string) string {
			if value, ok := vars[refName(ref)]; ok {
				return value
			}
			return ref
		})
		if s == before {
			break
		}
	}
	return s
}

// logicalLine is one command line after continuations are joined,
// remembering where it started so a finding points at the line a reader
// will look at.
type logicalLine struct {
	number int
	text   string
}

// logicalLines strips whole-line comments and joins backslash
// continuations. A stage's start_daemon call spans five physical lines;
// read one at a time, four of them look like a command called "--config".
func logicalLines(body string) []logicalLine {
	var out []logicalLine
	pending := ""
	start := 0
	for i, rawLine := range strings.Split(body, "\n") {
		line := rawLine
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			// A trailing comment after real code is left alone on
			// purpose: cutting at the first '#' would truncate a
			// legitimate path or a quoted string, and a comment cannot
			// make a deletion on the same line safe anyway.
			line = ""
		}
		if pending == "" {
			start = i + 1
		}
		if strings.HasSuffix(line, "\\") {
			pending += strings.TrimSuffix(line, "\\") + " "
			continue
		}
		out = append(out, logicalLine{number: start, text: pending + line})
		pending = ""
	}
	if pending != "" {
		out = append(out, logicalLine{number: start, text: pending})
	}
	return out
}

// splitShellCommands breaks a line into the commands it runs, including
// the ones inside a command substitution. It splits on `&` as well as
// `&&`, so a backgrounded destructive command is not read as part of the
// one before it.
func splitShellCommands(line string) []string {
	line = arithmeticRE.ReplaceAllString(line, "")
	var out []string
	line = substitutionRE.ReplaceAllStringFunc(line, func(sub string) string {
		m := substitutionRE.FindStringSubmatch(sub)
		inner := m[1] + m[2]
		out = append(out, splitShellCommands(inner)...)
		return ""
	})
	line = fdDupRE.ReplaceAllString(line, " ")
	replacer := strings.NewReplacer("&&", "\x00", "||", "\x00", ";", "\x00", "|", "\x00", "&", "\x00")
	return append(out, strings.Split(replacer.Replace(line), "\x00")...)
}

// parseCommand reads one command: the program it runs, the words it runs
// it on, and the targets of any redirection that truncates a file.
func parseCommand(cmd string) (verb string, words []string, truncated []string) {
	if m := caseLabelRE.FindString(cmd); m != "" {
		cmd = cmd[len(m):]
	}
	fields, truncated := splitRedirections(shellFields(cmd))
	for len(fields) > 0 {
		switch {
		case fields[0] == "":
			fields = fields[1:]
		case shellKeywords[fields[0]]:
			fields = fields[1:]
		case blockOpeners[fields[0]]:
			// `case "$1" in` and `for x in ...` take a word, not a
			// command, so there is nothing here to check.
			return "", nil, truncated
		case fieldAssignRE.MatchString(fields[0]):
			fields = fields[1:]
		default:
			return path.Base(fields[0]), fields[1:], truncated
		}
	}
	return "", nil, truncated
}

// splitRedirections separates redirections from the words a command acts
// on, and returns the targets of the ones that truncate. Appends are not
// truncations: both daemons' logs are opened with >>.
func splitRedirections(fields []string) (words, truncated []string) {
	for i := 0; i < len(fields); i++ {
		op, target, ok := redirection(fields[i])
		if !ok {
			words = append(words, fields[i])
			continue
		}
		if target == "" && i+1 < len(fields) {
			i++
			target = fields[i]
		}
		if op == ">" && target != "" && target != "/dev/null" {
			truncated = append(truncated, target)
		}
	}
	return words, truncated
}

// redirection recognises a redirection token and says whether it
// truncates its target.
func redirection(field string) (op, target string, ok bool) {
	rest := strings.TrimLeft(field, "0123456789")
	switch {
	case strings.HasPrefix(rest, ">>"):
		return ">>", strings.TrimPrefix(rest, ">>"), true
	case strings.HasPrefix(rest, ">|"):
		return ">", strings.TrimPrefix(rest, ">|"), true
	case strings.HasPrefix(rest, ">"):
		return ">", strings.TrimPrefix(rest, ">"), true
	case strings.HasPrefix(rest, "<"):
		return "<", strings.TrimPrefix(rest, "<"), true
	}
	return "", "", false
}

// shellFields splits on whitespace and removes quoting, which is enough
// for the deliberately plain shell these scripts are written in.
func shellFields(cmd string) []string {
	fields := strings.Fields(cmd)
	for i := range fields {
		fields[i] = unquote(fields[i])
	}
	return fields
}

// operandsOf drops option flags, leaving the paths a command acts on.
func operandsOf(fields []string) []string {
	var out []string
	for _, f := range fields {
		if f == "" || strings.HasPrefix(f, "-") {
			continue
		}
		out = append(out, f)
	}
	return out
}

func unquote(s string) string {
	for _, q := range []string{`"`, `'`} {
		if len(s) >= 2 && strings.HasPrefix(s, q) && strings.HasSuffix(s, q) {
			return s[1 : len(s)-1]
		}
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, `"`, ""), `'`, "")
}
