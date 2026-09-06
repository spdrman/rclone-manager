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

// ensureSession makes sure this client has a live session, signing in if
// it does not.
//
// The GET is not a formality. It is what the browser does on load, it
// reports whether something else already established a session, and it is
// the response that seeds the double-submit CSRF cookie that login itself
// then has to echo. A client that went straight to POST /auth/login would
// be refused with CSRF_TOKEN_MISSING every time, on a first run, for a
// reason that reads like a bug in the server.
func (c *Client) ensureSession(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.signedIn {
		return nil
	}

	var session apicontract.SessionResponse
	err := c.do(ctx, endpointByID["getSession"], nil, nil, &session)
	if err == nil {
		c.signedIn = true
		return nil
	}

	// Anything other than "you are not signed in" is this call's answer,
	// not a reason to try a password: an unreachable engine or a body the
	// contract does not describe must not be reported as a credential
	// problem.
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Status != http.StatusUnauthorized {
		return err
	}

	if c.username == "" || c.password == "" {
		return &Error{
			Operation: "login",
			Method:    http.MethodPost,
			Path:      apicontract.BasePath + "/auth/login",
			Status:    http.StatusUnauthorized,
			Code:      apicontract.ErrorCodeUnauthenticated,
			Message:   "no credentials were supplied and no session exists, so there is nothing to sign in with",
		}
	}

	if err := c.do(ctx, endpointByID["login"], nil,
		apicontract.CredentialsRequest{Username: c.username, Password: c.password}, nil); err != nil {
		return err
	}
	c.signedIn = true
	return nil
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
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.username == "" || c.password == "" {
		return &Error{
			Operation: "login",
			Method:    http.MethodPost,
			Path:      apicontract.BasePath + "/auth/login",
			Status:    http.StatusUnauthorized,
			Code:      apicontract.ErrorCodeUnauthenticated,
			Message:   "no credentials were supplied, so there is nothing to sign in with",
		}
	}

	// The CSRF cookie has to exist before login can echo it, and on a cold
	// client nothing has issued one yet. This read is what does it.
	if c.csrfToken() == "" {
		var session apicontract.SessionResponse
		if err := c.do(ctx, endpointByID["getSession"], nil, nil, &session); err != nil {
			var refusal *Error
			if !errors.As(err, &refusal) || refusal.Status != http.StatusUnauthorized {
				return err
			}
		}
	}

	c.signedIn = false
	if err := c.do(ctx, endpointByID["login"], nil,
		apicontract.CredentialsRequest{Username: c.username, Password: c.password}, nil); err != nil {
		return err
	}
	c.signedIn = true
	return nil
}

// Session asks the engine who this client is signed in as, signing in
// first if it is not.
//
// It always asks rather than answering from what it remembers. The whole
// point of the question is what the SERVING process believes, and a client
// answering it from its own memory would be the same shape of defect as a
// CLI reporting configuration the engine has not read.
func (c *Client) Session(ctx context.Context) (apicontract.SessionResponse, error) {
	var out apicontract.SessionResponse
	err := c.call(ctx, "getSession", nil, nil, &out)
	return out, err
}

// Logout ends the session on the engine.
func (c *Client) Logout(ctx context.Context) error {
	err := c.call(ctx, "logout", nil, nil, nil)
	c.mu.Lock()
	c.signedIn = false
	c.mu.Unlock()
	return err
}
