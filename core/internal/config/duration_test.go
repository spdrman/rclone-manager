// The custom duration scalar, and mostly what it refuses.
//
// One test accepts, four reject, and that ratio is the point. Every field
// using this type gives its zero a specific meaning in validate.go, so the
// dangerous outcome is not a parse failure, it is a value that decodes
// quietly to zero: a bare 30 read as nanoseconds, a mapping where a scalar
// was expected. Each of those would leave a duration field permanently at
// the value each of those checks reads as "the operator did not say".
//
// The round trip matters because the API layer loads a config, changes one
// setting and writes it back, so a duration that marshalled to something it
// could not read again would corrupt every other key on the first save.

package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestDurationUnmarshalsStrings(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{`"15m"`, 15 * time.Minute},
		{`"30h"`, 30 * time.Hour},
		{`"10m"`, 10 * time.Minute},
		{`"1h30m"`, 90 * time.Minute},
		{`"0s"`, 0},
	} {
		t.Run(tc.in, func(t *testing.T) {
			var d Duration
			if err := yaml.Unmarshal([]byte(tc.in), &d); err != nil {
				t.Fatalf("Unmarshal(%s): %v", tc.in, err)
			}
			if d.Duration() != tc.want {
				t.Fatalf("got %s, want %s", d.Duration(), tc.want)
			}
		})
	}
}

func TestDurationRejectsBareNumber(t *testing.T) {
	var d Duration
	err := yaml.Unmarshal([]byte(`30`), &d)
	if err == nil {
		t.Fatal("a bare number was accepted as a duration")
	}
}

func TestDurationRejectsGarbage(t *testing.T) {
	var d Duration
	err := yaml.Unmarshal([]byte(`"not-a-duration"`), &d)
	if err == nil {
		t.Fatal("garbage was accepted as a duration")
	}
}

func TestDurationRejectsNonScalar(t *testing.T) {
	var d Duration
	err := yaml.Unmarshal([]byte("foo: bar"), &d)
	if err == nil {
		t.Fatal("a mapping was accepted as a duration")
	}
}

func TestDurationRoundTrips(t *testing.T) {
	d := Duration(90 * time.Minute)
	out, err := yaml.Marshal(d)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Duration
	if err := yaml.Unmarshal(out, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back != d {
		t.Fatalf("round trip changed the value: %s vs %s", back, d)
	}
}
