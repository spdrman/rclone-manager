package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// The claim this file has to establish is a negative one about every
// rendering path at once, and a negative of that shape is usually asserted
// by checking the output looks redacted. That is the weaker check, because
// a path that renders nothing, or renders some other field, passes it too.
//
// So every case reduces to assertNeverLeaked, which hunts for the raw bytes
// in whatever came out the other end. The placeholder turning up is a
// separate and secondary assertion. Only the absence of the secret is the
// property being defended.
//
// The last test is the odd one out on purpose: it asserts the leak. fmt
// cannot pull an interface value out of an unexported struct field, so a
// Secret held in one renders its contents whenever the enclosing struct is
// formatted. Pinning the hole means a future Go that closes it shows up
// here as a failing test, rather than leaving secret.go carrying a warning
// that quietly stopped being true.

// theSecret is the raw value every test below wraps and then hunts for in
// whatever came out the other end. If any assertion below ever needs to
// change what "the raw value" is, this is the one place to do it.
const theSecret = "s3kr1t-ssh-key-material-do-not-log-me"

// assertNeverLeaked fails the test if raw appears anywhere in got. It is
// the one check every case in this file ultimately reduces to: not "does
// this look redacted", but "is the actual secret bytes nowhere in this
// output".
func assertNeverLeaked(t *testing.T, label, got string) {
	t.Helper()
	if strings.Contains(got, theSecret) {
		t.Errorf("%s: leaked the raw secret: %q", label, got)
	}
	if !strings.Contains(got, redacted) {
		t.Errorf("%s: did not contain the redaction placeholder %q, got %q", label, redacted, got)
	}
}

func TestSecretFmtVerbs(t *testing.T) {
	s := NewSecret(theSecret)

	// Every verb fmt understands, plus a couple that are not even valid for
	// a struct (%d, %x on a non-numeric/non-byte type) specifically because
	// an invalid verb is exactly the case where fmt would otherwise fall
	// back to a reflection-based dump of the value to explain the mismatch,
	// which is its own leak route if Format did not intercept first.
	verbs := []string{"%v", "%s", "%q", "%x", "%X", "%#v", "%+v", "%d", "%c", "%t"}
	for _, verb := range verbs {
		got := fmt.Sprintf(verb, s)
		assertNeverLeaked(t, "Sprintf("+verb+")", got)
		if got != redacted {
			t.Errorf("Sprintf(%s) = %q, want exactly %q", verb, got, redacted)
		}
	}
}

func TestSecretPrintFamily(t *testing.T) {
	s := NewSecret(theSecret)

	assertNeverLeaked(t, "Sprint", fmt.Sprint(s))
	assertNeverLeaked(t, "Sprintln", fmt.Sprintln(s))

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%v", s)
	assertNeverLeaked(t, "Fprintf", buf.String())

	wrapped := fmt.Errorf("dialing with key %v: %w", s, fmt.Errorf("connection refused"))
	assertNeverLeaked(t, "error built with %v", wrapped.Error())
}

func TestSecretPointerAlsoRedacts(t *testing.T) {
	s := NewSecret(theSecret)
	got := fmt.Sprintf("%v", &s)
	assertNeverLeaked(t, "Sprintf(%v, *Secret)", got)
}

// TestSecretNestedInStruct is the realistic accident this type exists to
// stop: an engineer debugging a config struct reaches for %+v or %#v on the
// whole thing, not on the secret field alone.
func TestSecretNestedInStruct(t *testing.T) {
	type remoteConfig struct {
		Host    string
		KeyFile Secret
	}
	cfg := remoteConfig{Host: "nas.example.internal", KeyFile: NewSecret(theSecret)}

	for _, verb := range []string{"%v", "%+v", "%#v"} {
		got := fmt.Sprintf(verb, cfg)
		assertNeverLeaked(t, "struct Sprintf("+verb+")", got)
		if !strings.Contains(got, "nas.example.internal") {
			t.Errorf("Sprintf(%s) on the struct also swallowed the non-secret field: %q", verb, got)
		}
	}
}

func TestSecretString(t *testing.T) {
	s := NewSecret(theSecret)
	assertNeverLeaked(t, "String()", s.String())
	if s.String() != redacted {
		t.Errorf("String() = %q, want %q", s.String(), redacted)
	}
}

func TestSecretGoString(t *testing.T) {
	s := NewSecret(theSecret)
	assertNeverLeaked(t, "GoString()", s.GoString())
}

func TestSecretMarshalText(t *testing.T) {
	s := NewSecret(theSecret)
	b, err := s.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
	assertNeverLeaked(t, "MarshalText", string(b))
}

func TestSecretMarshalJSON(t *testing.T) {
	s := NewSecret(theSecret)

	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("json.Marshal(Secret): %v", err)
	}
	assertNeverLeaked(t, "json.Marshal(Secret)", string(b))

	var got string
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("json.Unmarshal into string: %v", err)
	}
	if got != redacted {
		t.Errorf("round-tripped JSON string = %q, want %q", got, redacted)
	}
}

func TestSecretMarshalJSONNested(t *testing.T) {
	type remoteConfig struct {
		Host    string `json:"host"`
		KeyFile Secret `json:"key_file"`
	}
	cfg := remoteConfig{Host: "nas.example.internal", KeyFile: NewSecret(theSecret)}

	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal(struct with Secret field): %v", err)
	}
	assertNeverLeaked(t, "json.Marshal(nested Secret)", string(b))
	if !strings.Contains(string(b), "nas.example.internal") {
		t.Errorf("marshaling also swallowed the non-secret field: %s", b)
	}
}

