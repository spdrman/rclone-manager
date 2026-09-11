package apiclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/backupdproject/backupd/core/apicontract"
)

// What this client believes about the other process, and what happens when
// that belief goes stale.
//
// This is the same shape of defect the whole EPIC exists to remove: one
// process caching a fact about another process's state with nothing to
// invalidate it. #535 is a CLI holding configuration a running engine had
// already changed; a client holding "I am signed in" after the engine
// forgot is the same sentence with a different noun.

func TestClient_SignsInAgainWhenTheEngineForgotTheSession(t *testing.T) {
	engine := newFakeEngine(t)
	client := engine.client(t, engine.start())
	ctx := context.Background()

	if _, err := client.ListBackupSets(ctx); err != nil {
		t.Fatalf("the first call: %v", err)
	}

	// The engine restarts. It holds sessions in memory, so every one of
	// them is gone, while this client still has the cookie and still
	// believes it is signed in.
	engine.dropSessions()
	engine.reset()

	if _, err := client.ListBackupSets(ctx); err != nil {
		t.Fatalf("the second call, after the engine dropped its sessions: %v. This client holds credentials that would work and never tried them.", err)
	}
	want := []string{"listBackupSets", "getSession", "login", "listBackupSets"}
	if got := engine.operations(); !reflect.DeepEqual(got, want) {
		t.Errorf("the engine saw %v, want %v", got, want)
	}
}

func TestClient_DoesNotLoopAgainstAnEngineThatKeepsRefusing(t *testing.T) {
	// An engine that mints a session and then does not honour it. Whatever
	// is wrong there, retrying forever turns a fast wrong answer into a
	// hang, and a hang is the one failure an operator cannot read.
	engine := newFakeEngine(t)
	engine.answer["listBackupSets"] = func(w http.ResponseWriter, _ *http.Request) {
		engine.refuse(w, endpointByID["listBackupSets"], http.StatusUnauthorized, apicontract.ErrorCodeUnauthenticated, "authentication required")
	}
	client := engine.client(t, engine.start())

	_, err := client.ListBackupSets(context.Background())
	if err == nil {
		t.Fatal("ListBackupSets succeeded against an engine that refused every read")
	}
	if !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("error is %v, want one that satisfies errors.Is(err, ErrUnauthenticated)", err)
	}

	counts := map[string]int{}
	for _, op := range engine.operations() {
		counts[op]++
	}
	if counts["listBackupSets"] != 2 {
		t.Errorf("the read was attempted %d time(s), want exactly 2: one, and one after re-authenticating", counts["listBackupSets"])
	}
	if counts["login"] > 2 {
		t.Errorf("the client logged in %d times; a refusing engine has to cost a bounded number of password verifications", counts["login"])
	}
}

func TestClient_ConcurrentCallersOnAColdClientProduceOneLogin(t *testing.T) {
	engine := newFakeEngine(t)
	client := engine.client(t, engine.start())

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := client.ListBackupSets(context.Background()); err != nil {
				t.Errorf("ListBackupSets: %v", err)
			}
		}()
	}
	wg.Wait()

	logins := 0
	for _, op := range engine.operations() {
		if op == "login" {
			logins++
		}
	}
	if logins != 1 {
		t.Errorf("eight concurrent calls produced %d logins, want 1", logins)
	}
}

