package apiclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/apicontract"
)

// What this file plants, and why each one is planted rather than reasoned
// about.
//
// A client nobody has watched fail is not a client. Four things happen to
// one of these in the field, and all four are indistinguishable from each
// other if the client reports them the same way: the call worked, the
// caller is not signed in, nothing is listening, and something answered
// that is not the engine this build was written against. An operator
// reading "the backup service returned an unexpected response" for all
// four learns nothing, and #535 is what that costs.
//
// So each is planted here against a real HTTP server and watched. The
// fourth has five shapes rather than one, because "the contract does not
// describe this" is not a single event: a status the operation does not
// declare, a body that is not JSON at all, an error code outside the
// declared set, a body whose types do not match the schema, and a route
// that does not exist all arrive differently and all mean the same thing.

func TestClient_ASuccessfulCallReturnsWhatTheContractDeclares(t *testing.T) {
	engine := newFakeEngine(t)
	client := engine.client(t, engine.start())

	got, err := client.ListBackupSets(context.Background())
	if err != nil {
		t.Fatalf("ListBackupSets: %v", err)
	}
	if !reflect.DeepEqual(got, apicontract.ListBackupSetsResponse{}) {
		t.Errorf("ListBackupSets returned %#v, want the engine's own body decoded into the contract's type", got)
	}

	// The interesting assertion is not the value, it is the handshake that
	// had to happen first. The Web UI reads its session, signs in when it
	// has none, and only then asks for data; a client that skipped a step
	// would still have got a body out of a laxer fake.
	want := []string{"getSession", "login", "listBackupSets"}
	if got := engine.operations(); !reflect.DeepEqual(got, want) {
		t.Errorf("the engine saw %v, want %v", got, want)
	}
}

func TestClient_SignsInTheWayTheWebUIDoes(t *testing.T) {
	engine := newFakeEngine(t)
	client := engine.client(t, engine.start())

	if _, err := client.CreateBackupSet(context.Background(), apicontract.CreateBackupSetRequest{}); err != nil {
		t.Fatalf("CreateBackupSet: %v", err)
	}

	seen := engine.requests()
	byOperation := map[string]seenRequest{}
	for _, r := range seen {
		byOperation[r.Operation] = r
	}

	// The double-submit token: login and the mutation echo the cookie the
	// engine issued, and the read that seeded it does not send a header at
	// all (the contract declares no CSRF requirement on getSession, and a
	// client that sent one anyway would be guessing).
	login := byOperation["login"]
	if login.CSRFCookie == "" || login.CSRFHeader != login.CSRFCookie {
		t.Errorf("login carried cookie %q and header %q; the two have to match", login.CSRFCookie, login.CSRFHeader)
	}
	create := byOperation["createBackupSet"]
	if create.CSRFCookie == "" || create.CSRFHeader != create.CSRFCookie {
		t.Errorf("createBackupSet carried cookie %q and header %q; the two have to match", create.CSRFCookie, create.CSRFHeader)
	}
	if session := byOperation["getSession"]; session.CSRFHeader != "" {
		t.Errorf("getSession carried a %s header (%q); the contract requires none", csrfHeaderName, session.CSRFHeader)
	}

	// And the session itself travels as the cookie, not as anything this
	// client invented.
	if create.Session == "" {
		t.Errorf("createBackupSet carried no %s cookie, so it was not the signed-in caller", sessionCookieName)
	}
}

func TestClient_ReusesAnExistingSessionRatherThanSigningInAgain(t *testing.T) {
	engine := newFakeEngine(t)
	client := engine.client(t, engine.start())
	ctx := context.Background()

	for range 3 {
		if _, err := client.ListBackupSets(ctx); err != nil {
			t.Fatalf("ListBackupSets: %v", err)
		}
	}

	var logins int
	for _, op := range engine.operations() {
		if op == "login" {
			logins++
		}
	}
	if logins != 1 {
		t.Errorf("three calls produced %d logins, want 1: a client that signs in per call spends an Argon2id hash per call and burns the login rate limit", logins)
	}
}

