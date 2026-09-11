package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/backupdproject/backupd/core/apicontract"
)

// The three names on the wire that the generated binding does not carry.
//
// api/v1/openapi.json declares all three as security schemes, and
// apps/common/auth/local and apps/common/csrf implement them, but core may
// not import apps and the generator emits shapes rather than schemes. So
// this package holds its own copy, and contract_test.go reads the contract
// document and compares, because a constant copied across an import
// boundary that nothing compares is a constant that drifts.
//
// A client only strictly needs two of them. The session cookie is carried
// by the cookie jar without anybody naming it; it is named here so the
// comparison covers it and so a reader can see the whole credential set in
// one place.
const (
	sessionCookieName = "bm_session"
	csrfCookieName    = "bm_csrf"
	csrfHeaderName    = "X-CSRF-Token"
)

// defaultTimeout bounds one request, not one command. Every operation this
// package reaches is a configuration read or write against a local
// process; none of them is a transfer. A long-running job is submitted
// through POST /operations and polled, so no legitimate call here is slow,
// and a minute of a CLI hanging on a wedged engine is a minute an operator
// spends wondering whether it worked.
const defaultTimeout = 30 * time.Second

// defaultUserAgent names the product and the API version it speaks, so a
// request from this client is one an operator can pick out of an access
// log. "Go-http-client/1.1" names no product, no version and no surface,
// and #543's claim is that a command taking this route leaves the same
// audit trail as the Web UI.
//
// The BINARY's own version is deliberately not read here. internal/app
// reads build info to answer `version`, and its doc is explicit that a
// second reader of it would be a second answer to a question that has one.
// A caller that already knows its build (cmd/backupd does, from
// -ldflags) says so through Config.UserAgent, and this is what stands in
// until one does.
const defaultUserAgent = "backupd-cli (api " + apicontract.Version + ")"

// maxResponseBytes caps what is read from one response. The largest thing
// on this API is a backup-set or artifact listing, and the cap exists so
// that something on the far end which is not the engine cannot make the
// CLI read forever.
const maxResponseBytes = 32 << 20

// endpointByID indexes the generated contract so a call names an operation
// rather than a path. Built once; the contract is a compile-time constant
// in every sense that matters here.
var endpointByID = func() map[string]apicontract.Endpoint {
	out := make(map[string]apicontract.Endpoint, len(apicontract.Endpoints))
	for _, ep := range apicontract.Endpoints {
		out[ep.ID] = ep
	}
	return out
}()

// Config is everything New needs.
type Config struct {
	// BaseURL is the engine's /api/v1 host, WITHOUT the /api/v1 suffix:
	// either the engine's own listener from inside its container
	// (http://127.0.0.1:8080) or the published serve-ui port from a host
	// (http://nas.local:8080), which proxies to the same engine. Deciding
	// which is issue #542's; this package is told.
	BaseURL string

	// Username and Password are the local administrator's, the same pair
	// the Web UI's login page takes. They are held in memory for the life
	// of this client and written nowhere.
	//
	// Leaving them empty is legitimate and means "use an existing
	// session", which is a shape this client can be in when something else
	// established one. With neither a session nor credentials, a call
	// fails as ErrUnauthenticated and says which of the two was missing.
	Username string
	Password string

	// Timeout bounds one request. Zero means defaultTimeout.
	Timeout time.Duration

	// HTTPClient, when non-nil, supplies the transport. Nil keeps a
	// default one, which is what every caller inside a container wants.
	//
	// It exists for the other route. The product terminates no TLS itself,
	// so a host reaching the published port over https goes through the
	// operator's own reverse proxy, very often presenting a NAS-issued or
	// self-signed certificate that no public root signed. Without this
	// that operator's only working option is plain http. Set
	// Transport.TLSClientConfig.RootCAs here and the rest of this package
	// is unchanged, because the two routes are one API.
	//
	// New takes what it needs from this rather than trusting what arrives:
	// it copies the value and installs its own cookie jar, timeout and
	// redirect policy on the copy, so a client shared with something else
	// is not altered and its session cookies do not leak into a jar this
	// package did not make.
	HTTPClient *http.Client

	// UserAgent is what every request identifies itself as. Empty means
	// defaultUserAgent.
	//
	// #543's claim is that a command run through this route leaves the
	// same audit trail as the Web UI, and a log line reading
	// "Go-http-client/1.1" names no product, no version and no surface. A
	// caller that knows its own build (cmd/backupd does, from
	// -ldflags) should say so here.
	UserAgent string
}