// TestSecretNoPlainConversion documents, rather than tests at runtime, why
// there is no test here calling string(s): that line does not compile.
// Secret is a struct with an unexported field specifically so there is no
// bare conversion back to string, unlike a `type Secret string` definition
// where string(s) would silently compile and hand back the raw value. The
// only way to recover the wrapped value is the named Reveal method below.
func TestSecretReveal(t *testing.T) {
	s := NewSecret(theSecret)
	if got := s.Reveal(); got != theSecret {
		t.Errorf("Reveal() = %q, want %q", got, theSecret)
	}
}

func TestSecretZeroValueDoesNotPanic(t *testing.T) {
	var s Secret
	assertNeverLeaked(t, "zero-value String()", s.String())
	if s.Reveal() != "" {
		t.Errorf("zero-value Reveal() = %q, want empty", s.Reveal())
	}
}

// TestSecretLogValue exercises log/slog's own purpose-built hook directly,
// independent of any Logger in this package.
func TestSecretLogValue(t *testing.T) {
	s := NewSecret(theSecret)
	v := s.LogValue()
	if v.Kind() != slog.KindString {
		t.Fatalf("LogValue().Kind() = %v, want KindString", v.Kind())
	}
	assertNeverLeaked(t, "LogValue()", v.String())
}

func TestSecretThroughSlogJSONHandler(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, nil)
	logger := slog.New(h)

	logger.Info("dialing remote", "key_file", NewSecret(theSecret))

	assertNeverLeaked(t, "slog JSON handler", buf.String())
}

func TestSecretThroughSlogTextHandler(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, nil)
	logger := slog.New(h)

	logger.Info("dialing remote", "key_file", NewSecret(theSecret))

	assertNeverLeaked(t, "slog text handler", buf.String())
}

// TestSecretThroughSlogNestedInGroup proves the redaction survives being
// logged as part of a slog.Group, not just as a bare top-level attribute.
func TestSecretThroughSlogNestedInGroup(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewJSONHandler(&buf, nil)
	logger := slog.New(h)

	logger.Info("remote configured",
		slog.Group("remote",
			slog.String("host", "nas.example.internal"),
			slog.Any("key_file", NewSecret(theSecret)),
		),
	)

	got := buf.String()
	assertNeverLeaked(t, "slog group", got)
	if !strings.Contains(got, "nas.example.internal") {
		t.Errorf("group logging also swallowed the non-secret field: %s", got)
	}
}

// TestSecretThroughLoggerEndToEnd exercises this package's own Logger, not
// just bare log/slog, so the guarantee is proven at the level callers
// actually use.
func TestSecretThroughLoggerEndToEnd(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, LevelInfo)

	l.Event(context.Background(), LevelInfo, "test_event", "test",
		slog.Any("key_file", NewSecret(theSecret)),
	)

	assertNeverLeaked(t, "obs.Logger end-to-end", buf.String())
}

// unexportedSecretHolder and ExportedSecretHolder are the two halves of
// the measurement in Secret's own doc: the same wrapped value in an
// unexported field and in an exported one.
type unexportedSecretHolder struct{ s Secret }

// ExportedSecretHolder is the control. It is exported for no reason other
// than that its FIELD has to be, which is the whole point being measured.
type ExportedSecretHolder struct{ S Secret }

// TestSecretInAnUnexportedFieldStillLeaks pins a limitation, not a
// behaviour anyone wants.
//
// fmt cannot take an interface out of an unexported struct field
// (reflect.Value.CanInterface is false for one), so it never asks whether
// the value implements Formatter and prints the wrapped string instead.
// Secret's doc used to claim it covered every rendering path the standard
// library offers; it does not, and this test is what keeps that correction
// true.
//
// If a future Go stops leaking here this test will fail. That is a good
// failure: DELETE THIS TEST, and delete the "one rendering path this does
// NOT cover" section from Secret's doc with it. Do not weaken the
// assertion to keep it passing, because everything that reasserts
// redaction on top of this (internal/transport/rclone's
// resolvedCredentials, keyEncryptionSecretCacheEntry) exists only because
// of it.
func TestSecretInAnUnexportedFieldStillLeaks(t *testing.T) {
	const raw = "hunter2"

	leaked := fmt.Sprintf("%+v", unexportedSecretHolder{s: NewSecret(raw)})
	if !strings.Contains(leaked, raw) {
		t.Fatalf("an unexported Secret field no longer leaks through %%+v (got %q). "+
			"Go has closed the hole: delete this test and the limitation section in Secret's doc, "+
			"rather than weakening this assertion", leaked)
	}

	// The control, which is what makes the assertion above a measurement
	// of the FIELD's exportedness rather than of Secret being broken
	// outright.
	redactedRendering := fmt.Sprintf("%+v", ExportedSecretHolder{S: NewSecret(raw)})
	if strings.Contains(redactedRendering, raw) {
		t.Fatalf("an EXPORTED Secret field leaked through %%+v (got %q); Secret's Formatter is not working at all, "+
			"which makes the unexported-field result above meaningless", redactedRendering)
	}
}
