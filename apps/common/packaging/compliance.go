// Compliance metadata: issue #88 (B5.2), docs/EPIC-B-multi-nas.md §73
// Work Package 5.2 and §61.
//
// §73 WP5.2 asks for a licence, third-party notices, a privacy policy, a
// support link and a source or source-offer link, and asks that each one
// "resolve to real, current content". This file is the executable half of
// that: compliance.json declares them once, and the rules here decide
// whether the declaration is true of the tree rather than of anyone's
// memory of it.
//
// Two design choices are worth stating up front, because both are the
// difference between a check and a formality.
//
// A link's public reachability is DERIVED from one recorded fact, the
// source repository's visibility, instead of being asserted per link. Five
// links each carrying their own "yes this resolves" boolean is five places
// to forget, and the answer for all five is the same answer: this
// repository is private, so every link into it is a 404 for a store
// reviewer. One field, one truth, and flipping it is the operator action
// that closes the criterion.
//
// A copyleft dependency is a hard refusal rather than a warning. The
// project's own licence choice (Apache-2.0) is only available because the
// whole linked graph is permissive, and that is a fact about today's
// go.mod, not a property of the project. LicensePolicyComplaints is what
// keeps the premise and the conclusion attached to each other.
package packaging

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

//go:embed compliance.json
var complianceJSON []byte

// VisibilityPublic and VisibilityPrivate are the two values
// sourceRepository.visibility may take. Anything else is a complaint
// rather than a silent third state, because "unknown visibility" read as
// public is precisely the mistake this field exists to stop.
const (
	VisibilityPublic  = "public"
	VisibilityPrivate = "private"
)

// ComplianceProject is the identity a licence header and a store listing
// both need.
type ComplianceProject struct {
	DisplayName   string   `json:"displayName"`
	AppID         string   `json:"appId"`
	Copyright     string   `json:"copyright"`
	CopyrightNote []string `json:"copyrightNote"`
}

// ComplianceLicense is the project's own licence and the artifacts that
// discharge its obligations.
type ComplianceLicense struct {
	SPDXID     string   `json:"spdxId"`
	File       string   `json:"file"`
	NoticeFile string   `json:"noticeFile"`
	Inventory  string   `json:"inventory"`
	Rationale  []string `json:"rationale"`
	// CopyleftBlocksTheLicenseChoice records that a copyleft dependency
	// is a refusal, not a note. It is a field rather than a constant so
	// the test that proves the refusal fires can also prove that turning
	// it off is what stops it firing, which is the difference between a
	// policy and a comment.
	CopyleftBlocksTheLicenseChoice bool `json:"copyleftBlocksTheLicenseChoice"`
	// PermissiveIDs is the allowlist, and it is the allowlist that
	// makes the policy a refusal. CopyleftIDs is a denylist, and a
	// denylist answers "is this one of the licences somebody thought
	// of?", which is not the question: a bare GPL-3.0 out of npm
	// registry metadata, a compound expression, or a recognised but
	// unlisted id such as EUPL-1.2 is on nobody's list and would land
	// in the inventory as an accepted permissive component. So an id
	// is permitted only by being named here, and adding one is a data
	// change somebody has to make while reading the licence.
	PermissiveIDs []string `json:"permissiveIds"`
	// CopyleftIDs no longer decides anything. It makes the complaint
	// say "which is copyleft" instead of "which is not on the
	// allowlist", which is a better message and nothing more. Keeping
	// the two jobs apart is deliberate: editing this list must not be
	// able to turn a refusal into a pass.
	CopyleftIDs []string `json:"copyleftIds"`
}

// SourceRepository is where the source lives and who can see it.
type SourceRepository struct {
	URL            string   `json:"url"`
	Visibility     string   `json:"visibility"`
	VisibilityNote []string `json:"visibilityNote"`
}