// Client talks to one engine. Safe for concurrent use: the sign-in is
// serialised so that several calls racing on a cold client produce one
// login rather than one each.
type Client struct {
	base *url.URL
	http *http.Client

	username string
	password string

	userAgent string

	// sleep waits between attempts. It is a field so the suite can prove
	// this client sits out a full rate-limit window without spending a
	// real minute doing it.
	sleep func(context.Context, time.Duration) error

	// mu guards nothing but the three fields below, and is never held
	// across the network. See ensureSession for why that matters.
	mu sync.Mutex
	// signedIn is what this client believes about the OTHER process, which
	// is why generation exists next to it: the belief is versioned so that
	// a 401 invalidates the session it was answered under and not a newer
	// one somebody else established in the meantime.
	signedIn   bool
	generation uint64
	// signingIn is non-nil while one caller is doing the handshake, and is
	// closed when it finishes. Others wait on it, or on their own context.
	signingIn chan struct{}
}

// ProbeResult is the three answers Probe distinguishes, which are the
// three an operator has different things to do about.
type ProbeResult struct {
	// Reachable is whether anything answered at all.
	Reachable bool
	// Authenticated is whether the engine already knows this client, which
	// is asked and answered without spending a password.
	Authenticated bool
	// Username is who the engine says this client is, when it is anybody.
	Username string
}

// New validates cfg and returns a client for it.
//
// The URL is checked here rather than at the first call because a
// mistyped address is a configuration error, and finding it out at the
// moment of a mutation is the worst time to find it out.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, &ConfigError{Field: "BaseURL", Reason: "no base URL: this client has to be told where the engine is"}
	}
	base, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil {
		return nil, &ConfigError{Field: "BaseURL", Value: cfg.BaseURL, Reason: "this is not a URL: " + err.Error(), Err: err}
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, &ConfigError{Field: "BaseURL", Value: cfg.BaseURL, Reason: fmt.Sprintf("it has scheme %q, and /api/v1 is served over http or https", base.Scheme)}
	}
	if base.Host == "" {
		return nil, &ConfigError{Field: "BaseURL", Value: cfg.BaseURL, Reason: "it names no host"}
	}
	if base.RawQuery != "" || base.Fragment != "" {
		// Dropping them quietly would be worse than refusing: an operator
		// who put something there meant it, and a client that discards
		// half of what it was given and then works is a client nobody can
		// reason about the next time it does not.
		return nil, &ConfigError{Field: "BaseURL", Value: cfg.BaseURL, Reason: "it carries a query or fragment, and this is the engine's address, not a request"}
	}
	if base.User != nil {
		// Same reasoning, and one more of its own. BaseURL() exists so a
		// command can print this address (#542 reports the mode rather than
		// inferring it) and Unreachable names it in the failure most likely
		// to be pasted into a support ticket, so accepting a password here
		// puts one on a terminal. It buys nothing either: Go would attach it
		// as Authorization: Basic, and /api/v1 authenticates with a session
		// cookie, not Basic. The credentials go in Config.Username and
		// Config.Password, which are never printed.
		//
		// The message deliberately does not echo what was given.
		return nil, &ConfigError{Field: "BaseURL", Value: base.Redacted(), Reason: "it carries a username or password, and /api/v1 signs in with Config.Username and Config.Password instead; this address is printed"}
	}
	base.Path = strings.TrimSuffix(base.Path, "/")
	if strings.HasSuffix(base.Path, apicontract.BasePath) {
		// The single likeliest mistake: pasting the API's own URL rather
		// than the host's. Left alone it builds /api/v1/api/v1/... and
		// every call 404s, which reads as an engine that does not speak
		// this API rather than as an address with one segment too many.
		return nil, &ConfigError{Field: "BaseURL", Value: cfg.BaseURL, Reason: fmt.Sprintf("it already ends in %s, and this takes the engine's address and appends %s itself", apicontract.BasePath, apicontract.BasePath)}
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, &ConfigError{Reason: "the session cookie jar could not be built: " + err.Error(), Err: err}
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	// A copy, not the caller's own client. Whatever they built it for -
	// a root CA the host trusts and Go does not, a proxy, a dialer - is
	// carried over in the Transport, while the three things this package
	// is entitled to decide are set here rather than assumed. Reaching
	// into their value instead would put this package's session cookies in
	// a jar shared with whatever else uses that client.
	transport := http.Client{}
	if cfg.HTTPClient != nil {
		transport = *cfg.HTTPClient
	}
	transport.Jar = jar
	transport.Timeout = timeout
	// A redirect is not part of this API. Returning the 3xx unfollowed
	// sends it through the ordinary status check below, which refuses it
	// as a status the operation does not declare, rather than silently
	// replaying a mutation somewhere else.
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	userAgent := strings.TrimSpace(cfg.UserAgent)
	if userAgent == "" {
		userAgent = defaultUserAgent
	}

	return &Client{
		base:      base,
		username:  cfg.Username,
		password:  cfg.Password,
		userAgent: userAgent,
		sleep:     waitFor,
		http:      &transport,
	}, nil
}

