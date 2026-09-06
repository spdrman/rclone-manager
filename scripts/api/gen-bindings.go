// Command gen-bindings turns api/v1/openapi.json, the authoritative
// definition of /api/v1 (issue #166), into the Go and TypeScript bindings
// both sides of the boundary are held to.
//
// # Direction
//
// Spec-first, both sides generated. The contract document is written by
// hand and is the source of truth; the Go binding package and the
// TypeScript binding module are outputs and carry a DO NOT EDIT banner.
// docs/api/contract.md records why that direction was chosen over
// generating the contract out of the Go handlers.
//
// # Why a generator in this repository rather than an off-the-shelf one
//
// Every other gate here (scripts/architecture, scripts/perf) is a small,
// dependency-free program that the local mirror and CI both run with
// nothing installed beyond the Go toolchain that is already required. An
// external OpenAPI generator would add a network fetch and a second
// language runtime to the one job that has to be reproducible byte for
// byte on every machine, in exchange for supporting a great deal of
// OpenAPI this contract deliberately does not use. So this reads the
// subset the contract actually speaks, and refuses anything outside it
// rather than emitting something plausible.
//
// # Usage
//
//	go run scripts/api/gen-bindings.go <repo-root> <out-go> <out-ts>
//
// scripts/api/generate.sh writes the checked-in files;
// scripts/api/check-contract-drift.sh writes to a temporary directory and
// compares, which is how a hand-edited generated file is caught.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- model ---

type document struct {
	OpenAPI    string                     `json:"openapi"`
	Info       docInfo                    `json:"info"`
	Servers    []json.RawMessage          `json:"servers"`
	Components components                 `json:"components"`
	Paths      map[string]map[string]oper `json:"paths"`
	GoPackage  string                     `json:"x-go-package"`
	Generated  map[string]string          `json:"x-generated"`
}

type docInfo struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Summary     string `json:"summary"`
	Description string `json:"description"`
}

type components struct {
	Schemas         map[string]*schema         `json:"schemas"`
	SecuritySchemes map[string]json.RawMessage `json:"securitySchemes"`
	Parameters      map[string]json.RawMessage `json:"parameters"`
	Responses       map[string]json.RawMessage `json:"responses"`
}

type schema struct {
	Type        string             `json:"type"`
	Description string             `json:"description"`
	Format      string             `json:"format"`
	Enum        []string           `json:"enum"`
	Properties  map[string]*schema `json:"properties"`
	Required    []string           `json:"required"`
	Items       *schema            `json:"items"`
	Ref         string             `json:"$ref"`
	AllOf       []*schema          `json:"allOf"`
	// OneOf is modelled only where NEITHER binding generates a type from
	// it: the body of an error response, which both bindings represent by
	// their status and x-error-codes rather than by a schema. Anywhere a
	// type WOULD be generated, using oneOf is fatal (see refuseOneOf
	// below) rather than silently dropped, because a silently dropped
	// shape is exactly the drift this generator exists to prevent.
	OneOf            []*schema           `json:"oneOf"`
	WriteOnly        bool                `json:"writeOnly"`
	AdditionalProps  *bool               `json:"additionalProperties"`
	Minimum          *float64            `json:"minimum"`
	GoOmitEmpty      bool                `json:"x-go-omitempty"`
	GoPointer        bool                `json:"x-go-pointer"`
	GoEmbeds         []string            `json:"x-go-embeds"`
	CodeOrigins      map[string]string   `json:"x-code-origins"`
	ErrorClasses     map[string][]string `json:"x-error-classes"`
	CapabilityFields []string            `json:"x-capability-fields"`
}

type oper struct {
	OperationID string                `json:"operationId"`
	Summary     string                `json:"summary"`
	Tags        []string              `json:"tags"`
	Security    []map[string][]string `json:"security"`
	Parameters  []json.RawMessage     `json:"parameters"`
	RequestBody *body                 `json:"requestBody"`
	Responses   map[string]*response  `json:"responses"`
	Gate        bool                  `json:"x-destructive-gate"`
	Idempotency string                `json:"x-idempotency-key"`
	Concurrency *string               `json:"x-optimistic-concurrency"`
	CSRF        bool                  `json:"x-csrf-required"`
	Authn       bool                  `json:"x-authenticated"`
}

