// FR-35 clause 3, "identical API responses except for additive fields",
// turned into two cells a machine can decide.
//
// Neither one stands a server up. Comparing response bodies would need a
// running deployment and would still only cover the shapes some fixture
// happened to reach, whereas api/v1/openapi.json is where the promise
// actually lives: every binding on both sides of the wire is generated
// from it and check-contract-drift.sh already holds them to it. Pinning
// the contract therefore pins the responses, and does it exhaustively.
//
// The two cells exist because one comparison cannot answer both halves of
// the clause. A flat list of promises compared additively lets EPIC E add
// endpoints and fields, and refuses anything removed, retyped or made
// required. That list cannot see the other break: an operation that gains
// a requirement breaks every caller that already worked, and a gained
// requirement is an addition. So the second cell puts each operation's and
// each request schema's whole requirement set on one line, where gaining
// one rewrites an existing line instead of adding a new one.
//
// Several projection choices below (one line per enum value, "(none)" for
// an empty requirement set) exist because the first main this gate met
// reported ordinary additive changes as breaks. Each is argued where it is
// made.
package compat

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// captureAPIContract projects api/v1/openapi.json down to the list of
// promises it makes, one promise per line, sorted.
//
// This is FR-35's third clause, "identical API responses except for
// additive fields", turned into something a machine can decide. Comparing
// response BODIES would have meant standing a server up and would still
// only have covered the handful of shapes a fixture happens to reach; the
// contract is where the promise actually lives, every binding on both
// sides of the wire is generated from it, and check-contract-drift.sh
// already guarantees the bindings match it. So pinning the contract pins
// the responses.
//
// Additive-only is the whole point. A new endpoint, a new optional field,
// a new schema: all of those add a line and pass, which is exactly the
// room FR-35 leaves EPIC E. A removed field, a changed type, or an
// optional field that became required all take an existing line away, and
// there is no way to spell any of them that this cell lets through.
//
// info.version is deliberately not projected. A contract version bump is a
// release decision that accompanies additive changes as often as breaking
// ones, and a cell that goes red on every minor bump is a cell people
// learn to regenerate without reading.
func captureAPIContract(contractPath string) (Cell, Cell, error) {
	blob, err := os.ReadFile(contractPath)
	if err != nil {
		return Cell{}, Cell{}, err
	}
	var doc map[string]any
	if err := json.Unmarshal(blob, &doc); err != nil {
		return Cell{}, Cell{}, fmt.Errorf("parsing %s: %w", contractPath, err)
	}

	schemas := mapAt(doc, "components", "schemas")
	paths, _ := doc["paths"].(map[string]any)
	if len(schemas) == 0 || len(paths) == 0 {
		return Cell{}, Cell{}, fmt.Errorf("%s declares %d schemas and %d paths; a contract cell built from that would certify nothing",
			contractPath, len(schemas), len(paths))
	}

	var lines []string

	for _, p := range sortedAnyKeys(paths) {
		item, _ := paths[p].(map[string]any)
		for _, method := range sortedAnyKeys(item) {
			op, ok := item[method].(map[string]any)
			if !ok {
				continue
			}
			prefix := fmt.Sprintf("op %s %s", strings.ToUpper(method), p)
			if id, ok := op["operationId"].(string); ok {
				lines = append(lines, prefix+" operationId="+id)
			}
			for _, param := range anySlice(op["parameters"]) {
				pm, _ := param.(map[string]any)
				required := false
				if r, ok := pm["required"].(bool); ok {
					required = r
				}
				lines = append(lines, fmt.Sprintf("%s parameter %v in=%v required=%v type=%s",
					prefix, pm["name"], pm["in"], required, typeOf(mapOf(pm["schema"]))))
			}
			if rb := mapOf(op["requestBody"]); rb != nil {
				lines = append(lines, fmt.Sprintf("%s requestBody %s", prefix, contentShape(rb)))
			}
			responses := mapOf(op["responses"])
			for _, code := range sortedAnyKeys(responses) {
				lines = append(lines, fmt.Sprintf("%s response %s %s", prefix, code, contentShape(mapOf(responses[code]))))
			}
		}
	}

	for _, name := range sortedAnyKeys(schemas) {
		s := mapOf(schemas[name])
		lines = append(lines, fmt.Sprintf("schema %s type=%s", name, typeOf(s)))
		// One line per enum value rather than one joined line for the set.
		//
		// Joined, an enum that GAINS a value reads as a line that
		// disappeared, so additive-only refuses it, and adding an error
		// code to a response enum is an ordinary additive change this
		// repository makes routinely. Split, adding a value adds a line
		// and passes, and removing one takes a line away and fails, which
		// is the direction that actually breaks a client. Found by
		// composing this gate against a main that had just gained
		// BACKUP_SET_REPOINT_NOT_ACKNOWLEDGED.
		lines = append(lines, enumLines(fmt.Sprintf("schema %s", name), s)...)
		required := requiredSet(s)
		props := mapOf(s["properties"])
		for _, prop := range sortedAnyKeys(props) {
			ps := mapOf(props[prop])
			line := fmt.Sprintf("schema %s.%s type=%s required=%v", name, prop, typeOf(ps), required[prop])
			if ref, ok := ps["$ref"].(string); ok {
				line += " ref=" + ref
			}
			if items := mapOf(ps["items"]); items != nil {
				line += " items=" + typeOf(items)
			}
			lines = append(lines, line)
			lines = append(lines, enumLines(fmt.Sprintf("schema %s.%s", name, prop), ps)...)
		}
	}

	contract := Cell{
		Certifies: "FR-35 clause 3: every operation, parameter, response and schema property the /api/v1 contract already promises is still promised, unchanged. EPIC E may add to this list and may not take anything off it, which is what \"identical API responses except for additive fields\" means when a machine has to decide it.",
		Rule:      RuleAdditiveOnly,
		Lines:     lines,
	}

	reqLines := requestRequirements(doc, schemas)
	if len(reqLines) == 0 {
		return Cell{}, Cell{}, fmt.Errorf("%s declares no operation at all, so there is nothing this check could vouch for", contractPath)
	}
	requests := Cell{
		Certifies: "FR-35, the half the additive rule cannot see on its own: an EXISTING operation or request schema that gains a requirement breaks every client that was already calling it, and adding a requirement is an addition. One joined line per operation and per schema, so a new endpoint is a new line and an existing endpoint's requirements changing rewrites its line.",
		Rule:      RuleAdditiveOnly,
		Lines:     reqLines,
	}

	return contract, requests, nil
}