// BaseURL is the address this client talks to, for a command that has to
// name it in its output (#542 requires the mode to be reported, never
// inferred).
func (c *Client) BaseURL() string { return c.base.Redacted() }

// call performs one contract operation, signing in first if the operation
// requires a session.
//
// pathArgs fill the operation's path parameters in the order the contract
// declares them. body, when non-nil, is marshalled as the request body.
// out, when non-nil, receives the decoded response.
func (c *Client) call(ctx context.Context, operation string, pathArgs []string, body any, out any) error {
	return c.callQuery(ctx, operation, pathArgs, nil, body, out)
}

// callQuery is call for the one operation on this API that takes an
// argument somewhere other than the path or the body.
//
// GET /backups carries its backup-set filter as ?setId=, and until issue
// #544 nothing in this package could send a query at all. That mattered
// more than a missing feature usually does: a filter this client dropped
// would not fail, it would answer `artifacts --backup-set X` with every
// artifact in the deployment, which is a wrong answer wearing a right
// one's clothes.
//
// It is spelled as a second entry point rather than a fifth parameter on
// call because forty-odd operations take no query and a nil argument at
// every one of those call sites is forty places for the wrong value to be
// invisible. The values are escaped by net/url, so a backup set named with
// a space or an ampersand travels intact.
func (c *Client) callQuery(ctx context.Context, operation string, pathArgs []string, query url.Values, body any, out any) error {
	ep, err := endpoint(operation)
	if err != nil {
		return err
	}
	if !ep.Authenticated {
		return c.do(ctx, ep, pathArgs, query, body, out)
	}

	state, err := c.ensureSession(ctx)
	if err != nil {
		return err
	}
	err = c.do(ctx, ep, pathArgs, query, body, out)
	if !isSessionRefusal(err) {
		return err
	}

	// The engine says this caller has no session, and this caller thought
	// it had one. Something ended it: a restart, which drops every session
	// because they live in memory, or the twenty-four hours running out.
	// Returning the 401 while holding credentials that would work is the
	// EPIC's own defect in miniature - a cached belief about another
	// process with nothing to invalidate it - and it is load-bearing the
	// moment a command creates something and reads it back.
	//
	// Sending the request again is safe because a 401 is answered by the
	// authentication middleware, in front of every handler: the request
	// was refused before it did anything, so nothing happened that a
	// second attempt would repeat. That is true of a mutation as much as
	// of a read, and it is the only reason this may retry at all. The body
	// is re-encoded from the same value rather than replayed from a
	// consumed reader.
	//
	// Exactly once. An engine that mints a session and then refuses it is
	// broken in a way no amount of retrying fixes, and a loop would turn a
	// fast wrong answer into a hang.
	c.forget(state.generation)
	if _, err := c.ensureSession(ctx); err != nil {
		return err
	}
	return c.do(ctx, ep, pathArgs, query, body, out)
}

