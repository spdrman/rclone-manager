package packaging

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// The submission materials half of issue #90 / WP5.4: descriptions,
// icons, screenshots, release notes, privacy disclosures, permission
// rationale, support/source/license materials, and the per-target
// submission checklist that draws on them.
//
// # Why a checklist is checked and not just listed
//
// §73's acceptance criterion is that a store reviewer opening the
// submission finds each of those present "and matches that provider's own
// submission checklist format". A checklist is prose, and prose about
// whether a file exists rots the moment the file moves. So the checklist
// has one machine-readable table with a fixed set of rows, and this file
// holds it to the tree: a row naming a path that is not there fails, a
// required row that is missing fails, and a state outside the fixed set
// fails. What stays human is the surrounding text, which is what a
// reviewer actually reads.
//
// # Why "operator" and "blocked" are states rather than absences
//
// Two of these materials cannot be finished on a laptop. Screenshots need
// the app running on the provider's own hardware (§82), and the licence
// and SBOM inventory are B5.2's (#88) deliverable, not this one's. A
// checklist that simply omitted them would read as complete; a checklist
// that marked them failed would say this work package produced a broken
// bundle. Both are wrong, so the format carries the two honest states and
// the readiness verdict treats them differently from a pass.

// ChecklistState is one row's recorded state.
type ChecklistState string

const (
	// StateReady: the material exists at the recorded path.
	StateReady ChecklistState = "ready"
	// StateOperator: the material is produced on the real platform by
	// the §82 acceptance procedure, which is recorded instead of a file.
	StateOperator ChecklistState = "operator"
	// StateBlocked: another work package owns the material. The row
	// carries the issue.
	StateBlocked ChecklistState = "blocked"
	// StateNotApplicable: this target does not need the material, which
	// only a target with no store can say.
	StateNotApplicable ChecklistState = "not-applicable"
)

// ChecklistRow is one parsed row of a submission checklist table.
type ChecklistRow struct {
	Item  string
	Where string
	State ChecklistState
	// Blocker is the issue a blocked row names.
	Blocker string
}

var (
	checklistRowRe = regexp.MustCompile(`(?m)^\|\s*([a-z0-9-]+)\s*\|\s*([^|]*?)\s*\|\s*([^|]*?)\s*\|\s*$`)
	backtickedRe   = regexp.MustCompile("`([^`]+)`")
	blockerRe      = regexp.MustCompile(`#\d+`)
)

// ParseChecklist reads the machine-readable table out of a submission
// checklist. Rows are keyed by the capability id they satisfy, so the
// checklist and the capability list cannot drift into naming different
// things.
func ParseChecklist(text string) map[string]ChecklistRow {
	out := map[string]ChecklistRow{}
	for _, m := range checklistRowRe.FindAllStringSubmatch(text, -1) {
		item := strings.TrimSpace(m[1])
		if item == "item" || item == "---" {
			continue
		}
		row := ChecklistRow{Item: item, Where: strings.TrimSpace(m[2])}
		state := strings.TrimSpace(strings.ToLower(m[3]))
		if b := blockerRe.FindString(state); b != "" {
			row.Blocker = b
			state = strings.TrimSpace(strings.Replace(state, b, "", 1))
		}
		row.State = ChecklistState(strings.TrimSpace(state))
		if p := backtickedRe.FindStringSubmatch(row.Where); p != nil {
			row.Where = p[1]
		}
		out[item] = row
	}
	return out
}

// CheckChecklist holds one target's checklist to the tree and to the
// capability set: every required item has a row, every row's state is one
// of the four, a ready row's path exists, and a blocked row names the
// issue that owns it.
//
// required is the materials capability ids this target has to account
// for, which is where a target with no store legitimately differs: it
// accounts for a documented workflow rather than for seven store assets.
func CheckChecklist(source, text string, required []string) []Violation {
	var out []Violation
	add := func(detail string) { out = append(out, Violation{source, RuleMissingMaterial, detail}) }

	rows := ParseChecklist(text)
	if len(rows) == 0 {
		add("holds no machine-readable checklist table at all, so nothing in it can be held to the tree; the format is one row per item: `| <item> | <where> | <state> |`")
		return out
	}

	for _, item := range required {
		row, ok := rows[item]
		if !ok {
			add(fmt.Sprintf("has no row for %s; an item a checklist does not mention reads as one nobody needed", backquote(item)))
			continue
		}
		switch row.State {
		case StateReady:
			if row.Where == "" {
				add(fmt.Sprintf("marks %s ready and records nowhere it lives", backquote(item)))
				break
			}
			if _, err := os.Stat(Path(row.Where)); err != nil {
				add(fmt.Sprintf("marks %s ready at %s, which is not in the tree: %v", backquote(item), backquote(row.Where), err))
			}
		case StateOperator:
			if row.Where == "" {
				add(fmt.Sprintf("marks %s an operator step and names no procedure that produces it", backquote(item)))
				break
			}
			if _, err := os.Stat(Path(row.Where)); err != nil {
				add(fmt.Sprintf("marks %s an operator step covered by %s, which is not in the tree: %v", backquote(item), backquote(row.Where), err))
			}
		case StateBlocked:
			if row.Blocker == "" {
				add(fmt.Sprintf("marks %s blocked without naming the issue that owns it, which is indistinguishable from marking it blocked forever", backquote(item)))
			}
		case StateNotApplicable:
		default:
			add(fmt.Sprintf("gives %s state %s, and the four states are ready, operator, blocked #N and not-applicable", backquote(item), backquote(string(row.State))))
		}
	}

	for item := range rows {
		found := false
		for _, want := range required {
			if item == want {
				found = true
				break
			}
		}
		if !found {
			add(fmt.Sprintf("has a row for %s, which is not something this target has to account for; a checklist that grows rows nobody measures stops being a checklist", backquote(item)))
		}
	}

	sortViolations(out)
	return out
}