type body struct {
	Required bool `json:"required"`
	Content  map[string]struct {
		Schema *schema `json:"schema"`
	} `json:"content"`
}

type response struct {
	Description string `json:"description"`
	Content     map[string]struct {
		Schema *schema `json:"schema"`
	} `json:"content"`
	ErrorCodes []string `json:"x-error-codes"`
}

// endpoint is one operation, flattened into what both bindings need.
type endpoint struct {
	ID             string
	Method         string
	Path           string
	Authenticated  bool
	CSRF           bool
	Idempotency    string
	Gate           bool
	Concurrency    string
	RequestSchema  string
	ResponseSchema string
	SuccessStatus  int
	ErrorStatuses  []int
	ErrorCodes     map[int][]string
}

func main() {
	if len(os.Args) != 4 {
		fatal("usage: gen-bindings <repo-root> <out-go> <out-ts>")
	}
	root, outGo, outTS := os.Args[1], os.Args[2], os.Args[3]

	raw, err := os.ReadFile(filepath.Join(root, contractPath))
	if err != nil {
		fatal("reading the contract: %v", err)
	}
	var doc document
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		// DisallowUnknownFields is deliberate: an extension this generator
		// does not understand must stop the build rather than be silently
		// dropped out of both bindings, which is exactly the drift this
		// whole issue exists to prevent.
		fatal("the contract uses something this generator does not model: %v", err)
	}
	if doc.OpenAPI != "3.1.0" {
		fatal("the contract declares OpenAPI %q; this generator reads 3.1.0", doc.OpenAPI)
	}

	codes, ok := doc.Components.Schemas["ApiErrorCode"]
	if !ok || len(codes.Enum) == 0 {
		fatal("the contract has no ApiErrorCode enum, so there is no error-code registry to generate")
	}
	for _, c := range codes.Enum {
		if _, ok := codes.CodeOrigins[c]; !ok {
			fatal("error code %q has no entry in x-code-origins, so neither binding can tell a wire code from a UI-only one", c)
		}
	}

	// oneOf on a named schema would be skipped by objectSchemaNames (it is
	// neither an object nor an allOf), so both bindings would silently
	// lose the shape. Refuse it instead.
	for _, name := range sortedKeys(doc.Components.Schemas) {
		if len(doc.Components.Schemas[name].OneOf) > 0 {
			fatal("schema %q uses oneOf at its top level, and this generator only models oneOf on an error response body, where no type is generated from it. A named schema using oneOf would be dropped from both bindings without a word.", name)
		}
	}

	eps := flatten(doc)
	if len(eps) == 0 {
		fatal("the contract declares no operations, so generating bindings from it would produce nothing and pass vacuously")
	}

	names := objectSchemaNames(doc)
	if len(names) == 0 {
		fatal("the contract declares no object schemas, so generating bindings from it would produce nothing and pass vacuously")
	}

	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])

	goSrc, err := format.Source(renderGo(doc, codes, names, eps, digest))
	if err != nil {
		fatal("the generated Go does not parse, which is a generator bug rather than a contract one: %v", err)
	}
	if err := os.WriteFile(outGo, goSrc, 0o644); err != nil {
		fatal("writing %s: %v", outGo, err)
	}
	if err := os.WriteFile(outTS, renderTS(doc, codes, names, eps, digest), 0o644); err != nil {
		fatal("writing %s: %v", outTS, err)
	}
	fmt.Printf("generated %d schemas and %d operations into:\n  %s\n  %s\n", len(names), len(eps), outGo, outTS)
}

const contractPath = "api/v1/openapi.json"

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "gen-bindings: "+format+"\n", a...)
	os.Exit(1)
}

// ------------------------------------------------------------ flattening ---