// requestRequirements lists what a caller is already obliged to send, one
// joined line per operation and per request schema.
//
// Joined, and additive-only, and that combination is the whole design.
// The break worth catching is an EXISTING operation or an EXISTING schema
// gaining a requirement, because that is what stops a client that already
// worked from working. A brand new operation cannot break anybody, and it
// necessarily arrives with required path parameters of its own, so a
// line-per-requirement list compared exactly reports every new endpoint as
// a compatibility break. It did, the first time this gate met a main that
// had just gained PATCH /backup-sets/{source}/{set}.
//
// One line per key fixes both directions at once: a new key is a new line
// and passes, and an existing key that gains or loses a requirement has
// its line rewritten, which additive-only reads as the old line
// disappearing and refuses.
//
// Every operation gets a line even when it requires nothing, spelled
// "(none)". Without that, an operation with no required parameters has no
// baseline line at all, and adding the first required parameter to it
// looks like an addition. That is not hypothetical either: the control in
// scripts/compat/selftest.sh plants exactly that on GET /activity, which
// requires nothing today.
func requestRequirements(doc map[string]any, schemas map[string]any) []string {
	reachable := map[string]bool{}
	paths, _ := doc["paths"].(map[string]any)

	var lines []string
	for _, p := range sortedAnyKeys(paths) {
		item := mapOf(paths[p])
		for _, method := range sortedAnyKeys(item) {
			op := mapOf(item[method])
			if op == nil {
				continue
			}

			var required []string
			for _, param := range anySlice(op["parameters"]) {
				pm := mapOf(param)
				if r, ok := pm["required"].(bool); ok && r {
					required = append(required, fmt.Sprintf("%v(in %v)", pm["name"], pm["in"]))
				}
			}
			sort.Strings(required)
			lines = append(lines, fmt.Sprintf("operation %s %s requires parameters: %s",
				strings.ToUpper(method), p, joinOrNone(required)))

			if rb := mapOf(op["requestBody"]); rb != nil {
				for _, ref := range refsIn(rb) {
					markReachable(ref, schemas, reachable)
				}
			}
		}
	}

	for _, name := range sortedKeysOf(reachable) {
		lines = append(lines, fmt.Sprintf("request schema %s requires: %s",
			name, joinOrNone(sortedKeysOf(requiredSet(mapOf(schemas[name]))))))
	}

	sort.Strings(lines)
	return lines
}