// CheckMaterial holds one shared submission asset to the shape a store
// listing needs: it is there, it says something, and it covers each
// heading a reviewer looks for.
//
// The length floor is not padding. A description file holding the word
// "TODO" satisfies every "does the file exist" check ever written, and
// the whole reason this work package inspects the artifact rather than a
// document is that a document can say anything.
func CheckMaterial(source, text string, minChars int, required []string) []Violation {
	var out []Violation
	add := func(detail string) { out = append(out, Violation{source, RuleMissingMaterial, detail}) }

	body := strings.TrimSpace(text)
	if len(body) < minChars {
		add(fmt.Sprintf("holds %d characters, and a store listing needs at least %d; a placeholder passes every existence check there is", len(body), minChars))
	}
	lower := strings.ToLower(body)
	for _, marker := range []string{"todo", "tbd", "fixme", "lorem ipsum", "placeholder text"} {
		if strings.Contains(lower, marker) {
			add(fmt.Sprintf("still contains %s, so it is a draft rather than something to hand a reviewer", backquote(marker)))
		}
	}
	for _, want := range required {
		if !strings.Contains(lower, strings.ToLower(want)) {
			add(fmt.Sprintf("never covers %s, which every one of the targeted stores asks a listing to state", backquote(want)))
		}
	}

	sortViolations(out)
	return out
}

var (
	svgSizeRe         = regexp.MustCompile(`(?i)<svg[^>]*\bwidth\s*=\s*["'](\d+)`)
	svgCurrentColorRe = regexp.MustCompile(`(?i)currentcolor`)
	svgTitleRe        = regexp.MustCompile(`(?is)<title>\s*(.*?)\s*</title>`)
)

// CheckStoreIcon holds the shipped icon to what a store listing does with
// it, which is not what an application shell does with it.
//
// In the app the icon is inline SVG inside a themed page, so
// `currentColor` is the right answer and it inherits a sensible colour.
// A store renders the same file on its own page, in its own theme,
// outside any colour context at all: `currentColor` there resolves to the
// initial colour value, and the mark renders as a black shape on the
// store's own background or, on a dark listing, as very nearly nothing.
// It is the one icon defect that is invisible in every place a developer
// looks and visible in the only place that matters.
func CheckStoreIcon(source, text string, minPixels int) []Violation {
	var out []Violation
	add := func(detail string) { out = append(out, Violation{source, RuleMissingMaterial, detail}) }

	if strings.TrimSpace(text) == "" {
		add("is empty, so the listing has no mark at all")
		return out
	}
	if svgCurrentColorRe.MatchString(text) {
		add("paints with `currentColor`, which inherits a colour inside the app and resolves to the initial colour on a store's own listing page: the same file that looks right in the shell renders as a flat black mark, or as nearly nothing on a dark listing")
	}
	if m := svgSizeRe.FindStringSubmatch(text); m == nil {
		add("declares no intrinsic `width`, so a store that lays the file out without a box of its own has nothing to size it from")
	} else if n := atoiOr(m[1], 0); n < minPixels {
		add(fmt.Sprintf("declares an intrinsic width of %d, and the targeted stores render a listing icon at %d; an SVG scales, and a mark drawn for %dpx does not stop looking like a %dpx mark when it is blown up", n, minPixels, n, n))
	}
	if m := svgTitleRe.FindStringSubmatch(text); m == nil || strings.TrimSpace(m[1]) == "" {
		add("carries no `<title>`, which is both the accessible name and what several catalogue front ends use as the image's alt text")
	}

	sortViolations(out)
	return out
}

func atoiOr(s string, fallback int) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return fallback
		}
		n = n*10 + int(r-'0')
	}
	return n
}

// MaterialsCapabilityIDs are the materials capabilities in the order §73
// lists them.
var MaterialsCapabilityIDs = []string{
	"materials-description",
	"materials-icon",
	"materials-screenshots",
	"materials-release-notes",
	"materials-privacy-disclosure",
	"materials-permission-rationale",
	"materials-support-source-license",
}