// endpoint resolves a contract operation id.
//
// Every path in this package comes through here rather than out of a map
// lookup nobody checked. This is the one failure mode that would otherwise
// be a silently mistyped string, and a client that asks for a route the
// engine does not serve is exactly issue #211's defect.
func endpoint(operation string) (apicontract.Endpoint, error) {
	ep, found := endpointByID[operation]
	if !found {
		return apicontract.Endpoint{}, &ContractViolation{
			Operation: operation,
			Reason:    "api/v1/openapi.json declares no such operation, so there is no path to build",
		}
	}
	return ep, nil
}

// do is call without the sign-in, so that the sign-in itself can use it
// without recursion.
func (c *Client) do(ctx context.Context, ep apicontract.Endpoint, pathArgs []string, query url.Values, body any, out any) error {
	path, err := fillPath(ep, pathArgs)
	if err != nil {
		return &ContractViolation{Operation: ep.ID, Method: ep.Method, Reason: err.Error()}
	}
	// Parsed rather than assembled field by field. fillPath has already
	// escaped every path parameter, and assigning that string to
	// url.URL.Path would escape it a second time when the URL is rendered,
	// turning a set named "a b" into one named "a%20b".
	target, err := url.Parse(c.base.String() + apicontract.BasePath + path)
	if err != nil {
		return &ContractViolation{Operation: ep.ID, Method: ep.Method, Reason: "the request URL could not be built: " + err.Error()}
	}
	// Assigned rather than appended to whatever the base URL carried:
	// New refuses a base URL with a query of its own, so there is never
	// anything here to preserve, and Encode escapes each value.
	if len(query) > 0 {
		target.RawQuery = query.Encode()
	}

	var encoded []byte
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			// A body this client cannot encode is this client breaching
			// the contract's request schema, found before anything was
			// sent, which is what ContractViolation's Status 0 is for.
			schema := ep.RequestSchema
			if schema == "" {
				schema = "request body"
			}
			return &ContractViolation{
				Operation: ep.ID,
				Method:    ep.Method,
				Reason:    "what was handed over is not something the contract's " + schema + " can be encoded as: " + err.Error(),
			}
		}
	}

	for attempt := 0; ; attempt++ {
		req, err := c.newRequest(ctx, ep, target, body != nil, encoded)
		if err != nil {
			return err
		}

		resp, err := c.http.Do(req)
		if err != nil {
			// Every failure here happened before a response existed: a
			// refused connection, an unresolvable name, a timeout, a
			// cancelled context. The cause is kept on the error so a
			// caller can still reach context.DeadlineExceeded through
			// errors.Is.
			return &Unreachable{Operation: ep.ID, BaseURL: c.base.Redacted(), Err: unwrapURLError(err)}
		}

		if wait, ok := rateLimitWait(ep, resp, attempt); ok {
			drain(resp)
			if err := c.wait(ctx, wait); err != nil {
				return &Unreachable{Operation: ep.ID, BaseURL: c.base.Redacted(), Err: err}
			}
			continue
		}

		err = c.receive(ep, target.EscapedPath(), resp, out)
		drain(resp)
		return err
	}
}

