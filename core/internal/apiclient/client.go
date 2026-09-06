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
	"strings"
	"sync"
	"time"

	"github.com/spdrman/rclone-manager/core/apicontract"
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
}

// Client talks to one engine. Safe for concurrent use: the sign-in is
// serialised so that several calls racing on a cold client produce one
// login rather than one each.
type Client struct {
	base *url.URL
	http *http.Client

	username string
	password string

	mu       sync.Mutex
	signedIn bool
}

// New validates cfg and returns a client for it.
//
// The URL is checked here rather than at the first call because a
// mistyped address is a configuration error, and finding it out at the
// moment of a mutation is the worst time to find it out.
func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("apiclient: no base URL: this client has to be told where the engine is")
	}
	base, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil {
		return nil, fmt.Errorf("apiclient: base URL %q: %w", cfg.BaseURL, err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return nil, fmt.Errorf("apiclient: base URL %q has scheme %q; /api/v1 is served over http or https", cfg.BaseURL, base.Scheme)
	}
	if base.Host == "" {
		return nil, fmt.Errorf("apiclient: base URL %q names no host", cfg.BaseURL)
	}
	base.Path = strings.TrimSuffix(base.Path, "/")
	base.RawQuery, base.Fragment = "", ""

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("apiclient: cookie jar: %w", err)
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	return &Client{
		base:     base,
		username: cfg.Username,
		password: cfg.Password,
		http: &http.Client{
			Jar:     jar,
			Timeout: timeout,
			// A redirect is not part of this API. Returning the 3xx
			// unfollowed sends it through the ordinary status check
			// below, which refuses it as a status the operation does not
			// declare, rather than silently replaying a mutation
			// somewhere else.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// BaseURL is the address this client talks to, for a command that has to
// name it in its output (#542 requires the mode to be reported, never
// inferred).
func (c *Client) BaseURL() string { return c.base.String() }

// call performs one contract operation, signing in first if the operation
// requires a session.
//
// pathArgs fill the operation's path parameters in the order the contract
// declares them. body, when non-nil, is marshalled as the request body.
// out, when non-nil, receives the decoded response.
func (c *Client) call(ctx context.Context, operation string, pathArgs []string, body any, out any) error {
	ep, ok := endpointByID[operation]
	if !ok {
		// Unreachable from this package's own methods, and checked anyway:
		// this is the one failure mode that would otherwise be a silently
		// mistyped string, and a client that asks for a route the engine
		// does not serve is exactly issue #211's defect.
		return &ContractViolation{
			Operation: operation,
			Reason:    "api/v1/openapi.json declares no such operation, so there is no path to build",
		}
	}
	if ep.Authenticated {
		if err := c.ensureSession(ctx); err != nil {
			return err
		}
	}
	return c.do(ctx, ep, pathArgs, body, out)
}

// do is call without the sign-in, so that the sign-in itself can use it
// without recursion.
func (c *Client) do(ctx context.Context, ep apicontract.Endpoint, pathArgs []string, body any, out any) error {
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

	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("apiclient: %s: encoding the request body: %w", ep.ID, err)
		}
		payload = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, ep.Method, target.String(), payload)
	if err != nil {
		return fmt.Errorf("apiclient: %s: building the request: %w", ep.ID, err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if ep.CSRFRequired {
		token := c.csrfToken()
		if token == "" {
			// The runtime issues this cookie in front of every response it
			// serves, so its absence is the deployment's, not the
			// caller's. Sending the mutation anyway would earn a 403 about
			// a header the operator never saw and cannot supply.
			return &ContractViolation{
				Operation: ep.ID,
				Method:    ep.Method,
				Path:      target.EscapedPath(),
				Reason: fmt.Sprintf("the engine issued no %s cookie, so the %s header this operation requires cannot be echoed back",
					csrfCookieName, csrfHeaderName),
			}
		}
		req.Header.Set(csrfHeaderName, token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// Every failure here happened before a response existed: a refused
		// connection, an unresolvable name, a timeout, a cancelled
		// context. The cause is kept on the error so a caller can still
		// reach context.DeadlineExceeded through errors.Is.
		return &Unreachable{Operation: ep.ID, BaseURL: c.base.String(), Err: unwrapURLError(err)}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		_ = resp.Body.Close()
	}()

	return c.receive(ep, target.EscapedPath(), resp, out)
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