func TestClient_ASignInDoesNotHoldOtherCallersToItsOwnDeadline(t *testing.T) {
	// A caller with a short deadline must not decide how long everybody
	// else waits. Before this, ensureSession held the mutex across the
	// whole handshake, so the second caller sat behind the first caller's
	// context rather than its own.
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		http.NotFound(w, r)
	}))
	t.Cleanup(slow.Close)
	t.Cleanup(func() { close(release) })

	client, err := New(Config{BaseURL: slow.URL, Username: "operator", Password: "correct-horse-battery"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	started := make(chan struct{})
	go func() {
		close(started)
		_, _ = client.ListBackupSets(first)
	}()
	<-started

	second, cancelSecond := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelSecond()
	done := make(chan error, 1)
	go func() {
		_, err := client.ListBackupSets(second)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("the second caller failed with %v, want its own deadline", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the second caller was still waiting two seconds after its own 50ms deadline, so it is waiting on the first caller's context rather than on its own")
	}
}

// Session used to cost three round trips on a cold client and two on a
// warm one, because getSession is itself an authenticated operation: call
// ran ensureSession, which asked getSession, and then call asked again.
//
// The traffic is the small half. The design consequence is that there was
// no way to ask "is an engine there and am I signed in" without spending
// an Argon2id verification against the engine's login rate limiter, and
// #542 needs exactly that question answered cheaply.
func TestClient_SessionDoesNotAskTheSameQuestionTwice(t *testing.T) {
	engine := newFakeEngine(t)
	client := engine.client(t, engine.start())
	ctx := context.Background()

	got, err := client.Session(ctx)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if got.Username != engine.username {
		t.Errorf("Session reported %q, want %q. This is the response that says whether a session is live and whose it is.", got.Username, engine.username)
	}
	want := []string{"getSession", "login", "getSession"}
	if seen := engine.operations(); !reflect.DeepEqual(seen, want) {
		t.Errorf("a cold Session made %v, want %v", seen, want)
	}

	engine.reset()
	if _, err := client.Session(ctx); err != nil {
		t.Fatalf("Session: %v", err)
	}
	if seen := engine.operations(); !reflect.DeepEqual(seen, []string{"getSession"}) {
		t.Errorf("a warm Session made %v, want exactly one getSession", seen)
	}

	// And the case where the answer was already in hand. The engine's
	// session is still live and this client has stopped believing in it,
	// which is where both a 401 retry and a Probe leave it. The check that
	// re-establishes the belief IS getSession, so its answer is the answer
	// Session was asked for.
	client.mu.Lock()
	client.signedIn = false
	client.mu.Unlock()
	engine.reset()
	if _, err := client.Session(ctx); err != nil {
		t.Fatalf("Session: %v", err)
	}
	if seen := engine.operations(); !reflect.DeepEqual(seen, []string{"getSession"}) {
		t.Errorf("Session made %v; the session check is the question, and asking it twice spends a round trip on an answer already in hand", seen)
	}
}

func TestClient_ASessionNamingNobodyIsNotALiveSession(t *testing.T) {
	// The contract makes SessionResponse.username required, and this is
	// the one response whose whole job is to say whether a session is live
	// and whose it is. A 200 with nobody in it answers neither, and
	// treating err == nil as "signed in" is how a client ends up acting on
	// a session it does not have.
	engine := newFakeEngine(t)
	engine.answer["getSession"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"username":""}`))
	}
	client := engine.client(t, engine.start())

	_, err := client.Session(context.Background())
	var violation *ContractViolation
	if !errors.As(err, &violation) {
		t.Fatalf("error is %T (%v), want a *ContractViolation", err, err)
	}
	if !strings.Contains(err.Error(), "username") {
		t.Errorf("error reads %q and does not say what was missing", err)
	}
}

func TestClient_ProbeAnswersWithoutSpendingAPassword(t *testing.T) {
	t.Run("nothing is listening", func(t *testing.T) {
		closed := httptest.NewServer(http.NotFoundHandler())
		base := closed.URL
		closed.Close()

		client, err := New(Config{BaseURL: base, Username: "operator", Password: "correct-horse-battery"})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		got, err := client.Probe(context.Background())
		if !errors.Is(err, ErrUnreachable) {
			t.Fatalf("Probe returned %v, want one that satisfies errors.Is(err, ErrUnreachable)", err)
		}
		if got.Reachable || got.Authenticated {
			t.Errorf("Probe reported %+v against a closed port", got)
		}
	})

	t.Run("an engine that does not know this client", func(t *testing.T) {
		engine := newFakeEngine(t)
		client := engine.client(t, engine.start())

		got, err := client.Probe(context.Background())
		if err != nil {
			t.Fatalf("Probe: %v", err)
		}
		if !got.Reachable {
			t.Error("Probe reported an engine that answered as unreachable")
		}
		if got.Authenticated {
			t.Errorf("Probe reported %+v; nothing had signed in", got)
		}
		// The whole point: no password was spent to find that out.
		if seen := engine.operations(); !reflect.DeepEqual(seen, []string{"getSession"}) {
			t.Errorf("Probe made %v, want exactly one getSession and no login", seen)
		}
	})

	t.Run("an engine that does know this client", func(t *testing.T) {
		engine := newFakeEngine(t)
		client := engine.client(t, engine.start())
		if _, err := client.ListBackupSets(context.Background()); err != nil {
			t.Fatalf("ListBackupSets: %v", err)
		}
		engine.reset()

		got, err := client.Probe(context.Background())
		if err != nil {
			t.Fatalf("Probe: %v", err)
		}
		if !got.Reachable || !got.Authenticated || got.Username != engine.username {
			t.Errorf("Probe reported %+v, want reachable and signed in as %q", got, engine.username)
		}
		if seen := engine.operations(); !reflect.DeepEqual(seen, []string{"getSession"}) {
			t.Errorf("Probe made %v, want exactly one getSession", seen)
		}
	})
}

// rateLimitedLogin refuses the first refusals logins with 429
// RATE_LIMITED, the way apps/common/auth/local's fixed-window limiter does
// once one host has signed in ten times inside a minute.
func rateLimitedLogin(engine *fakeEngine, refusals int) {
	var mu sync.Mutex
	seen := 0
	engine.answer["login"] = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen++
		limited := seen <= refusals
		mu.Unlock()
		if limited {
			engine.refuse(w, endpointByID["login"], http.StatusTooManyRequests, apicontract.ErrorCodeRateLimited, "too many login attempts")
			return
		}
		engine.login(w, r, endpointByID["login"])
	}
}

// The login rate limit is ten per minute keyed by remote IP, and this
// package's own design signs in once per CLI invocation. So the eleventh
// mutation from one host inside a minute is refused with 429, and a script
// creating twenty backup sets is an ordinary thing to do. Waiting is the
// right answer; failing is not.
func TestClient_WaitsOutTheLoginRateLimit(t *testing.T) {
	engine := newFakeEngine(t)
	rateLimitedLogin(engine, 2)
	client := engine.client(t, engine.start())

	var waited []time.Duration
	client.sleep = func(_ context.Context, d time.Duration) error {
		waited = append(waited, d)
		return nil
	}

	if _, err := client.ListBackupSets(context.Background()); err != nil {
		t.Fatalf("ListBackupSets against an engine that rate-limited the first two logins: %v", err)
	}
	if len(waited) != 2 {
		t.Fatalf("the client waited %v, want two waits for two refusals", waited)
	}
	if waited[1] <= waited[0] {
		t.Errorf("the client waited %v; a retry that does not back off is a retry that keeps the limiter shut", waited)
	}
}

func TestClient_GivesUpOnAnEngineThatAlwaysRateLimits(t *testing.T) {
	engine := newFakeEngine(t)
	rateLimitedLogin(engine, 1000)
	client := engine.client(t, engine.start())

	total := time.Duration(0)
	waits := 0
	client.sleep = func(_ context.Context, d time.Duration) error {
		total += d
		waits++
		return nil
	}

	err := client.Login(context.Background())
	if err == nil {
		t.Fatal("Login succeeded against an engine that refused every attempt")
	}
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Code != apicontract.ErrorCodeRateLimited {
		t.Fatalf("error is %v, want the engine's own RATE_LIMITED refusal", err)
	}
	if waits == 0 {
		t.Error("the client gave up without waiting at all, so an ordinary provisioning script still breaks")
	}
	if total < time.Minute {
		t.Errorf("the client waited %v in total; the limiter's window is a minute, so giving up sooner gives up before the thing it is waiting for can happen", total)
	}
	if total > 5*time.Minute {
		t.Errorf("the client waited %v in total, which is a hang rather than a slow failure", total)
	}
}

func TestClient_DoesNotWaitOutAStatusTheOperationDoesNotDeclare(t *testing.T) {
	// listBackupSets declares no 429 at all. Something answering 429 there
	// is not this API's rate limiter, and sitting out a minute for it would
	// be waiting on a fact nobody stated.
	engine := newFakeEngine(t)
	engine.answer["listBackupSets"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"RATE_LIMITED","message":"nope"}}`))
	}
	client := engine.client(t, engine.start())
	client.sleep = func(context.Context, time.Duration) error {
		t.Error("the client waited out a 429 on an operation the contract does not declare one for")
		return nil
	}

	_, err := client.ListBackupSets(context.Background())
	var violation *ContractViolation
	if !errors.As(err, &violation) {
		t.Fatalf("error is %T (%v), want a *ContractViolation", err, err)
	}
}