// newRequest builds one attempt. It is per-attempt rather than built once
// because a request carries its body as a reader, which a retry would find
// already consumed, and because the double-submit token can be issued by
// the very response that refused the attempt before it.
func (c *Client) newRequest(ctx context.Context, ep apicontract.Endpoint, target *url.URL, hasBody bool, encoded []byte) (*http.Request, error) {
	var payload io.Reader
	if hasBody {
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, ep.Method, target.String(), payload)
	if err != nil {
		return nil, &ContractViolation{
			Operation: ep.ID,
			Method:    ep.Method,
			Path:      target.EscapedPath(),
			Reason:    "the request could not be built: " + err.Error(),
		}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	if ep.CSRFRequired {
		token := c.csrfToken()
		if token == "" {
			// The runtime issues this cookie in front of every response it
			// serves, so its absence is the deployment's, not the
			// caller's. Sending the mutation anyway would earn a 403 about
			// a header the operator never saw and cannot supply.
			return nil, &ContractViolation{
				Operation: ep.ID,
				Method:    ep.Method,
				Path:      target.EscapedPath(),
				Reason: fmt.Sprintf("the engine issued no %s cookie, so the %s header this operation requires cannot be echoed back",
					csrfCookieName, csrfHeaderName),
			}
		}
		req.Header.Set(csrfHeaderName, token)
	}
	return req, nil
}

// The rate limit this client is on the wrong side of, and why waiting is
// the only honest answer to it.
//
// apps/common/auth/local allows ten logins a minute per remote IP, in a
// fixed window, and this package's whole design signs in once per CLI
// invocation and lets the session expire. So a provisioning script making
// its eleventh change from one host inside a minute is refused with 429
// RATE_LIMITED, and there is nothing wrong with what it did. The
// alternatives are worse than waiting: a CLI-only credential would be a
// second thing deciding who may act on one deployment, which is the defect
// #536 exists to remove, and failing the eleventh command is failing an
// ordinary thing to do.
//
// The ceiling is chosen to cover that window. Doubling from two seconds,
// five waits reach sixty-two, which is a whole fixed window plus a
// margin, so a client that gives up has waited out the thing it was
// waiting for rather than giving up before it could happen. Bounded,
// because the failure has to be slow rather than absent.
const (
	rateLimitAttempts     = 5
	firstRateLimitBackoff = 2 * time.Second
	maxRateLimitBackoff   = 32 * time.Second
)

// rateLimitWait reports how long to wait before trying this request again,
// and whether to wait at all.
//
// Only for an operation the contract declares a 429 for. Something
// answering 429 where the contract names no such refusal is not this API's
// rate limiter, and sitting out a minute for it would be waiting on a fact
// nobody stated; that answer goes to receive, which reports it as the
// contract violation it is.
func rateLimitWait(ep apicontract.Endpoint, resp *http.Response, attempt int) (time.Duration, bool) {
	if resp.StatusCode != http.StatusTooManyRequests || attempt >= rateLimitAttempts {
		return 0, false
	}
	if !slices.Contains(ep.ErrorCodes[http.StatusTooManyRequests], apicontract.ErrorCodeRateLimited) {
		return 0, false
	}

	backoff := min(firstRateLimitBackoff<<attempt, maxRateLimitBackoff)
	// The engine sends no Retry-After today, but a reverse proxy in front
	// of it may, and a limiter that has said how long to wait knows better
	// than a schedule guessing at it.
	if asked, ok := retryAfter(resp); ok && asked > backoff {
		backoff = min(asked, maxRateLimitBackoff)
	}
	return backoff, true
}

// retryAfter reads the header in either of the two forms RFC 9110 allows.
func retryAfter(resp *http.Response) (time.Duration, bool) {
	raw := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if raw == "" {
		return 0, false
	}
	if seconds, err := strconv.Atoi(raw); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, true
	}
	if when, err := http.ParseTime(raw); err == nil {
		if d := time.Until(when); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}

// wait sleeps, through the seam the suite replaces.
func (c *Client) wait(ctx context.Context, d time.Duration) error {
	if c.sleep != nil {
		return c.sleep(ctx, d)
	}
	return waitFor(ctx, d)
}

// waitFor is the real wait, and it is interruptible: a caller that gave up
// while this client was sitting out a rate limit gets its own context's
// answer rather than the rest of the wait.
func waitFor(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// drain reads and closes a response so its connection can be reused, and
// caps what it reads so something on that port which is not the engine
// cannot make the CLI read forever.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	_ = resp.Body.Close()
}

// receive holds the engine's answer to what the contract says this
// operation may answer, and only decodes once it has passed.
func (c *Client) receive(ep apicontract.Endpoint, path string, resp *http.Response, out any) error {
	violation := func(reason string) error {
		return &ContractViolation{Operation: ep.ID, Method: ep.Method, Path: path, Status: resp.StatusCode, Reason: reason}
	}

	if resp.StatusCode == ep.SuccessStatus {
		if ep.ResponseSchema == "" || out == nil {
			return nil
		}
		if err := requireJSON(resp); err != nil {
			return violation(err.Error())
		}
		dec := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes))
		if err := dec.Decode(out); err != nil {
			return violation(fmt.Sprintf("the body is not the %s the contract declares: %v", ep.ResponseSchema, err))
		}
		if dec.More() {
			return violation(fmt.Sprintf("the body is a %s followed by trailing content, and a response with more than one value in it is not one this contract describes", ep.ResponseSchema))
		}
		return nil
	}

	declared, ok := ep.ErrorCodes[resp.StatusCode]
	if !ok {
		return violation(fmt.Sprintf("the operation declares %d on success and %s on failure", ep.SuccessStatus, statusList(ep)))
	}

	code, message, correlationID, err := decodeRefusal(resp)
	if err != nil {
		return violation(err.Error())
	}
	if !slices.Contains(declared, code) {
		return violation(fmt.Sprintf("the code %q is not one of the %d the operation declares at %d (%s)",
			code, len(declared), resp.StatusCode, codeList(declared)))
	}
	return &Error{
		Operation:     ep.ID,
		Method:        ep.Method,
		Path:          path,
		Status:        resp.StatusCode,
		Code:          code,
		Message:       message,
		CorrelationID: correlationID,
	}
}