// flatten turns the paths object into a deterministically ordered list.
// OpenAPI's paths and its per-path methods are both JSON objects, so their
// document order is not preserved by any decoder; sorting by path then
// method is what makes the generated output byte-stable across runs.
func flatten(doc document) []endpoint {
	var out []endpoint
	paths := sortedKeys(doc.Paths)
	for _, p := range paths {
		methods := sortedKeys(doc.Paths[p])
		for _, m := range methods {
			o := doc.Paths[p][m]
			e := endpoint{
				ID:            o.OperationID,
				Method:        strings.ToUpper(m),
				Path:          p,
				Authenticated: o.Authn,
				CSRF:          o.CSRF,
				Idempotency:   o.Idempotency,
				Gate:          o.Gate,
				ErrorCodes:    map[int][]string{},
			}
			if o.Concurrency != nil {
				e.Concurrency = *o.Concurrency
			}
			if o.RequestBody != nil {
				body := o.RequestBody.Content["application/json"].Schema
				refuseOneOf(e.ID, "request body", body)
				e.RequestSchema = refName(body)
			}
			for status, r := range o.Responses {
				n, err := strconv.Atoi(status)
				if err != nil {
					fatal("operation %s has a non-numeric response status %q", e.ID, status)
				}
				if n < 400 {
					e.SuccessStatus = n
					if c, ok := r.Content["application/json"]; ok {
						refuseOneOf(e.ID, fmt.Sprintf("%d response body", n), c.Schema)
						e.ResponseSchema = refName(c.Schema)
					}
					continue
				}
				e.ErrorStatuses = append(e.ErrorStatuses, n)
				e.ErrorCodes[n] = r.ErrorCodes
			}
			sort.Ints(e.ErrorStatuses)
			if e.SuccessStatus == 0 {
				fatal("operation %s declares no success response", e.ID)
			}
			out = append(out, e)
		}
	}
	return out
}

// refuseOneOf stops a oneOf reaching a position a binding is generated
// from. refName would quietly return "" for one (it has no $ref of its
// own), and an empty schema name means "this operation has no body",
// which is a different and wrong statement.
func refuseOneOf(id, where string, s *schema) {
	if s != nil && len(s.OneOf) > 0 {
		fatal("operation %s's %s uses oneOf; this generator models oneOf only on an error response body, where no type is generated from it", id, where)
	}
}

var refPattern = regexp.MustCompile(`^#/components/schemas/([A-Za-z0-9_]+)$`)

func refName(s *schema) string {
	if s == nil || s.Ref == "" {
		return ""
	}
	m := refPattern.FindStringSubmatch(s.Ref)
	if m == nil {
		fatal("unsupported $ref %q; this contract only references #/components/schemas/<Name>", s.Ref)
	}
	return m[1]
}