// ComplianceLink is one store-facing link.
//
// Exactly one of URL and RepoPath is the thing a reviewer follows;
// RepoPath is a file in this repository (which is also how it is checked
// for substance), URL is an address. MustMention are phrases the target
// has to actually contain, so a file that exists but says nothing is not
// mistaken for content.
type ComplianceLink struct {
	ID                    string   `json:"id"`
	Title                 string   `json:"title"`
	Spec                  string   `json:"spec"`
	URL                   string   `json:"url"`
	RepoPath              string   `json:"repoPath"`
	TargetsThisRepository bool     `json:"targetsThisRepository"`
	MustMention           []string `json:"mustMention"`
}

// DistributionTarget is one shipping path's own artifacts.
//
// Artifacts empty and UnbuiltReason empty is the vacuous case and is
// refused: a checksum manifest over nothing passes by having nothing to
// disagree with.
type DistributionTarget struct {
	Artifacts     []string `json:"artifacts"`
	UnbuiltReason string   `json:"unbuiltReason"`
}

// Distribution is every shipping path.
type Distribution struct {
	Note    []string                      `json:"note"`
	Targets map[string]DistributionTarget `json:"targets"`
}

// PerformanceMetric is one number the provenance bundle carries forward.
// Value is a pointer so "not measured yet" and "measured as zero" are
// different states rather than the same one.
type PerformanceMetric struct {
	ID     string   `json:"id"`
	Unit   string   `json:"unit"`
	Value  *float64 `json:"value"`
	Source string   `json:"source"`
}

// Performance is §81's evidence set.
type Performance struct {
	Note    []string            `json:"note"`
	Metrics []PerformanceMetric `json:"metrics"`
}

// Compliance is compliance.json.
type Compliance struct {
	Project          ComplianceProject `json:"project"`
	License          ComplianceLicense `json:"license"`
	SourceRepository SourceRepository  `json:"sourceRepository"`
	Links            []ComplianceLink  `json:"links"`
	Distribution     Distribution      `json:"distribution"`
	Performance      Performance       `json:"performance"`
}

// LoadCompliance parses the embedded compliance.json.
func LoadCompliance() (Compliance, error) {
	var c Compliance
	if err := json.Unmarshal(complianceJSON, &c); err != nil {
		return Compliance{}, fmt.Errorf("packaging: parse compliance.json: %w", err)
	}
	return c, nil
}

// MustLoadCompliance is LoadCompliance for callers that cannot proceed
// without it.
func MustLoadCompliance() Compliance {
	c, err := LoadCompliance()
	if err != nil {
		panic(err)
	}
	return c
}

// StoreReadyForPublicLinks reports whether a store reviewer outside this
// project could follow the declared links and see anything.
//
// It is one derived answer rather than five recorded ones, and it is
// deliberately pessimistic about a visibility value it does not
// recognise: an unparsed string read as "public" is how a compliance
// claim gets made on no evidence at all.
func (c Compliance) StoreReadyForPublicLinks() bool {
	return c.SourceRepository.Visibility == VisibilityPublic
}

// Link returns the declared link with this id.
func (c Compliance) Link(id string) (ComplianceLink, bool) {
	for _, l := range c.Links {
		if l.ID == id {
			return l, true
		}
	}
	return ComplianceLink{}, false
}

// LinkIDs returns the declared link ids in declaration order.
func (c Compliance) LinkIDs() []string {
	out := make([]string, 0, len(c.Links))
	for _, l := range c.Links {
		out = append(out, l.ID)
	}
	return out
}

