package apiclient

import (
	"context"
	"errors"
	"net/http"

	"github.com/spdrman/rclone-manager/core/apicontract"
)

// Signing in, which is the whole of this package's claim to be one
// authority rather than a second one.
//
// There is no credential here that the Web UI does not also present, and
// no endpoint here the Web UI does not also call. The browser asks GET
// /api/v1/auth/session on load, POSTs /api/v1/auth/login when that says
// nobody is signed in, and carries the resulting cookie; so does this. A
// CLI-only token, a shared secret in the image, or a socket that skips
// authentication because the caller is already inside the container would
// each be a second thing deciding who may act on one deployment, which is
// exactly the defect issue #536 exists to remove.
//
// # Why /data/state/local-auth.json is not read
//
// It is on the same volume, it is readable, and it holds the enrolled
// administrator's username. It holds an Argon2id hash next to it, which is
// not a credential anybody can present: verifying a password against it
// means having the password already. A client that read it could only
// learn who the administrator is, and the one thing that would be useful
// for - telling "no account exists yet" apart from "wrong password" - is a
// distinction apps/common/auth/local deliberately refuses to make, paying
// a full Argon2id hash on every failing branch so that neither the body
// nor the clock gives it away. Reading it to answer a question the service
// declines to answer would undo that on purpose.
//
// # Sessions are not cached across processes
//
// The engine holds sessions in memory and a restart signs everybody out.
// A CLI invocation therefore signs in, does its work, and lets the session
// expire; it writes no token to disk. That costs one Argon2id verification
// per command, which is the price of not having a credential file that
// outlives the process that made it.
//
// # What this client is allowed to believe
//
// As little as possible, and never for longer than the last thing the
// engine said. "I am signed in" is a fact about the OTHER process, and a
// client holding it after the engine has forgotten is #535's own defect
// with a different noun in it: one process caching another's state with
// nothing to invalidate it. So the belief is versioned (see sessionState),
// the engine's own 401 clears it, and Probe re-establishes it without
// spending a password.

// sessionState is what one trip through ensureSession learned.
type sessionState struct {
	// generation identifies the session this call was let through on. A
	// caller that later meets a 401 hands it back to forget, so that only
	// the belief it actually acted on is invalidated and a session
	// established since then by somebody else is left alone.
	generation uint64

	// session and known carry the engine's own answer, when establishing
	// the session is what produced one. GET /auth/session is both the
	// check and the question Session asks, so throwing its answer away and
	// asking again is a round trip spent on a fact already in hand.
	session apicontract.SessionResponse
	known   bool
}

// ensureSession makes sure this client has a live session, signing in if
// it does not.
//
// The GET is not a formality. It is what the browser does on load, it
// reports whether something else already established a session, and it is
// the response that seeds the double-submit CSRF cookie that login itself
// then has to echo. A client that went straight to POST /auth/login would
// be refused with CSRF_TOKEN_MISSING every time, on a first run, for a
// reason that reads like a bug in the server.
//
// # Why the lock is not held across the handshake
//
// Several calls racing on a cold client still produce one login: the first
// to arrive signs in and the rest wait on the channel it leaves behind.
// But they wait on THEIR OWN contexts as well, because holding the mutex
// across the network would make one caller's deadline everybody's
// deadline, and a command with a five-second budget must not be able to
// pin a background refresh to it, or the other way round.
//
// A waiter whose leader failed loops and tries the handshake itself rather
// than inheriting a failure it did not make. That terminates: every
// iteration either returns, or waits for a sign-in somebody else has
// already begun, and each of those is bounded by the waiter's own context.
func (c *Client) ensureSession(ctx context.Context) (sessionState, error) {
	for {
		if err := ctx.Err(); err != nil {
			return sessionState{}, c.abandoned(err)
		}

		c.mu.Lock()
		if c.signedIn {
			state := sessionState{generation: c.generation}
			c.mu.Unlock()
			return state, nil
		}
		if inflight := c.signingIn; inflight != nil {
			c.mu.Unlock()
			select {
			case <-inflight:
				continue
			case <-ctx.Done():
				return sessionState{}, c.abandoned(ctx.Err())
			}
		}
		inflight := make(chan struct{})
		c.signingIn = inflight
		c.mu.Unlock()

		state, err := c.signIn(ctx)

		c.mu.Lock()
		if err == nil {
			c.signedIn = true
			c.generation++
			state.generation = c.generation
		}
		c.signingIn = nil
		c.mu.Unlock()
		close(inflight)

		return state, err
	}
}

