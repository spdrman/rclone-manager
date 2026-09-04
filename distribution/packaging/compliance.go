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
// A licence the project has not read is a hard refusal rather than a
// warning. The premise behind the Apache-2.0 choice is that every
// component linked into a shipped binary is either permissive or one of
// the non-permissive licences this project accepts on purpose, with what
// that acceptance obliges written down and discharged. That is a fact
// about today's go.mod rather than a property of the project, so
// LicensePolicyComplaints and LicenceObligationComplaints are what keep
// the premise and the conclusion attached to each other.
//
// The graph is not wholly permissive and has not been since rclone's s3
// backend was registered: two MPL-2.0 modules arrive under it and cannot
// be unlinked from it. AcceptedNonPermissiveLicence is the category that
// says so honestly instead of widening permissiveIds, which would have
// cleared the same check by making this package's data file assert
// something false.
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
	// PermissiveIDs is the allowlist of licences that are permissive,
	// and it is the allowlist that makes the policy a refusal.
	// CopyleftIDs is a denylist, and a
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
	// AcceptedNonPermissive is the third category: licences that are
	// NOT permissive and that this project ships under anyway, each
	// with the obligation that acceptance carries. It is deliberately
	// not part of PermissiveIDs, and the reasoning is in
	// AcceptedNonPermissiveLicence's own doc comment.
	AcceptedNonPermissive []AcceptedNonPermissiveLicence `json:"acceptedNonPermissive"`
}

// AcceptedNonPermissiveLicence is one licence that is not permissive and
// that this project ships under on purpose, with what that costs written
// down next to it.
//
// It is a separate category because the alternative was one line: add
// the id to permissiveIds and the refusal goes away. permissiveIds is a
// statement about what a licence IS, and it is the list somebody reads
// to find out whether this graph is permissive. MPL-2.0 is file-level
// weak copyleft, so putting it there would make the one data file whose
// whole job is to be true say something false, in the place it is least
// true.
//
// The shape is the point: an entry here cannot be a bare id. It has to
// name the clause the acceptance triggers, where a recipient obtains
// the licence, where a recipient obtains the source, and which
// artifacts carry that offer. An incomplete entry admits nothing
// (LicensePolicyComplaints refuses the component), and a complete entry
// whose artifacts do not actually carry the offer admits nothing either
// (LicenceObligationComplaints reads them). Accepted here means accepted
// and discharged, or it means refused.
type AcceptedNonPermissiveLicence struct {
	SPDXID string `json:"spdxId"`
	// Scope says how far the licence's reciprocity reaches, in words.
	// "Non-permissive" spans everything from per-file source
	// availability to network-use copyleft, and that difference is the
	// entire decision, so it is recorded rather than left to whoever
	// recognises the id.
	Scope string `json:"scope"`
	// Obligation names the clause and says what somebody has to do
	// about it. A section number on its own is a citation.
	Obligation string `json:"obligation"`
	// LicenceTextURL is where a recipient gets the licence itself.
	// MPL-2.0 §3.1 asks for exactly this in as many words: recipients
	// have to be told how to obtain a copy of the licence, not only
	// that it governs the files.
	LicenceTextURL string `json:"licenceTextUrl"`
	// SourceRetrieval is where a recipient gets the Source Code Form,
	// as a template over one component: {module} and {version} are
	// substituted per component, so the offer names an exact release
	// instead of a project. A template that does not vary by component
	// is refused, because "the source is on GitHub somewhere" does not
	// identify the source that was shipped.
	//
	// {module} and {version} are both escaped the way a Go module
	// proxy escapes them, for Go components. An unescaped capital
	// letter is a 404, and a containment check over a file would pass
	// on that 404 happily.
	SourceRetrieval string `json:"sourceRetrieval"`
	// Components are the exact artifacts this acceptance covers, as
	// "module@version".
	//
	// The acceptance is per RELEASE, not per licence, and this list is
	// what makes that true rather than stated. Without it, accepting
	// MPL-2.0 once would admit every future module that happens to
	// carry the same id, which is a wider allowlist with extra steps.
	// A different version is a different upload: different licence
	// file bytes, different notices, nobody has read either, and its
	// source address is a different address. So a version bump has to
	// come back through here, which is the same reasoning permissiveIds
	// is built on, one level finer.
	Components []string `json:"components"`
	// DischargedBy are the repository paths that carry the offer to a
	// recipient. Recording an obligation is not discharging it, so
	// each of these is read and checked for the id, for the licence
	// text address, and for every affected component at its exact
	// version with its own resolved source address.
	DischargedBy []string `json:"dischargedBy"`
	// Rationale is why this acceptance was taken rather than the
	// alternatives, for the reader who wants the decision and not the
	// mechanism.
	Rationale []string `json:"rationale"`
}