// TargetIDs returns the declared distribution targets, sorted.
func (c Compliance) TargetIDs() []string {
	out := make([]string, 0, len(c.Distribution.Targets))
	for id := range c.Distribution.Targets {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// IsCopyleft reports whether an SPDX id is on the declared copyleft list.
// It decides the wording of a complaint, never whether there is one.
func (c Compliance) IsCopyleft(spdxID string) bool {
	return containsFold(c.License.CopyleftIDs, spdxID)
}

// IsPermissive reports whether an SPDX id is on the declared permissive
// allowlist. This is the one that decides.
func (c Compliance) IsPermissive(spdxID string) bool {
	return containsFold(c.License.PermissiveIDs, spdxID)
}

func containsFold(list []string, want string) bool {
	want = strings.TrimSpace(want)
	for _, id := range list {
		if strings.EqualFold(strings.TrimSpace(id), want) {
			return true
		}
	}
	return false
}

// undecidedLicenseMarkers are the substrings that make an SPDX string a
// question rather than an answer. A choice ("(MIT OR GPL-3.0)"), a
// conjunction, an exception ("GPL-2.0-or-later WITH
// Classpath-exception-2.0") and npm's "SEE LICENSE IN LICENSE" all
// describe terms nobody has decided between, and every one of them is
// non-empty, so the unidentified-licence rule below does not catch them.
var undecidedLicenseMarkers = []string{" OR ", " AND ", " WITH ", "(", ")", "SEE LICENSE"}

// LicenseExpressionIsUndecided reports whether an SPDX string states a
// licence or only the range of licences something might be under.
func LicenseExpressionIsUndecided(spdxID string) bool {
	upper := strings.ToUpper(spdxID)
	for _, marker := range undecidedLicenseMarkers {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

// minimumLinkBody is the length below which a "policy" is a placeholder.
// The number matches the one #90's preflight already uses for its
// listing materials, so a reviewer meets one bar rather than two.
const minimumLinkBody = 400

// ReadFileFunc reads a repository-relative path. Injected so the link
// rules can be driven against constructed content instead of only
// against the tree, which is the difference between a rule that has been
// seen to refuse and one that has only ever been seen to pass.
type ReadFileFunc func(rel string) ([]byte, error)

// RepoReader reads from the real repository.
func RepoReader() ReadFileFunc {
	return func(rel string) ([]byte, error) { return os.ReadFile(Path(rel)) }
}

// LinkComplaints says every way the declared links fail §73 WP5.2's
// "each resolves to real, current content".
//
// It returns complaints rather than a bool so a caller can report all of
// them at once, and so a table test can assert WHICH one fired. A rule
// that only reports that something was wrong cannot distinguish "the
// privacy policy is missing" from "the privacy policy exists and never
// mentions telemetry", and those need different work.
func LinkComplaints(c Compliance, read ReadFileFunc) []string {
	var out []string
	if len(c.Links) == 0 {
		return []string{"compliance.json declares no links at all, so the link check has nothing to follow and passes by default"}
	}
	switch c.SourceRepository.Visibility {
	case VisibilityPublic, VisibilityPrivate:
	default:
		out = append(out, fmt.Sprintf("sourceRepository.visibility is %q, which is neither %q nor %q; an unrecognised visibility read as public is a compliance claim made on no evidence",
			c.SourceRepository.Visibility, VisibilityPublic, VisibilityPrivate))
	}
	seen := map[string]bool{}
	for _, l := range c.Links {
		if seen[l.ID] {
			out = append(out, fmt.Sprintf("link %q is declared twice, so one of the two decides nothing", l.ID))
			continue
		}
		seen[l.ID] = true

		if l.URL == "" && l.RepoPath == "" {
			out = append(out, fmt.Sprintf("link %q names neither a URL nor a repository path, so there is nothing for a reviewer to follow", l.ID))
			continue
		}
		if l.URL != "" {
			if !strings.HasPrefix(l.URL, "https://") {
				out = append(out, fmt.Sprintf("link %q points at %q, which is not https", l.ID, l.URL))
			}
			if l.TargetsThisRepository && !strings.HasPrefix(l.URL, c.SourceRepository.URL) {
				out = append(out, fmt.Sprintf("link %q claims to target this repository and points at %q, which is not under %s", l.ID, l.URL, c.SourceRepository.URL))
			}
		}
		if l.RepoPath == "" {
			continue
		}
		data, err := read(l.RepoPath)
		if err != nil {
			out = append(out, fmt.Sprintf("link %q points at %s, which is not in the tree: %v", l.ID, l.RepoPath, err))
			continue
		}
		if len(data) < minimumLinkBody {
			out = append(out, fmt.Sprintf("link %q points at %s, which is %d bytes; under %d bytes is a placeholder, not content a reviewer can act on",
				l.ID, l.RepoPath, len(data), minimumLinkBody))
			continue
		}
		body := string(data)
		for _, phrase := range l.MustMention {
			if !strings.Contains(body, phrase) {
				out = append(out, fmt.Sprintf("link %q points at %s, which never mentions %q, so it does not cover what this link promises",
					l.ID, l.RepoPath, phrase))
			}
		}
	}
	return out
}

// LicensePolicyComplaints says every way the third-party inventory
// invalidates the project's own licence choice.
//
// The premise behind Apache-2.0 here is that nothing in the graph is
// copyleft. This is that premise, executable. A component whose licence
// could not be identified is refused too: an unidentified licence is not
// evidence of a permissive one, and treating it as one is how a graph
// silently acquires an obligation nobody accepted.
func LicensePolicyComplaints(c Compliance, inv Inventory) []string {
	var out []string
	if len(inv.Components) == 0 {
		return []string{"the third-party inventory lists no components at all, so the licence policy has nothing to judge and passes by default"}
	}
	if inv.ProjectLicense != c.License.SPDXID {
		out = append(out, fmt.Sprintf("the inventory says the project is under %q and compliance.json says %q; a NOTICE generated against the wrong licence discharges nothing",
			inv.ProjectLicense, c.License.SPDXID))
	}
	if len(c.License.PermissiveIDs) == 0 {
		return []string{"compliance.json declares no permissive licence allowlist, so every component would be judged against an empty list; an empty reading is a refusal, not a pass"}
	}
	for _, comp := range inv.Components {
		switch {
		case comp.Version == "":
			out = append(out, fmt.Sprintf("%s (%s) is listed with no version, so nobody can tell which release's terms were read", comp.Name, comp.Ecosystem))
		case comp.LicenseID == "":
			out = append(out, fmt.Sprintf("%s@%s has no identified licence; an unidentified licence is not evidence of a permissive one", comp.Name, comp.Version))
		case LicenseExpressionIsUndecided(comp.LicenseID):
			out = append(out, fmt.Sprintf("%s@%s is listed as %q, which is an expression rather than a decided licence; a choice between licences is not evidence that the permissive branch is the one this project takes",
				comp.Name, comp.Version, comp.LicenseID))
		case !c.IsPermissive(comp.LicenseID):
			out = append(out, fmt.Sprintf("%s@%s is %s, %s", comp.Name, comp.Version, comp.LicenseID, c.whyNotPermitted(comp.LicenseID)))
		}
	}
	return out
}

// whyNotPermitted explains a refusal that has already been decided.
func (c Compliance) whyNotPermitted(spdxID string) string {
	if c.IsCopyleft(spdxID) {
		if c.License.CopyleftBlocksTheLicenseChoice {
			return fmt.Sprintf("which is copyleft: the project's %s choice rests on the whole linked graph being permissive, and this component is the counter-example", c.License.SPDXID)
		}
		return fmt.Sprintf("which is copyleft. copyleftBlocksTheLicenseChoice is off in compliance.json and it is refused regardless, because the %s choice rests on the allowlist rather than on a boolean in a data file", c.License.SPDXID)
	}
	return fmt.Sprintf("which is not on the permissive allowlist compliance.json declares (%s); an id nobody has read the terms of is not evidence of a permissive one",
		strings.Join(c.License.PermissiveIDs, ", "))
}