func TestClient_TalksHTTPSThroughAnOperatorsOwnCertificate(t *testing.T) {
	// The product terminates no TLS itself, so a host reaching the
	// published port over HTTPS goes through the operator's reverse proxy,
	// very often with a NAS-issued or self-signed certificate. Without a
	// transport hook that operator can only use plain http.
	engine := newFakeEngine(t)
	base, trusting := engine.startTLS()

	// The negative control, and the state of things before this hook: with
	// nothing but Go's own roots, that certificate cannot be verified and
	// the operator's only working option is plain http.
	plain, err := New(Config{BaseURL: base, Username: engine.username, Password: engine.password})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := plain.ListBackupSets(context.Background()); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("a client trusting only Go's roots reached a certificate nothing public signed: %v", err)
	}

	client, err := New(Config{BaseURL: base, Username: engine.username, Password: engine.password, HTTPClient: trusting})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.ListBackupSets(context.Background()); err != nil {
		t.Fatalf("ListBackupSets over HTTPS with the operator's own root: %v", err)
	}

	// The supplied client is the caller's, and may be shared. New takes
	// what it needs from it rather than reaching into it.
	if trusting.Jar != nil {
		t.Error("New installed its cookie jar on the caller's own http.Client")
	}
	if trusting.CheckRedirect != nil {
		t.Error("New installed its redirect policy on the caller's own http.Client")
	}
}

func TestClient_NamesItselfOnEveryRequest(t *testing.T) {
	engine := newFakeEngine(t)
	base := engine.start()

	client := engine.client(t, base)
	if _, err := client.ListBackupSets(context.Background()); err != nil {
		t.Fatalf("ListBackupSets: %v", err)
	}
	for _, r := range engine.requests() {
		if r.UserAgent == "" || strings.HasPrefix(r.UserAgent, "Go-http-client") {
			t.Errorf("%s identified as %q; #543 claims the same audit trail as the Web UI, and an audit trail that says Go-http-client names no product", r.Operation, r.UserAgent)
		}
	}

	engine.reset()
	named, err := New(Config{BaseURL: base, Username: engine.username, Password: engine.password, UserAgent: "backupd/1.2.3 (test)"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := named.ListBackupSets(context.Background()); err != nil {
		t.Fatalf("ListBackupSets: %v", err)
	}
	for _, r := range engine.requests() {
		if r.UserAgent != "backupd/1.2.3 (test)" {
			t.Errorf("%s identified as %q, want the caller's own UserAgent", r.Operation, r.UserAgent)
		}
	}
}