// incompleteBecause says why an acceptance records too little to stand,
// or returns "" when it records enough.
func (a AcceptedNonPermissiveLicence) incompleteBecause() string {
	switch {
	case strings.TrimSpace(a.SPDXID) == "":
		return "names no licence, so it accepts nothing in particular"
	case strings.TrimSpace(a.Scope) == "":
		return "names no scope, so a reader cannot tell how far the licence reaches"
	case strings.TrimSpace(a.Obligation) == "":
		return "names no obligation, which makes it an allowlist entry with a longer name"
	case strings.TrimSpace(a.LicenceTextURL) == "":
		return "says nowhere a recipient can obtain the licence text"
	case strings.TrimSpace(a.SourceRetrieval) == "":
		return "says nowhere a recipient can obtain the source"
	case !strings.Contains(a.SourceRetrieval, "{module}") || !strings.Contains(a.SourceRetrieval, "{version}"):
		return fmt.Sprintf("gives sourceRetrieval as %q, which does not vary by component; an offer that names a project rather than a release does not identify the source that shipped", a.SourceRetrieval)
	case len(a.Components) == 0:
		return "names no component, so it accepts a licence rather than the releases somebody read, and every future module carrying the same id with it"
	case len(a.DischargedBy) == 0:
		return "names no artifact that carries the offer, so the obligation is recorded and nothing discharges it"
	}
	return ""
}

// Accepts reports whether this acceptance covers one exact release.
func (a AcceptedNonPermissiveLicence) Accepts(comp Component) bool {
	return containsFold(a.Components, comp.Name+"@"+comp.Version)
}

// Covers reports whether this acceptance is the one for an SPDX id.
func (a AcceptedNonPermissiveLicence) Covers(spdxID string) bool {
	return strings.EqualFold(strings.TrimSpace(a.SPDXID), strings.TrimSpace(spdxID))
}

// SourceURLFor renders the source address for one component.
func (a AcceptedNonPermissiveLicence) SourceURLFor(comp Component) string {
	module, version := comp.Name, comp.Version
	if comp.Ecosystem == EcosystemGo {
		// Both halves, not just the path. golang.org/x/mod's
		// EscapeVersion runs the same escapeString as EscapePath, so a
		// pre-release tag like v2.0.0-RC1 needs it exactly as much as
		// a capitalised module path does, and neither of this graph's
		// two MPL versions happens to have an uppercase letter today.
		// That is the reason to do it now rather than when one does:
		// the failure is a rendered URL that 404s, and the check that
		// reads the artifact only asks whether the string is there.
		module, version = EscapeGoModulePath(module), EscapeGoModulePath(version)
	}
	url := strings.ReplaceAll(a.SourceRetrieval, "{module}", module)
	return strings.ReplaceAll(url, "{version}", version)
}