// decodeRefusal reads whichever of the two error envelopes this route
// uses.
//
// There are two, and the disagreement is real rather than something this
// client could tidy up: apps/common/auth/local answers flat, because
// ui/shared's login page reads that shape, and apps/common/webhost nests
// code and message under an "error" key. Both are in the contract, as
// AuthErrorResponse and ErrorResponse, and reading only one would turn the
// other's refusals into an empty code, which then fails the declared-code
// check for entirely the wrong reason.
func decodeRefusal(resp *http.Response) (apicontract.ErrorCode, string, string, error) {
	if err := requireJSON(resp); err != nil {
		return "", "", "", err
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", "", "", fmt.Errorf("the body could not be read: %v", err)
	}

	var nested apicontract.ErrorResponse
	if err := json.Unmarshal(raw, &nested); err == nil && nested.Error.Code != "" {
		return nested.Error.Code, nested.Error.Message, resp.Header.Get("X-Correlation-Id"), nil
	}

	var flat apicontract.AuthErrorResponse
	if err := json.Unmarshal(raw, &flat); err == nil && flat.Code != "" {
		correlationID := flat.CorrelationID
		if correlationID == "" {
			correlationID = resp.Header.Get("X-Correlation-Id")
		}
		return flat.Code, flat.Message, correlationID, nil
	}

	return "", "", "", errors.New("the body carries neither the contract's ErrorResponse nor its AuthErrorResponse, so the refusal names no code this client can act on")
}