// joinOrNone spells an empty requirement set out loud, so it has a
// baseline line to change when it stops being empty.
func joinOrNone(vs []string) string {
	if len(vs) == 0 {
		return "(none)"
	}
	return strings.Join(vs, "|")
}

func markReachable(ref string, schemas map[string]any, seen map[string]bool) {
	name := strings.TrimPrefix(ref, "#/components/schemas/")
	if name == ref || seen[name] {
		return
	}
	s := mapOf(schemas[name])
	if s == nil {
		return
	}
	seen[name] = true
	for _, r := range refsIn(s) {
		markReachable(r, schemas, seen)
	}
}

// refsIn collects every $ref anywhere beneath v.
func refsIn(v any) []string {
	var out []string
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if k == "$ref" {
				if s, ok := child.(string); ok {
					out = append(out, s)
				}
				continue
			}
			out = append(out, refsIn(child)...)
		}
	case []any:
		for _, child := range t {
			out = append(out, refsIn(child)...)
		}
	}
	return out
}

// contentShape renders what a request or response carries: the media type
// and the schema behind it, by reference where there is one.
func contentShape(node map[string]any) string {
	if node == nil {
		return "(none)"
	}
	content := mapOf(node["content"])
	if len(content) == 0 {
		return "(no body)"
	}
	parts := make([]string, 0, len(content))
	for _, media := range sortedAnyKeys(content) {
		schema := mapOf(mapOf(content[media])["schema"])
		if ref, ok := schema["$ref"].(string); ok {
			parts = append(parts, media+" -> "+ref)
			continue
		}
		parts = append(parts, media+" -> "+typeOf(schema))
	}
	return strings.Join(parts, ", ")
}

func typeOf(s map[string]any) string {
	if s == nil {
		return "(none)"
	}
	if ref, ok := s["$ref"].(string); ok {
		return ref
	}
	t, _ := s["type"].(string)
	if t == "" {
		switch {
		case s["oneOf"] != nil:
			t = "oneOf"
		case s["allOf"] != nil:
			t = "allOf"
		case s["anyOf"] != nil:
			t = "anyOf"
		default:
			t = "(untyped)"
		}
	}
	if f, ok := s["format"].(string); ok {
		t += "/" + f
	}
	return t
}

// enumLines renders a schema node's enum, one value per line.
func enumLines(prefix string, node map[string]any) []string {
	values := anySlice(node["enum"])
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, fmt.Sprintf("%s enum value=%v", prefix, v))
	}
	sort.Strings(out)
	return out
}

func requiredSet(s map[string]any) map[string]bool {
	out := map[string]bool{}
	for _, r := range anySlice(s["required"]) {
		if name, ok := r.(string); ok {
			out[name] = true
		}
	}
	return out
}

func mapAt(doc map[string]any, keys ...string) map[string]any {
	cur := doc
	for _, k := range keys {
		cur = mapOf(cur[k])
		if cur == nil {
			return nil
		}
	}
	return cur
}

func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func anySlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func sortedAnyKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedKeysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