// signIn is the handshake itself, run by exactly one caller at a time and
// holding no lock while it does.
func (c *Client) signIn(ctx context.Context) (sessionState, error) {
	session, err := c.readSession(ctx)
	if err == nil {
		return sessionState{session: session, known: true}, nil
	}

	// Anything other than "you are not signed in" is this call's answer,
	// not a reason to try a password: an unreachable engine or a body the
	// contract does not describe must not be reported as a credential
	// problem.
	if !isSessionRefusal(err) {
		return sessionState{}, err
	}

	if c.username == "" || c.password == "" {
		return sessionState{}, &NoCredentials{
			Operation: "login",
			Reason:    "no credentials were supplied and no session exists, so there is nothing to sign in with",
		}
	}

	ep, err := endpoint("login")
	if err != nil {
		return sessionState{}, err
	}
	if err := c.do(ctx, ep, nil, nil, apicontract.CredentialsRequest{Username: c.username, Password: c.password}, nil); err != nil {
		return sessionState{}, err
	}
	// Login answers 204 with no body, so the engine has not yet said whose
	// session this now is. known stays false, and a caller that needs the
	// name asks for it.
	return sessionState{}, nil
}

// readSession is GET /auth/session, and the one place that decides what
// counts as a live session.
//
// A 200 is not enough on its own. The contract makes SessionResponse's
// username required, and this response's whole job is to say whether a
// session is live and whose it is; a body naming nobody answers neither,
// and treating err == nil as "signed in" is how a client ends up acting on
// a session it does not have.
func (c *Client) readSession(ctx context.Context) (apicontract.SessionResponse, error) {
	ep, err := endpoint("getSession")
	if err != nil {
		return apicontract.SessionResponse{}, err
	}
	var out apicontract.SessionResponse
	if err := c.do(ctx, ep, nil, nil, nil, &out); err != nil {
		return apicontract.SessionResponse{}, err
	}
	if err := requireNamedSession(out); err != nil {
		return apicontract.SessionResponse{}, err
	}
	return out, nil
}

// requireNamedSession is the check itself, kept apart from the request so
// that both ways of asking the question apply it.
func requireNamedSession(session apicontract.SessionResponse) error {
	if session.Username != "" {
		return nil
	}
	ep, err := endpoint("getSession")
	if err != nil {
		return err
	}
	return &ContractViolation{
		Operation: ep.ID,
		Method:    ep.Method,
		Status:    http.StatusOK,
		Reason:    "the body is a SessionResponse carrying no username, and the contract requires one; a session belonging to nobody is not a session",
	}
}

// forget drops this client's belief that it is signed in, but only if the
// belief being dropped is the one the caller acted on. A 401 answered a
// request that went out under a particular session, and invalidating a
// newer one established since would cost an extra login for no reason.
func (c *Client) forget(generation uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.generation == generation {
		c.signedIn = false
	}
}

// remember records a session the engine has just confirmed, and starts a
// new generation so an in-flight 401 from before the confirmation cannot
// throw it away.
func (c *Client) remember(signedIn bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.signedIn = signedIn
	c.generation++
}

// isSessionRefusal reports whether an error is the engine saying that this
// caller has no session, which is the only refusal a password answers.
func isSessionRefusal(err error) bool {
	var refusal *Error
	return errors.As(err, &refusal) && refusal.Status == http.StatusUnauthorized
}

// abandoned reports a caller's own context ending while it waited for a
// sign-in. It is Unreachable rather than anything about credentials: no
// engine answered, and the reason is kept so errors.Is still reaches
// context.Canceled or context.DeadlineExceeded.
func (c *Client) abandoned(err error) error {
	return &Unreachable{Operation: "login", BaseURL: c.base.Redacted(), Err: err}
}