func TestClient_AnUnauthenticatedCallIsReportedAsAuthentication(t *testing.T) {
	t.Run("the credentials are wrong", func(t *testing.T) {
		engine := newFakeEngine(t)
		base := engine.start()
		client, err := New(Config{BaseURL: base, Username: engine.username, Password: "not-the-password"})
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		_, err = client.ListBackupSets(context.Background())
		if err == nil {
			t.Fatal("ListBackupSets succeeded against an engine that refused the credentials")
		}
		if !errors.Is(err, ErrUnauthenticated) {
			t.Errorf("error is %v, want one that satisfies errors.Is(err, ErrUnauthenticated)", err)
		}
		var apiErr *Error
		if !errors.As(err, &apiErr) {
			t.Fatalf("error is %T, want an *Error carrying the engine's own code", err)
		}
		if apiErr.Code != apicontract.ErrorCodeUnauthenticated {
			t.Errorf("code is %q, want %q", apiErr.Code, apicontract.ErrorCodeUnauthenticated)
		}
		// A refused sign-in must not become a request for data. The engine
		// would refuse it too, but a client that asked anyway is a client
		// that reports the wrong operation in the failure.
		for _, op := range engine.operations() {
			if op == "listBackupSets" {
				t.Errorf("the client asked for backup sets after being refused a session: %v", engine.operations())
			}
		}
	})

	t.Run("no credentials were supplied at all", func(t *testing.T) {
		engine := newFakeEngine(t)
		client, err := New(Config{BaseURL: engine.start()})
		if err != nil {
			t.Fatalf("New: %v", err)
		}

		_, err = client.ListBackupSets(context.Background())
		if !errors.Is(err, ErrUnauthenticated) {
			t.Fatalf("error is %v, want one that satisfies errors.Is(err, ErrUnauthenticated)", err)
		}
		if !strings.Contains(err.Error(), "no credentials") {
			t.Errorf("error reads %q; an operator who supplied no username or password should be told that, not told their password was rejected", err)
		}
	})
}

func TestClient_AnEngineThatIsNotThereIsReportedAsUnreachable(t *testing.T) {
	// A server that existed and then stopped, which is exactly what a CLI
	// meets when the engine is not running: the port is closed and the
	// connection is refused before any HTTP is exchanged.
	closed := httptest.NewServer(http.NotFoundHandler())
	base := closed.URL
	closed.Close()

	client, err := New(Config{BaseURL: base, Username: "operator", Password: "correct-horse-battery"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.ListBackupSets(context.Background())
	if err == nil {
		t.Fatal("ListBackupSets succeeded against a closed port")
	}
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("error is %v, want one that satisfies errors.Is(err, ErrUnreachable)", err)
	}
	// The three failures this must never be confused with. An engine that
	// is down is an operational fact with an obvious remedy; reported as a
	// rejected credential or as a broken contract it sends the operator
	// looking in the wrong place entirely.
	if errors.Is(err, ErrUnauthenticated) {
		t.Errorf("a closed port was reported as an authentication failure: %v", err)
	}
	var violation *ContractViolation
	if errors.As(err, &violation) {
		t.Errorf("a closed port was reported as a contract violation: %v", err)
	}
	if !strings.Contains(err.Error(), base) {
		t.Errorf("error reads %q, and does not name the address it tried (%s), which is the one thing an operator needs from it", err, base)
	}
}

func TestClient_AnAnswerTheContractDoesNotDescribeIsRefused(t *testing.T) {
	cases := []struct {
		name      string
		operation string
		answer    http.HandlerFunc
		call      func(*Client) error
		wants     string
	}{
		{
			name:      "a success status the operation does not declare",
			operation: "createBackupSet",
			answer: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{}`))
			},
			call: func(c *Client) error {
				_, err := c.CreateBackupSet(context.Background(), apicontract.CreateBackupSetRequest{})
				return err
			},
			wants: "200",
		},
		{
			name:      "a body that is not JSON at all",
			operation: "listBackupSets",
			answer: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
			},
			call: func(c *Client) error {
				_, err := c.ListBackupSets(context.Background())
				return err
			},
			wants: "text/html",
		},
		{
			name:      "a body whose types are not the schema's",
			operation: "listBackupSets",
			answer: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"backup_sets":"not-a-list"}`))
			},
			call: func(c *Client) error {
				_, err := c.ListBackupSets(context.Background())
				return err
			},
			wants: "ListBackupSetsResponse",
		},
		{
			name:      "an error status the operation does not declare",
			operation: "listBackupSets",
			answer: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTeapot)
				_, _ = w.Write([]byte(`{"error":{"code":"INTERNAL","message":"nope"}}`))
			},
			call: func(c *Client) error {
				_, err := c.ListBackupSets(context.Background())
				return err
			},
			wants: "418",
		},
		{
			name:      "an error code the operation does not declare for that status",
			operation: "listBackupSets",
			answer: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"code":"ARTIFACT_NOT_FOUND","message":"nope"}}`))
			},
			call: func(c *Client) error {
				_, err := c.ListBackupSets(context.Background())
				return err
			},
			wants: "ARTIFACT_NOT_FOUND",
		},
		{
			name:      "a body that decodes and then keeps going",
			operation: "listBackupSets",
			answer: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"backup_sets":[]}{"backup_sets":[]}`))
			},
			call: func(c *Client) error {
				_, err := c.ListBackupSets(context.Background())
				return err
			},
			wants: "trailing",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			engine := newFakeEngine(t)
			engine.answer[tc.operation] = tc.answer
			client := engine.client(t, engine.start())

			err := tc.call(client)
			if err == nil {
				t.Fatal("the call succeeded against an answer the contract does not describe")
			}
			var violation *ContractViolation
			if !errors.As(err, &violation) {
				t.Fatalf("error is %T (%v), want a *ContractViolation", err, err)
			}
			if violation.Operation != tc.operation {
				t.Errorf("the violation names operation %q, want %q", violation.Operation, tc.operation)
			}
			if !strings.Contains(err.Error(), tc.wants) {
				t.Errorf("error reads %q, and does not mention %q, so it does not say what was wrong", err, tc.wants)
			}
			// Never one of the other three. This is the confusion that
			// matters most: an engine answering nonsense is not an engine
			// that is down and not a credential that was refused.
			if errors.Is(err, ErrUnreachable) || errors.Is(err, ErrUnauthenticated) {
				t.Errorf("a contract violation was also reported as unreachable or unauthenticated: %v", err)
			}
		})
	}
}