// EscapeGoModulePath applies the module proxy's case encoding: an
// uppercase letter becomes "!" followed by its lowercase form. Proxy
// paths are case-folded, and the escape is how two modules differing
// only in case stay distinct on a case-insensitive filesystem.
//
// This is not hypothetical here. github.com/IBM/go-sdk-core/v5 and
// github.com/Max-Sum/base32768 are both in this project's own linked
// graph, and unescaped their proxy URLs are 404s: I asked
// proxy.golang.org both ways and the escaped path answers 200. A source
// offer that renders a 404 is worse than none, because the check that
// reads the artifact only asks whether the string is present.
//
// It is deliberately the same function for a path and for a version,
// because golang.org/x/mod's EscapePath and EscapeVersion are the same
// function too (both call escapeString). Only ASCII uppercase is
// touched; a module path or version that reaches here with a "!" or a
// non-ASCII rune in it would already have been rejected upstream by
// CheckPath, and go list would never have produced one.
func EscapeGoModulePath(path string) string {
	var b strings.Builder
	b.Grow(len(path) + 8)
	for i := 0; i < len(path); i++ {
		if c := path[i]; c >= 'A' && c <= 'Z' {
			b.WriteByte('!')
			b.WriteByte(c + ('a' - 'A'))
			continue
		}
		b.WriteByte(path[i])
	}
	return b.String()
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
	// LicenceDelivery is how a recipient of THIS target gets the
	// licence, the notices and the third-party inventory. Empty is the
	// same kind of refusal as an unbuilt target with no reason: the
	// answer was "the image is shared, so probably the image", written
	// in a note about something else and true of ten targets out of
	// eleven. See LicenceDeliveryComplaints.
	LicenceDelivery string `json:"licenceDelivery"`
}

// LicenceDelivery is what "the image carries them" means, in the form
// the check can compare against container/Dockerfile.
//
// ImagePaths is repository path to in-image path, and it is a
// declaration that has to agree with what the runtime stage actually
// COPYs in both directions: a Dockerfile edited without this file is a
// recipient told to look somewhere the file is not, and this file edited
// without the Dockerfile is the same sentence from the other end.
type LicenceDelivery struct {
	Note       []string          `json:"note"`
	ImagePaths map[string]string `json:"imagePaths"`
	Labels     map[string]string `json:"labels"`
}

// Distribution is every shipping path.
type Distribution struct {
	Note            []string                      `json:"note"`
	LicenceDelivery LicenceDelivery               `json:"licenceDelivery"`
	Targets         map[string]DistributionTarget `json:"targets"`
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

// AcceptedNonPermissive returns the recorded acceptance for an SPDX id.
//
// It returns the entry rather than a bool so a caller cannot treat
// "accepted" as the end of the question: the entry is what says whether
// the acceptance records enough to stand.
func (c Compliance) AcceptedNonPermissive(spdxID string) (AcceptedNonPermissiveLicence, bool) {
	for _, a := range c.License.AcceptedNonPermissive {
		if a.Covers(spdxID) {
			return a, true
		}
	}
	return AcceptedNonPermissiveLicence{}, false
}

// AcceptedNonPermissiveIDs returns the accepted non-permissive ids in
// declaration order, for a message that has to list them.
func (c Compliance) AcceptedNonPermissiveIDs() []string {
	out := make([]string, 0, len(c.License.AcceptedNonPermissive))
	for _, a := range c.License.AcceptedNonPermissive {
		out = append(out, a.SPDXID)
	}
	return out
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
// The premise behind Apache-2.0 here is that every linked component is
// either permissive or one of the exact RELEASES this project accepts
// under a non-permissive licence, with the obligation recorded. This is
// that premise, executable. A component whose licence could not be
// identified is refused too: an unidentified licence is not evidence of
// a permissive one, and treating it as one is how a graph silently
// acquires an obligation nobody accepted.
//
// Releases rather than licences, because an acceptance keyed on an SPDX
// id would admit every future module carrying that id, and a version
// bump would inherit an acceptance somebody gave a different upload.
//
// It is pure, and it judges the DECLARATION of an acceptance rather than
// its discharge, because whether the NOTICE a recipient receives
// actually carries the offer is a question about the tree and not about
// compliance.json. LicenceObligationComplaints is that half.
//
// The acceptance is per release, and this half enforces that itself: an
// entry names the exact releases it covers, so a brand-new module under
// an accepted licence, or a recorded module at a version nobody read, is
// refused here with no filesystem in the way. Measured both ways: with
// the acceptance keyed on the id alone, vault-client-go@v0.4.3 and
// go-retryablehttp bumped to v0.7.9 got zero complaints from this
// function; with the release list they get one each.
//
// Never call this on its own to decide whether a graph is acceptable,
// even so. What it cannot see is whether the artifacts carry the offer:
// bump a recorded module, update the release list to match, and this
// function is green while docs/compliance/source-offer.md still offers
// the old version to a recipient. LicenceComplaints runs both halves and
// is the entry point.
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
		case c.IsPermissive(comp.LicenseID):
			// Permitted by being named on the allowlist, which is
			// the whole of what the allowlist means.
		default:
			accepted, ok := c.AcceptedNonPermissive(comp.LicenseID)
			if !ok {
				out = append(out, fmt.Sprintf("%s@%s is %s, %s", comp.Name, comp.Version, comp.LicenseID, c.whyNotPermitted(comp.LicenseID)))
				break
			}
			if why := accepted.incompleteBecause(); why != "" {
				out = append(out, fmt.Sprintf("%s@%s is %s, which compliance.json accepts as non-permissive, and the acceptance %s; a licence accepted without recording what it obliges is a wider allowlist wearing a longer name",
					comp.Name, comp.Version, comp.LicenseID, why))
				break
			}
			if !accepted.Accepts(comp) {
				out = append(out, fmt.Sprintf("%s@%s is %s, and compliance.json accepts that licence for %s and not for this release; a different version is a different upload with its own licence bytes, its own notices and its own source address, so it comes back through acceptedNonPermissive rather than inheriting an acceptance somebody gave something else",
					comp.Name, comp.Version, comp.LicenseID, strings.Join(accepted.Components, ", ")))
			}
		}
	}
	return out
}