// objectSchemaNames is every schema that becomes a struct/interface, in a
// stable order. ApiErrorCode is an enum, not an object, and is generated
// separately.
func objectSchemaNames(doc document) []string {
	var out []string
	for name, s := range doc.Components.Schemas {
		if s.Type == "object" || len(s.AllOf) > 0 {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// properties returns a schema's own properties (following allOf), with the
// embedded parents reported separately so the Go side can embed rather
// than restate them.
func properties(doc document, s *schema) (props map[string]*schema, required map[string]bool, embeds []string) {
	props = map[string]*schema{}
	required = map[string]bool{}
	collect := func(part *schema) {
		for k, v := range part.Properties {
			props[k] = v
		}
		for _, r := range part.Required {
			required[r] = true
		}
	}
	if len(s.AllOf) > 0 {
		for _, part := range s.AllOf {
			if part.Ref != "" {
				parent := refName(part)
				embeds = append(embeds, parent)
				continue
			}
			collect(part)
		}
		return props, required, embeds
	}
	collect(s)
	return props, required, nil
}

// -------------------------------------------------------------- Go output ---

var goInitialisms = map[string]string{
	"api": "API", "id": "ID", "ok": "OK", "pem": "PEM", "ssh": "SSH",
	"url": "URL", "ui": "UI", "csrf": "CSRF", "json": "JSON", "cpu": "CPU",
}

var camelTail = regexp.MustCompile(`Id$`)

func goName(jsonName string) string {
	var b strings.Builder
	for _, part := range strings.Split(jsonName, "_") {
		if part == "" {
			continue
		}
		if up, ok := goInitialisms[strings.ToLower(part)]; ok {
			b.WriteString(up)
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	// A camelCase property (the /auth schemas, which predate this
	// contract's snake_case convention) survives the loop above as one
	// part, so only its first letter was raised. Fix the one initialism
	// that actually occurs there rather than re-splitting on case.
	return camelTail.ReplaceAllString(b.String(), "ID")
}

func goType(doc document, s *schema) string {
	base := func() string {
		if s.Ref != "" {
			n := refName(s)
			if n == "ApiErrorCode" {
				return "ErrorCode"
			}
			return n
		}
		switch s.Type {
		case "string":
			return "string"
		case "boolean":
			return "bool"
		case "integer":
			switch s.Format {
			case "int32", "":
				return "int"
			case "int64":
				return "int64"
			case "uint64":
				return "uint64"
			default:
				fatal("unsupported integer format %q", s.Format)
			}
		case "array":
			return "[]" + goType(doc, s.Items)
		}
		fatal("unsupported schema type %q", s.Type)
		return ""
	}()
	if s.GoPointer {
		return "*" + base
	}
	return base
}

func renderGo(doc document, codes *schema, names []string, eps []endpoint, digest string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, `// Code generated by scripts/api/gen-bindings.go from %s. DO NOT EDIT.
//
// Package apicontract is the Go half of the generated /api/v1 bindings
// (issue #166). The contract document, not this file and not the handlers
// in apps/common/webhost, is the definition of the boundary: edit
// %s and re-run scripts/api/generate.sh.
//
// A hand edit here is caught by scripts/api/check-contract-drift.sh, which
// regenerates into a temporary directory and compares. A handler whose
// shape stops matching one of these types is caught by
// apps/common/webhost's TestContract_HandlerShapesMatchTheContract.
package apicontract

// Version and BasePath are the versioned boundary itself.
const (
	Version  = %q
	BasePath = %q
)

// ContractSHA256 is the digest of the contract document these bindings were
// generated from.
//
// It exists so the ordinary go test run catches the commonest
// mistake, editing the contract and forgetting to regenerate, without
// shelling out to the generator: TestContract_TheBindingsWereGeneratedFromThisContract
// hashes api/v1/openapi.json and compares. The full byte-for-byte
// comparison still lives in scripts/api/check-contract-drift.sh, which is
// the only thing that can also catch a hand edit to the body of this file.
const ContractSHA256 = %q

// ErrorCode is a stable, machine-readable failure token. The human-readable
// message beside it on the wire MAY change without notice; this may not.
type ErrorCode string

// The complete error-code registry. Codes are declared in the contract, not
// here, and ui/shared reads the same registry through the TypeScript half of
// these bindings, so the two can never hold different lists.
const (
`, contractPath, contractPath, "v1", "/api/v1", digest)

	for _, c := range codes.Enum {
		fmt.Fprintf(&b, "\tErrorCode%s ErrorCode = %q\n", errorCodeGoName(c), c)
	}
	b.WriteString(")\n\n")

	var wire, ui []string
	for _, c := range codes.Enum {
		if codes.CodeOrigins[c] == "wire" {
			wire = append(wire, c)
		} else {
			ui = append(ui, c)
		}
	}
	writeCodeSlice(&b, "WireErrorCodes", "codes a server may put on the wire. Every one of these is emitted by real handler code, and apps/common/webhost's TestContract_EveryWireErrorCodeIsRegistered holds that both ways.", wire)
	writeCodeSlice(&b, "UIErrorCodes", "the shared UI's own presentation vocabulary. No endpoint emits these; they are registered here so there is one registry rather than a second hand-maintained list in ui/shared.", ui)
	writeCodeSlice(&b, "ErrorCodes", "every registered code, wire and UI alike, in contract order.", codes.Enum)

	b.WriteString("// ErrorClasses groups codes by the refusal they represent, so a caller (or\n// a red team) can assert the RIGHT refusal rather than any refusal.\nvar ErrorClasses = map[string][]ErrorCode{\n")
	for _, class := range sortedKeys(codes.ErrorClasses) {
		fmt.Fprintf(&b, "\t%q: {", class)
		for i, c := range codes.ErrorClasses[class] {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "ErrorCode%s", errorCodeGoName(c))
		}
		b.WriteString("},\n")
	}
	b.WriteString("}\n\n")

	b.WriteString(`// Endpoint is one operation of the contract, with the requirements a
// handler has to satisfy expressed as data rather than as convention:
// whether a session is required, whether CSRF is required, whether an
// idempotency key is required, whether the destructive gate stands in
// front of it, and which optimistic-concurrency token it reads.
type Endpoint struct {
	ID              string
	Method          string
	Path            string
	Authenticated   bool
	CSRFRequired    bool
	IdempotencyKey  string
	DestructiveGate bool
	Concurrency     string
	RequestSchema   string
	ResponseSchema  string
	SuccessStatus   int
	ErrorCodes      map[int][]ErrorCode
}

// Endpoints is every operation the contract declares, ordered by path then
// method.
var Endpoints = []Endpoint{
`)
	for _, e := range eps {
		fmt.Fprintf(&b, "\t{\n\t\tID: %q, Method: %q, Path: %q,\n", e.ID, e.Method, e.Path)
		fmt.Fprintf(&b, "\t\tAuthenticated: %t, CSRFRequired: %t, IdempotencyKey: %q, DestructiveGate: %t, Concurrency: %q,\n",
			e.Authenticated, e.CSRF, e.Idempotency, e.Gate, e.Concurrency)
		fmt.Fprintf(&b, "\t\tRequestSchema: %q, ResponseSchema: %q, SuccessStatus: %d,\n", e.RequestSchema, e.ResponseSchema, e.SuccessStatus)
		b.WriteString("\t\tErrorCodes: map[int][]ErrorCode{\n")
		for _, status := range e.ErrorStatuses {
			fmt.Fprintf(&b, "\t\t\t%d: {", status)
			for i, c := range e.ErrorCodes[status] {
				if i > 0 {
					b.WriteString(", ")
				}
				fmt.Fprintf(&b, "ErrorCode%s", errorCodeGoName(c))
			}
			b.WriteString("},\n")
		}
		b.WriteString("\t\t},\n\t},\n")
	}
	b.WriteString("}\n\n")

	fmt.Fprintf(&b, "// CapabilityFields is the platform-capability set GET /system/capabilities\n// reports, as wire field names. ui/shared's PlatformCapabilities is held to\n// exactly this list, so the two capability models cannot diverge.\nvar CapabilityFields = []string{\n")
	for _, f := range doc.Components.Schemas["CapabilitiesResponse"].CapabilityFields {
		fmt.Fprintf(&b, "\t%q,\n", f)
	}
	b.WriteString("}\n\n")

	for _, name := range names {
		s := doc.Components.Schemas[name]
		props, _, embeds := properties(doc, s)
		if d := describe(s); d != "" {
			fmt.Fprintf(&b, "// %s is %s\n", name, d)
		}
		fmt.Fprintf(&b, "type %s struct {\n", name)
		for _, e := range embeds {
			fmt.Fprintf(&b, "\t%s\n", e)
		}
		for _, p := range sortedKeys(props) {
			ps := props[p]
			tag := p
			if ps.GoOmitEmpty {
				tag += ",omitempty"
			}
			fmt.Fprintf(&b, "\t%s %s `json:%q`\n", goName(p), goType(doc, ps), tag)
		}
		b.WriteString("}\n\n")
	}

	b.WriteString(`// SchemaTypes maps a contract schema name to a zero value of the generated
// Go type for it. A conformance test reaches a type by contract name
// through this map rather than through a hand-written lookup, so a schema
// added to the contract cannot quietly go unchecked.
var SchemaTypes = map[string]any{
`)
	for _, name := range names {
		fmt.Fprintf(&b, "\t%q: %s{},\n", name, name)
	}
	b.WriteString("}\n")
	return []byte(b.String())
}

func writeCodeSlice(b *strings.Builder, name, doc string, codes []string) {
	fmt.Fprintf(b, "// %s is %s\nvar %s = []ErrorCode{\n", name, doc, name)
	for _, c := range codes {
		fmt.Fprintf(b, "\tErrorCode%s,\n", errorCodeGoName(c))
	}
	b.WriteString("}\n\n")
}

var nonIdent = regexp.MustCompile(`[^A-Za-z0-9]+`)

// errorCodeGoName turns a wire token (UPPER_SNAKE) or a UI token
// (kebab-case) into one Go identifier suffix, with no collision between
// the two vocabularies possible: the two never spell the same word.
func errorCodeGoName(code string) string {
	parts := nonIdent.Split(strings.ToLower(code), -1)
	var b strings.Builder
	for _, p := range parts {
		if p == "" {
			continue
		}
		if up, ok := goInitialisms[p]; ok {
			b.WriteString(up)
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]) + p[1:])
	}
	return b.String()
}

func describe(s *schema) string {
	d := s.Description
	if d == "" && len(s.AllOf) > 0 {
		d = s.Description
	}
	if d == "" {
		return ""
	}
	// Lower the first letter so the comment reads "Name is the ...", but
	// never when the sentence opens on an acronym or an HTTP method:
	// "POST /auth/login" must not become "pOST /auth/login".
	if len(d) > 1 && d[1] >= 'a' && d[1] <= 'z' {
		d = strings.ToLower(d[:1]) + d[1:]
	}
	return wrapComment(d)
}

// wrapComment folds a description into a Go/TS comment body at a width
// that matches the rest of the repository.
func wrapComment(text string) string {
	const width = 66
	words := strings.Fields(text)
	var lines []string
	cur := ""
	for _, w := range words {
		if cur == "" {
			cur = w
			continue
		}
		if len(cur)+1+len(w) > width {
			lines = append(lines, cur)
			cur = w
			continue
		}
		cur += " " + w
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return strings.Join(lines, "\n// ")
}

// -------------------------------------------------------------- TS output ---

func tsType(doc document, s *schema) string {
	if s.Ref != "" {
		n := refName(s)
		if n == "ApiErrorCode" {
			return "ApiErrorCode"
		}
		return "Wire" + n
	}
	switch s.Type {
	case "string":
		if len(s.Enum) > 0 {
			quoted := make([]string, 0, len(s.Enum))
			for _, e := range s.Enum {
				quoted = append(quoted, strconv.Quote(e))
			}
			return strings.Join(quoted, " | ")
		}
		return "string"
	case "boolean":
		return "boolean"
	case "integer":
		return "number"
	case "array":
		inner := tsType(doc, s.Items)
		if strings.Contains(inner, "|") {
			return "(" + inner + ")[]"
		}
		return inner + "[]"
	}
	fatal("unsupported schema type %q", s.Type)
	return ""
}

func renderTS(doc document, codes *schema, names []string, eps []endpoint, digest string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, `// Code generated by scripts/api/gen-bindings.go from %s. DO NOT EDIT.
//
// The TypeScript half of the generated /api/v1 bindings (issue #166). The
// contract document is the definition of the boundary: edit
// %s and re-run scripts/api/generate.sh.
//
// A hand edit here is caught by scripts/api/check-contract-drift.sh, which
// regenerates into a temporary directory and compares. contracts.ts and
// client.ts consume these types rather than restating them, which is what
// stops a second, hand-maintained copy of the wire shapes existing at all.

export const API_VERSION = %q;
export const API_BASE_PATH = %q;

/** The digest of the contract document these bindings were generated from.
 *  A contract edited without regenerating changes this value, so the
 *  change is visible in review as well as to
 *  scripts/api/check-contract-drift.sh. */
export const CONTRACT_SHA256 = %q;

`, contractPath, contractPath, "v1", "/api/v1", digest)

	var wire, ui []string
	for _, c := range codes.Enum {
		if codes.CodeOrigins[c] == "wire" {
			wire = append(wire, c)
		} else {
			ui = append(ui, c)
		}
	}
	writeTSCodes(&b, "WIRE_ERROR_CODES", "Codes a server may actually put on the wire.", wire)
	writeTSCodes(&b, "UI_ERROR_CODES", "This UI's own presentation vocabulary. No endpoint emits these; \"unknown\" is the sentinel an unrecognised code off the network degrades to.", ui)
	writeTSCodes(&b, "API_ERROR_CODES", "Every registered code, wire and UI alike, in contract order. contracts.ts re-exports this rather than keeping a list of its own.", codes.Enum)

	b.WriteString("export type ApiErrorCode = (typeof API_ERROR_CODES)[number];\n\n")

	b.WriteString("/** Codes grouped by the refusal they represent, so a caller can assert\n *  the RIGHT refusal rather than any refusal. */\nexport const API_ERROR_CLASSES = {\n")
	for _, class := range sortedKeys(codes.ErrorClasses) {
		fmt.Fprintf(&b, "  %q: [", class)
		for i, c := range codes.ErrorClasses[class] {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(strconv.Quote(c))
		}
		b.WriteString("],\n")
	}
	b.WriteString("} as const satisfies Record<string, readonly ApiErrorCode[]>;\n\n")

	fmt.Fprintf(&b, "/** The platform-capability set GET /system/capabilities reports, as wire\n *  field names. types/platform.ts's PlatformCapabilities is held to exactly\n *  this list by contract.conformance.test.ts. */\nexport const CAPABILITY_FIELDS = [\n")
	for _, f := range doc.Components.Schemas["CapabilitiesResponse"].CapabilityFields {
		fmt.Fprintf(&b, "  %q,\n", f)
	}
	b.WriteString("] as const;\n\n")

	b.WriteString(`/** One operation of the contract. The requirements are data, not
 *  convention: whether a session is required, whether CSRF is required,
 *  whether an idempotency key is required, whether the destructive gate
 *  stands in front of it, and which optimistic-concurrency token it
 *  reads. */
export interface ContractOperation {
  readonly id: string;
  readonly method: string;
  readonly path: string;
  readonly authenticated: boolean;
  readonly csrfRequired: boolean;
  readonly idempotencyKey: string;
  readonly destructiveGate: boolean;
  readonly concurrency: string;
  readonly requestSchema: string;
  readonly responseSchema: string;
  readonly successStatus: number;
  readonly errorCodes: Readonly<Record<number, readonly ApiErrorCode[]>>;
}

export const API_OPERATIONS: readonly ContractOperation[] = [
`)
	for _, e := range eps {
		fmt.Fprintf(&b, "  {\n    id: %q,\n    method: %q,\n    path: %q,\n", e.ID, e.Method, e.Path)
		fmt.Fprintf(&b, "    authenticated: %t,\n    csrfRequired: %t,\n    idempotencyKey: %q,\n    destructiveGate: %t,\n    concurrency: %q,\n",
			e.Authenticated, e.CSRF, e.Idempotency, e.Gate, e.Concurrency)
		fmt.Fprintf(&b, "    requestSchema: %q,\n    responseSchema: %q,\n    successStatus: %d,\n", e.RequestSchema, e.ResponseSchema, e.SuccessStatus)
		b.WriteString("    errorCodes: {\n")
		for _, status := range e.ErrorStatuses {
			fmt.Fprintf(&b, "      %d: [", status)
			for i, c := range e.ErrorCodes[status] {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(strconv.Quote(c))
			}
			b.WriteString("],\n")
		}
		b.WriteString("    }\n  },\n")
	}
	b.WriteString("];\n\n")

	for _, name := range names {
		s := doc.Components.Schemas[name]
		props, required, embeds := properties(doc, s)
		if d := s.Description; d != "" {
			fmt.Fprintf(&b, "/** %s */\n", strings.ReplaceAll(wrapComment(d), "\n// ", "\n *  "))
		}
		fmt.Fprintf(&b, "export interface Wire%s", name)
		if len(embeds) > 0 {
			ext := make([]string, 0, len(embeds))
			for _, e := range embeds {
				ext = append(ext, "Wire"+e)
			}
			fmt.Fprintf(&b, " extends %s", strings.Join(ext, ", "))
		}
		b.WriteString(" {\n")
		for _, p := range sortedKeys(props) {
			opt := ""
			if !required[p] {
				opt = "?"
			}
			fmt.Fprintf(&b, "  %s%s: %s;\n", tsPropertyName(p), opt, tsType(doc, props[p]))
		}
		b.WriteString("}\n\n")
	}
	return []byte(b.String())
}

var plainIdent = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)

func tsPropertyName(p string) string {
	if plainIdent.MatchString(p) {
		return p
	}
	return strconv.Quote(p)
}

func writeTSCodes(b *strings.Builder, name, doc string, codes []string) {
	fmt.Fprintf(b, "/** %s */\nexport const %s = [\n", strings.ReplaceAll(wrapComment(doc), "\n// ", "\n *  "), name)
	for _, c := range codes {
		fmt.Fprintf(b, "  %q,\n", c)
	}
	b.WriteString("] as const;\n\n")
}