func TestClient_ARouteTheDeploymentDoesNotServeIsRefused(t *testing.T) {
	// An engine older or newer than this build, answering chi's bare 404
	// with no envelope at all. The client must not read that as an empty
	// success.
	engine := newFakeEngine(t)
	engine.answer["listBackupSets"] = http.NotFound
	client := engine.client(t, engine.start())

	_, err := client.ListBackupSets(context.Background())
	var violation *ContractViolation
	if !errors.As(err, &violation) {
		t.Fatalf("error is %T (%v), want a *ContractViolation", err, err)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error reads %q and does not name the status", err)
	}
}

func TestClient_RefusesWhenTheEngineIssuesNoCSRFCookie(t *testing.T) {
	// The double-submit cookie is issued by the runtime in front of every
	// response. A deployment where it never arrives cannot be talked to,
	// and the client has to say so rather than send a mutation it knows
	// will be refused with a code about a header the operator never saw.
	engine := newFakeEngine(t)
	engine.issueCSRFCookie = false
	client := engine.client(t, engine.start())

	_, err := client.CreateBackupSet(context.Background(), apicontract.CreateBackupSetRequest{})
	if err == nil {
		t.Fatal("CreateBackupSet succeeded with no CSRF token to echo")
	}
	if !strings.Contains(err.Error(), csrfCookieName) {
		t.Errorf("error reads %q and does not name the %s cookie it needed", err, csrfCookieName)
	}
}

func TestClient_ReachesTheSameEngineThroughTheUIHostsProxy(t *testing.T) {
	// Both routes are real, and this is the whole reason the client takes
	// a base URL rather than deciding one. The engine publishes no port,
	// so a CLI inside its container talks to it directly on loopback; a
	// CLI on a host has only the serve-ui port, which reverse-proxies
	// /api/v1 to that same engine unchanged. If the second needed anything
	// the first did not, "one authority" would already be false.
	engine := newFakeEngine(t)
	direct := engine.start()

	upstream, err := url.Parse(direct)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	proxy := httptest.NewServer(&httputil.ReverseProxy{Rewrite: func(r *httputil.ProxyRequest) {
		r.SetURL(upstream)
		r.Out.Host = r.In.Host
	}})
	t.Cleanup(proxy.Close)

	for _, base := range []string{direct, proxy.URL, proxy.URL + "/"} {
		client := engine.client(t, base)
		if _, err := client.ListBackupSets(context.Background()); err != nil {
			t.Errorf("ListBackupSets through %s: %v", base, err)
		}
	}
}

func TestNew_RefusesABaseURLItCannotUse(t *testing.T) {
	cases := []struct {
		name string
		base string
	}{
		{"empty", ""},
		{"no scheme", "127.0.0.1:8080"},
		{"a scheme that is not HTTP", "unix:///var/run/backup-manager.sock"},
		{"no host", "http://"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(Config{BaseURL: tc.base}); err == nil {
				t.Fatalf("New accepted %q", tc.base)
			}
		})
	}
}