// whyNotPermitted explains a refusal that has already been decided.
func (c Compliance) whyNotPermitted(spdxID string) string {
	if c.IsCopyleft(spdxID) {
		if c.License.CopyleftBlocksTheLicenseChoice {
			return fmt.Sprintf("which is copyleft: the project's %s choice rests on every linked component being permissive or an accepted non-permissive licence whose obligation is written down and discharged, and this component is neither", c.License.SPDXID)
		}
		return fmt.Sprintf("which is copyleft. copyleftBlocksTheLicenseChoice is off in compliance.json and it is refused regardless, because the %s choice rests on the allowlist rather than on a boolean in a data file", c.License.SPDXID)
	}
	accepted := "nothing"
	if ids := c.AcceptedNonPermissiveIDs(); len(ids) > 0 {
		accepted = strings.Join(ids, ", ")
	}
	return fmt.Sprintf("which is not on the permissive allowlist compliance.json declares (%s) and is not one of the non-permissive licences it accepts on purpose (%s); an id nobody has read the terms of is not evidence of a permissive one",
		strings.Join(c.License.PermissiveIDs, ", "), accepted)
}

// LicenceObligationComplaints says every way an accepted non-permissive
// licence's obligation is recorded and not actually discharged.
//
// This is the half that reads the tree. An acceptance whose artifacts do
// not name the licence, do not say where the licence text is, or do not
// give the exact release and the exact address of the source that
// shipped, is an obligation somebody wrote down and nobody met, and this
// project would rather have a red build than a compliance record that
// reads well.
//
// Two rules here are about the mechanism rather than the paperwork. An
// id on permissiveIds AND on acceptedNonPermissive is refused, because
// the allowlist decides first and the obligation recorded here would
// never be looked at again. And an acceptance nothing in the inventory
// is under is refused, because a standing acceptance for a licence the
// graph does not contain is a permission granted in advance, which is
// the one thing this category must never become.
//
// Never call this on its own either. It reads the artifacts for the
// components the inventory carries under an ACCEPTED licence, so a
// component under a licence nobody accepted is not its question, and a
// graph with a GPL module in it produces nothing here. LicenceComplaints
// runs both halves and is the entry point.
//
// One thing the artifact list is not: two independent proofs. NOTICE is
// rendered by buildNotice from the same register this function reads,
// and TestComplianceArtifactsMatchThisTree keeps the checked-in file
// byte-identical to that render, so on a data change the NOTICE arm
// cannot fail. It still earns its place, because it catches a renderer
// change that stops emitting one of the strings a recipient needs. The
// hand-written source-offer.md is the arm that can disagree with the
// register.
func LicenceObligationComplaints(c Compliance, inv Inventory, read ReadFileFunc) []string {
	if read == nil {
		return []string{"no reader was supplied to the obligation check, so no artifact was read and nothing was checked; an unread artifact is not a discharged obligation"}
	}
	var out []string
	for _, a := range c.License.AcceptedNonPermissive {
		if why := a.incompleteBecause(); why != "" {
			out = append(out, fmt.Sprintf("compliance.json accepts %q as non-permissive and the acceptance %s", a.SPDXID, why))
			continue
		}
		if c.IsPermissive(a.SPDXID) {
			out = append(out, fmt.Sprintf("%s is on permissiveIds and on acceptedNonPermissive at once; the allowlist decides first, so the obligation recorded against it would never be checked and the acceptance would be a wider allowlist wearing a longer name", a.SPDXID))
		}
		if LicenseExpressionIsUndecided(a.SPDXID) {
			out = append(out, fmt.Sprintf("acceptedNonPermissive carries %q, which is an expression rather than a decided licence, so it can never match a component id and accepts nothing", a.SPDXID))
			continue
		}
		var affected []Component
		for _, comp := range inv.Components {
			if a.Covers(comp.LicenseID) {
				affected = append(affected, comp)
			}
		}
		if len(affected) == 0 {
			out = append(out, fmt.Sprintf("compliance.json accepts %s as non-permissive and nothing in the inventory is under it; an acceptance with nothing to justify it is a permission granted in advance, so it is removed rather than kept warm", a.SPDXID))
			continue
		}
		// The other direction. A release listed here and gone from the
		// graph is a permission left lying around, and the version bump
		// that removed it is exactly when somebody should have to look
		// at the offer again.
		inInventory := map[string]bool{}
		for _, comp := range affected {
			inInventory[strings.ToLower(comp.Name+"@"+comp.Version)] = true
		}
		for _, want := range a.Components {
			if !inInventory[strings.ToLower(strings.TrimSpace(want))] {
				out = append(out, fmt.Sprintf("compliance.json accepts %s for %s and the inventory has no such release under that licence; an acceptance for something the graph no longer links is a permission left lying around", a.SPDXID, want))
			}
		}
		for _, rel := range a.DischargedBy {
			data, err := read(rel)
			if err != nil {
				out = append(out, fmt.Sprintf("%s's obligation is declared as discharged by %s, which is not in the tree: %v", a.SPDXID, rel, err))
				continue
			}
			body := string(data)
			if !strings.Contains(body, a.SPDXID) {
				out = append(out, fmt.Sprintf("%s never names %s, so it is not what tells a recipient which licence governs those files", rel, a.SPDXID))
			}
			if !strings.Contains(body, a.LicenceTextURL) {
				out = append(out, fmt.Sprintf("%s never gives %s, so a recipient is told %s applies and not how to read it", rel, a.LicenceTextURL, a.SPDXID))
			}
			for _, comp := range affected {
				if !strings.Contains(body, comp.Name+"@"+comp.Version) {
					out = append(out, fmt.Sprintf("%s never names %s@%s, which is %s and is linked into %s", rel, comp.Name, comp.Version, a.SPDXID, strings.Join(comp.LinkedInto, " and ")))
				}
				if url := a.SourceURLFor(comp); !strings.Contains(body, url) {
					out = append(out, fmt.Sprintf("%s never gives %s, so the offer for %s@%s names no address a recipient can fetch that source from", rel, url, comp.Name, comp.Version))
				}
			}
		}
	}
	return out
}

// LicenceComplaints says every way the third-party inventory invalidates
// the project's licence choice, and it is the only function here that
// answers that question on its own.
//
// It is two halves because they need different inputs, and neither is
// closed alone. LicensePolicyComplaints is pure and judges what the
// inventory says against what compliance.json declares: a licence nobody
// accepted, an acceptance that records too little, a module or a version
// the acceptance does not name. What it cannot see is the tree, so an
// offer that still names last release's version passes it. Measured: a
// recorded module bumped with the release list updated in step gets zero
// complaints from it while the written offer is four complaints stale.
// LicenceObligationComplaints reads the artifacts and refuses exactly
// that, but it never looks at a component under a licence that is not
// accepted, so a graph with a GPL module in it gets zero complaints from
// it, measured too. So the pair is the gate, and a caller that runs one
// half has a hole the width of the other.
func LicenceComplaints(c Compliance, inv Inventory, read ReadFileFunc) []string {
	out := LicensePolicyComplaints(c, inv)
	return append(out, LicenceObligationComplaints(c, inv, read)...)
}
