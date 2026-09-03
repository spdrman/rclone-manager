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
		if enum := anySlice(s["enum"]); len(enum) > 0 {
			lines = append(lines, fmt.Sprintf("schema %s enum=%s", name, joinAny(enum)))
		}
		required := requiredSet(s)
		props := mapOf(s["properties"])
		for _, prop := range sortedAnyKeys(props) {
			ps := mapOf(props[prop])
			line := fmt.Sprintf("schema %s.%s type=%s required=%v", name, prop, typeOf(ps), required[prop])
			if enum := anySlice(ps["enum"]); len(enum) > 0 {
				line += " enum=" + joinAny(enum)
			}
			if ref, ok := ps["$ref"].(string); ok {
				line += " ref=" + ref
			}
			if items := mapOf(ps["items"]); items != nil {
				line += " items=" + typeOf(items)
			}
			lines = append(lines, line)
		}
	}

	contract := Cell{
		Certifies: "FR-35 clause 3: every operation, parameter, response and schema property the /api/v1 contract already promises is still promised, unchanged. EPIC E may add to this list and may not take anything off it, which is what \"identical API responses except for additive fields\" means when a machine has to decide it.",
		Rule:      RuleAdditiveOnly,
		Lines:     lines,
	}

	reqLines := requestRequirements(doc, schemas)
	if len(reqLines) == 0 {
		return Cell{}, Cell{}, fmt.Errorf("no request body in %s declares a required property; that is not a contract this check can vouch for", contractPath)
	}
	requests := Cell{
		Certifies: "FR-35, the half additive-only cannot see: a request schema that gains a required property breaks every client that was already sending the old shape, and adding one is an addition, so the additive rule would wave it through. This list is compared exactly.",
		Rule:      RuleIdentical,
		Lines:     reqLines,
	}

	return contract, requests, nil
}

// requestRequirements lists what a caller is obliged to send, over every
// schema reachable from a request body.
func requestRequirements(doc map[string]any, schemas map[string]any) []string {
	reachable := map[string]bool{}
	paths, _ := doc["paths"].(map[string]any)
	for _, p := range sortedAnyKeys(paths) {
		item := mapOf(paths[p])
		for _, method := range sortedAnyKeys(item) {
			op := mapOf(item[method])
			rb := mapOf(op["requestBody"])
			if rb == nil {
				continue
			}
			for _, ref := range refsIn(rb) {
				markReachable(ref, schemas, reachable)
			}
		}
	}

	var lines []string
	for _, name := range sortedKeysOf(reachable) {
		s := mapOf(schemas[name])
		req := requiredSet(s)
		for _, prop := range sortedKeysOf(req) {
			lines = append(lines, fmt.Sprintf("request schema %s requires %s", name, prop))
		}
	}
	sort.Strings(lines)
	return lines
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

func joinAny(vs []any) string {
	parts := make([]string, 0, len(vs))
	for _, v := range vs {
		parts = append(parts, fmt.Sprintf("%v", v))
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}
