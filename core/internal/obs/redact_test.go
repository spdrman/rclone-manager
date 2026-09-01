package obs

import (
	"net"
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

// TestRedactor_RedactsIPv6DialErrorShape reproduces, deterministically and
// without depending on this machine's own DNS or hosts file, the
// adversarial review's exact empirical finding on PR #304: Go's net
// dialer formats an IPv6 connection failure as "dial tcp [::1]:PORT:
// connect: connection refused" (brackets, no literal hostname anywhere),
// not "dial tcp ::1:PORT: ...". A needle built by naive string
// concatenation ("::1" + ":" + "PORT") would never match this; only
// net.JoinHostPort's bracketing does.
func TestRedactor_RedactsIPv6DialErrorShape(t *testing.T) {
	r := NewRedactor(Endpoint{Host: "::1", Port: 51839, User: "backupuser"})
	input := "dial tcp [::1]:51839: connect: connection refused"
	got := r.Filter(input)
	if strings.Contains(got, "::1") {
		t.Fatalf("Filter(%q) = %q, want the IPv6 address redacted", input, got)
	}
	if strings.Contains(got, "51839") {
		t.Fatalf("Filter(%q) = %q, want the port redacted", input, got)
	}
	if !strings.Contains(got, redacted) {
		t.Fatalf("Filter(%q) = %q, want the redaction placeholder", input, got)
	}
}

// TestNewRedactor_AlsoRedactsWhatHostResolvesTo is the obs-package-local
// proof for the Critical finding: needles must cover not just the
// CONFIGURED Host string but every address it resolves to, since that
// resolved address (never the literal hostname) is what actually shows up
// in a real dial failure. internal/app/redaction_test.go's
// TestSensitiveEndpointRedactsResolvedIPFromDNSHostname is the full
// end-to-end version of this same proof, driven through a real rclone
// sftp dial; this test isolates just the needle-building behavior.
func TestNewRedactor_AlsoRedactsWhatHostResolvesTo(t *testing.T) {
	resolved, err := net.LookupHost("localhost")
	if err != nil || len(resolved) == 0 {
		t.Skipf("this host cannot resolve %q, cannot exercise the DNS-hostname path: %v", "localhost", err)
	}

	r := NewRedactor(Endpoint{Host: "localhost", Port: 2222, User: "backupuser"})
	for _, ip := range resolved {
		input := "dial tcp " + net.JoinHostPort(ip, "2222") + ": connect: connection refused"
		got := r.Filter(input)
		if strings.Contains(got, ip) {
			t.Errorf("Filter(%q) = %q, still contains the resolved address %q", input, got, ip)
		}
		if !strings.Contains(got, redacted) {
			t.Errorf("Filter(%q) = %q, want the redaction placeholder", input, got)
		}
	}
}

// TestResolveHost_SkipsAlreadyLiteralIPs proves resolveHost's early exit:
// a Host that is already an IP (v4 or v6) triggers no DNS lookup at all,
// since the literal itself is already registered as a needle by
// addHostShapes and there is nothing further to resolve.
func TestResolveHost_SkipsAlreadyLiteralIPs(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1", "10.0.0.5"} {
		if ips := resolveHost(host); ips != nil {
			t.Errorf("resolveHost(%q) = %v, want nil for an IP literal", host, ips)
		}
	}
	if ips := resolveHost(""); ips != nil {
		t.Errorf("resolveHost(\"\") = %v, want nil", ips)
	}
}
