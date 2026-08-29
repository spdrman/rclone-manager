package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that reads from the "15m", "30h" style strings
// every duration field in the EPIC's FR-5 example uses, instead of a bare
// number.
//
// A bare number is refused on purpose. Every field that uses this type gives
// its zero value a specific, checked meaning elsewhere in this package (see
// stale_after, stable_for and validation.command.timeout in validate.go), so
// quietly accepting a stray integer as a count of nanoseconds would trade one
// kind of config mistake (a typo in a unit suffix) for a much quieter one (a
// duration field that is, in effect, always zero).
type Duration time.Duration

// Duration returns the underlying time.Duration.
func (d Duration) Duration() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML requires a duration string and parses it with
// time.ParseDuration. Anything else, including a bare scalar number, is a
// parse error rather than a guess.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return fmt.Errorf("duration must be given as a string like \"15m\" or \"30h\"")
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalYAML renders the duration back as a string, so a config that was
// loaded and re-serialized round-trips.
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}
