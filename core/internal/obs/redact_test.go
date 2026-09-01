package obs

import (
	"strings"
	"testing"
)

func TestNewRedactor_NilForNoEndpoints(t *testing.T) {
	if r := NewRedactor(); r != nil {
		t.Fatalf("NewRedactor() = %v, want nil for zero endpoints", r)
	}
	if r := NewRedactor(Endpoint{}); r != nil {
		t.Fatalf("NewRedactor(Endpoint{}) = %v, want nil for an endpoint with every field empty", r)
	}
}

func TestRedactor_FilterNilReceiverAndEmptyInput(t *testing.T) {
	var r *Redactor
	if got := r.Filter("dial tcp 127.0.0.1:22: connect: connection refused"); got != "dial tcp 127.0.0.1:22: connect: connection refused" {
		t.Fatalf("nil *Redactor.Filter altered its input: %q", got)
	}

	built := NewRedactor(Endpoint{Host: "127.0.0.1", Port: 22, User: "backup"})
	if got := built.Filter(""); got != "" {
		t.Fatalf("Filter(\"\") = %q, want empty", got)
	}
}

func TestRedactor_Filter(t *testing.T) {
	cases := []struct {
		name        string
		endpoints   []Endpoint
		input       string
		wantAbsent  []string
		wantPresent []string
	}{
		{
			name:        "dial failure: host:port from Go's own net dialer",
			endpoints:   []Endpoint{{Host: "127.0.0.1", Port: 55570, User: "backupuser"}},
			input:       `list: transient: source "cicd-pipeline/gitea-forge-dump": NewFs: couldn't connect SSH: dial tcp 127.0.0.1:55570: connect: connection refused`,
			wantAbsent:  []string{"55570", "127.0.0.1:55570"},
			wantPresent: []string{"cicd-pipeline/gitea-forge-dump", "connection refused", redacted},
		},
		{
			name:        "transfer failure: user@host:port inside an sftp URL",
			endpoints:   []Endpoint{{Host: "127.0.0.1", Port: 51178, User: "backupuser"}},
			input:       `corrupted on transfer: sha256 hashes differ: src(sftp://backupuser@127.0.0.1:51178/upload/no-hash) "" vs dst(...)`,
			wantAbsent:  []string{"51178", "backupuser@127.0.0.1:51178", "backupuser"},
			wantPresent: []string{"corrupted on transfer", redacted},
		},
		{
			name:        "bare host with no port in the message",
			endpoints:   []Endpoint{{Host: "backup.internal.example", Port: 22, User: "svc"}},
			input:       "host key verification failed for backup.internal.example",
			wantAbsent:  []string{"backup.internal.example"},
			wantPresent: []string{"host key verification failed", redacted},
		},
		{
			name:       "port zero (no port configured) never becomes a needle",
			endpoints:  []Endpoint{{Host: "10.0.0.5", Port: 0, User: "svc"}},
			input:      "dial tcp 10.0.0.5:0: connect: connection refused",
			wantAbsent: []string{"10.0.0.5"},
			// The literal ":0" is not, itself, a registered needle (Port
			// zero means "not configured", not "port zero"); only the bare
			// host is. That's fine: the host is gone, which is what
			// matters, and this case documents the boundary rather than
			// asserting the ":0" fragment specifically survives or not.
		},
		{
			name: "two endpoints, only one appears in this message",
			endpoints: []Endpoint{
				{Host: "prod.example.internal", Port: 22, User: "backup"},
				{Host: "staging.example.internal", Port: 2222, User: "backup"},
			},
			input:       "dial tcp prod.example.internal:22: connect: connection refused",
			wantAbsent:  []string{"prod.example.internal", "prod.example.internal:22"},
			wantPresent: []string{"connection refused", redacted},
		},
		{
			name:        "no endpoints configured: message passes through untouched",
			endpoints:   nil,
			input:       "dial tcp 127.0.0.1:55570: connect: connection refused",
			wantPresent: []string{"127.0.0.1:55570", "connection refused"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRedactor(tc.endpoints...)
			got := r.Filter(tc.input)
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("Filter(%q) = %q, still contains %q", tc.input, got, absent)
				}
			}
			for _, present := range tc.wantPresent {
				if !strings.Contains(got, present) {
					t.Errorf("Filter(%q) = %q, want it to still contain %q", tc.input, got, present)
				}
			}
		})
	}
}

// TestRedactor_LongestNeedleWinsFirst proves the overlap case NewRedactor's
// own doc calls out: with both "host" and "user@host" registered, "host"
// must not run first and leave "user@[REDACTED]" (a mangled partial
// redaction with the account name intact) instead of one clean
// "[REDACTED]" covering the whole "user@host" span.
func TestRedactor_LongestNeedleWinsFirst(t *testing.T) {
	r := NewRedactor(Endpoint{Host: "example.internal", User: "backupuser"})
	got := r.Filter("connecting as backupuser@example.internal failed")
	if strings.Contains(got, "backupuser") || strings.Contains(got, "example.internal") {
		t.Fatalf("Filter left a fragment of the endpoint behind: %q", got)
	}
	if !strings.Contains(got, redacted) {
		t.Fatalf("Filter(%q) = %q, want the redaction placeholder", "...", got)
	}
}
