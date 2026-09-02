package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// #264: omitting the key must stay indistinguishable from every config
// written before it existed, and must not appear on a round trip.
func TestMaxConnectionsIsAbsentUnlessAsked(t *testing.T) {
	var r Remote
	if err := yaml.Unmarshal([]byte("type: sftp\nhost: h\nuser: u\n"), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.MaxConnections != 0 {
		t.Errorf("MaxConnections = %d for a config that never mentioned it, want 0 (rclone's unlimited default)", r.MaxConnections)
	}
	out, err := yaml.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "max_connections") {
		t.Errorf("a remote that asked for no ceiling grew a max_connections key on marshal:\n%s", out)
	}
}

func TestMaxConnectionsRoundTrips(t *testing.T) {
	var r Remote
	if err := yaml.Unmarshal([]byte("type: sftp\nhost: h\nuser: u\nmax_connections: 2\n"), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.MaxConnections != 2 {
		t.Fatalf("MaxConnections = %d, want 2", r.MaxConnections)
	}
	out, err := yaml.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(out), "max_connections: 2") {
		t.Errorf("configured ceiling did not survive a round trip:\n%s", out)
	}
}

// Zero is a real answer, not a missing one, so it must never be refused.
func TestValidateAcceptsNoCeilingAndRefusesANegativeOne(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   int
		wantErr bool
	}{
		{"unset", 0, false},
		{"a real ceiling", 2, false},
		{"negative", -1, true},
		// #355 finding 10: rclone builds a token dispenser of exactly
		// this many tokens at every NewFs, filling the channel one send
		// at a time (lib/pacer.NewTokenDispenser), so a fat-fingered
		// value is not a harmless no-op. An upper bound costs one line
		// and there is no host on earth this is a ceiling FOR.
		{"the largest value that is still a ceiling", maxConnectionsCeiling, false},
		{"pathological", maxConnectionsCeiling + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfigWithMaxConnections(t, tc.value)
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("max_connections %d was accepted; out of range in either direction it is not a ceiling, it is something rclone will take and then misbehave over", tc.value)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("max_connections %d was refused: %v", tc.value, err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "max_connections") {
				t.Errorf("refusal %q does not name the field an operator has to fix", err)
			}
		})
	}
}

func validConfigWithMaxConnections(t *testing.T, n int) Config {
	t.Helper()
	cfg := validConfig()
	cfg.Sources[0].BackupSets[0].Remote.MaxConnections = n
	return cfg
}