// Login signs in, whether or not this client already believes it has a
// session.
//
// Separate from ensureSession because they answer different questions.
// ensureSession asks "can this call proceed", and reusing a live session
// is the right answer to that: an engine that hashed a password once per
// command would spend Argon2id's cost per command and burn its own login
// rate limit. Login asks "prove these credentials", which only a real
// round trip can answer.
func (c *Client) Login(ctx context.Context) error {
	if c.username == "" || c.password == "" {
		// Not an *Error. Nothing was contacted, so nothing refused
		// anything, and the base URL may point at nothing at all.
		return &NoCredentials{
			Operation: "login",
			Reason:    "no credentials were supplied, so there is nothing to sign in with",
		}
	}

	// The CSRF cookie has to exist before login can echo it, and on a cold
	// client nothing has issued one yet. This read is what does it.
	if c.csrfToken() == "" {
		if _, err := c.readSession(ctx); err != nil && !isSessionRefusal(err) {
			return err
		}
	}

	c.remember(false)
	ep, err := endpoint("login")
	if err != nil {
		return err
	}
	if err := c.do(ctx, ep, nil, nil, apicontract.CredentialsRequest{Username: c.username, Password: c.password}, nil); err != nil {
		return err
	}
	c.remember(true)
	return nil
}

// Probe reports whether an engine is there and whether it already knows
// this client, for the price of one GET.
//
// #542 has to decide and REPORT which route a command took, and before
// this there was no way to ask that question without signing in: Session
// went through the ordinary path, so asking "am I signed in" spent an
// Argon2id verification against the engine's own login rate limiter. The
// three answers Probe distinguishes are the three that matter - an engine
// that knows this caller, an engine that does not, and no engine at all -
// and they have three different things to print.
//
// "Not signed in" is an answer rather than a failure, so it comes back
// with a nil error. Only a failure to get any answer at all is an error.
func (c *Client) Probe(ctx context.Context) (ProbeResult, error) {
	session, err := c.readSession(ctx)
	if err == nil {
		c.remember(true)
		return ProbeResult{Reachable: true, Authenticated: true, Username: session.Username}, nil
	}
	if isSessionRefusal(err) {
		// The engine answered, so it is there; and it says this client is
		// nobody, so whatever it believed before is stale.
		c.remember(false)
		return ProbeResult{Reachable: true}, nil
	}
	return ProbeResult{}, err
}

// Session asks the engine who this client is signed in as, signing in
// first if it is not.
//
// It always asks rather than answering from what it remembers. The whole
// point of the question is what the SERVING process believes, and a client
// answering it from its own memory would be the same shape of defect as a
// CLI reporting configuration the engine has not read.
//
// It asks once, though. The session check IS this question, so when
// establishing the session is what produced an answer, that answer is the
// one returned rather than the first of two identical requests.
func (c *Client) Session(ctx context.Context) (apicontract.SessionResponse, error) {
	state, err := c.ensureSession(ctx)
	if err != nil {
		return apicontract.SessionResponse{}, err
	}
	if state.known {
		return state.session, nil
	}
	var out apicontract.SessionResponse
	if err := c.call(ctx, "getSession", nil, nil, &out); err != nil {
		return apicontract.SessionResponse{}, err
	}
	if err := requireNamedSession(out); err != nil {
		return apicontract.SessionResponse{}, err
	}
	return out, nil
}

// Logout ends the session on the engine, and is worth calling from a CLI
// even though the process is about to exit: the engine holds sessions for
// twenty-four hours from creation, so a command that signs in and walks
// away leaves one live for the rest of the day.
//
// A client that never signed in has nothing to end and does not sign in to
// do it. That branch is not a micro-optimisation: the contract marks
// logout as authenticated, so going through the ordinary path would have
// this present a password purely in order to throw the resulting session
// away, which is a real credential use for no effect.
func (c *Client) Logout(ctx context.Context) error {
	c.mu.Lock()
	signedIn := c.signedIn
	c.mu.Unlock()
	if !signedIn {
		return nil
	}

	ep, err := endpoint("logout")
	if err != nil {
		return err
	}
	err = c.do(ctx, ep, nil, nil, nil, nil)
	// Unconditional, and whatever the engine answered: this client asked
	// for the session to end, so it has no business acting as though it
	// still has one.
	c.remember(false)
	return err
}