// requireJSON refuses a body that is not declared as JSON before anything
// tries to parse it. The message names the type that did arrive, because
// "text/html" is the single most useful word in this whole failure: it
// means something answered on that port that is not the engine.
func requireJSON(resp *http.Response) error {
	raw := resp.Header.Get("Content-Type")
	if raw == "" {
		return errors.New("the response carries no Content-Type, and every body this contract describes is JSON")
	}
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return fmt.Errorf("the response's Content-Type %q could not be parsed: %v", raw, err)
	}
	if mediaType != "application/json" {
		return fmt.Errorf("the response's Content-Type is %q, and every body this contract describes is application/json", raw)
	}
	return nil
}

// csrfToken returns the double-submit cookie's current value, or "" when
// nothing has issued one yet.
func (c *Client) csrfToken() string {
	for _, cookie := range c.http.Jar.Cookies(c.base) {
		if cookie.Name == csrfCookieName {
			return cookie.Value
		}
	}
	return ""
}

// fillPath substitutes an operation's path parameters, escaping each one.
// A count mismatch is a failure rather than a best effort: a path built
// with a parameter left as "{id}" is a path the engine has no route for.
//
// Escaping per parameter is the point rather than a detail. A value is
// data, so a separator inside one becomes %2F and cannot invent a segment
// the contract did not declare; an identity that really does span segments
// is spelled as the segments it is, one parameter each, which is what
// api/v1/openapi.json now publishes.
//
// The two values that survive that escape untouched are refused here
// instead. "." and ".." are legal path characters, so url.PathEscape
// returns them unchanged and a caller could otherwise talk this client
// into building "/backup-sets/../settings": a path the contract never
// declared, assembled by the very function whose job is that only the
// contract decides. They are refused as VALUES, not as substrings, because
// a dot is an ordinary character in a backup's name and "..2026-08-30.dump"
// is a file somebody has.
func fillPath(ep apicontract.Endpoint, args []string) (string, error) {
	var b strings.Builder
	rest := ep.Path
	used := 0
	for {
		open := strings.IndexByte(rest, '{')
		if open < 0 {
			b.WriteString(rest)
			break
		}
		closeIdx := strings.IndexByte(rest[open:], '}')
		if closeIdx < 0 {
			return "", fmt.Errorf("the contract's path %q for this operation is malformed", ep.Path)
		}
		b.WriteString(rest[:open])
		if used >= len(args) {
			return "", fmt.Errorf("the contract's path %q takes more parameters than the %d supplied", ep.Path, len(args))
		}
		if args[used] == "" {
			return "", fmt.Errorf("the contract's path %q takes a value for %s and was given an empty one", ep.Path, rest[open:open+closeIdx+1])
		}
		if args[used] == "." || args[used] == ".." {
			return "", fmt.Errorf("the contract's path %q takes a value for %s and was given %q, which is a relative path segment rather than a name; it would build a path this contract does not declare", ep.Path, rest[open:open+closeIdx+1], args[used])
		}
		b.WriteString(url.PathEscape(args[used]))
		used++
		rest = rest[open+closeIdx+1:]
	}
	if used != len(args) {
		return "", fmt.Errorf("the contract's path %q takes %d parameter(s) and was given %d", ep.Path, used, len(args))
	}
	return b.String(), nil
}

// statusList and codeList render what an operation was allowed to say, so
// a violation message tells a reader what the contract expected rather
// than only what arrived.
func statusList(ep apicontract.Endpoint) string {
	if len(ep.ErrorCodes) == 0 {
		return "nothing"
	}
	statuses := slices.Sorted(maps.Keys(ep.ErrorCodes))
	parts := make([]string, 0, len(statuses))
	for _, status := range statuses {
		parts = append(parts, fmt.Sprint(status))
	}
	return strings.Join(parts, ", ")
}

func codeList(codes []apicontract.ErrorCode) string {
	parts := make([]string, 0, len(codes))
	for _, code := range codes {
		parts = append(parts, string(code))
	}
	slices.Sort(parts)
	return strings.Join(parts, ", ")
}

// unwrapURLError strips net/http's *url.Error wrapper, which repeats the
// method and the full URL in its own message. Keeping it would print the
// address twice in an error that already names it once, for no extra fact.
func unwrapURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return err
}
