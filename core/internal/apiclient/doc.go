// Package apiclient is the CLI's client for the engine's own /api/v1, so
// that a command run against a live deployment reaches the process holding
// that deployment's configuration instead of writing past it.
//
// # Why this exists
//
// Until this package there was no HTTP client anywhere under
// core/cmd/rbm. Every command opened its own service through
// setup.go, which is correct when nothing else is running and quietly
// wrong when something is: issue #535 records a `backup-set create` run
// through `docker exec` that succeeded, appeared in `sources`, and was
// invisible to the Web UI, because the serving process had read its
// configuration an hour earlier and has no watcher. Issue #536's rule is
// that when an engine is up, every surface is a client of it. This is the
// route that makes the CLI one.
//
// Choosing between that route and a direct one is issue #542's, and
// putting commands on it is #543's and #544's. This package only knows how
// to talk.
//
// # What it talks to
//
// Both places a CLI actually runs from, because both are real and they are
// not two APIs:
//
//	inside the engine's container   the engine's own listener, on loopback.
//	                                It publishes no port at all, so this is
//	                                the only way in from there, and it is
//	                                the process that holds the config.
//
//	from a host                     the published serve-ui port. That
//	                                container reverse-proxies /api/v1/*
//	                                to the same engine unchanged - same
//	                                path, same method, same body - and is
//	                                the only service with a LAN-facing
//	                                port.
//
// So there is one client and one base URL setting rather than a mode, and
// the second route needs nothing the first does not. If it did, "one
// authority per deployment" would already be false.
//
// # Authentication
//
// The Web UI's, not a second one. A second scheme would be issue #536's
// own defect in a new place: two things deciding who may act on one
// deployment.
//
// So this signs in the way the browser does. It asks GET
// /api/v1/auth/session, which both reports whether a session is live and
// seeds the double-submit CSRF cookie the runtime issues in front of every
// response. If there is no session it POSTs /api/v1/auth/login with the
// operator's username and password, echoing that cookie back in the
// X-CSRF-Token header, and the session then travels as the same HTTP-only
// cookie a browser carries. Every state-changing call afterwards carries
// the same double-submit pair.
//
// It deliberately does NOT read /data/state/local-auth.json, which is
// sitting right there on the same volume. That file holds a username and
// an Argon2id hash, which is not a credential anybody can present, and a
// second reader of it would be a second thing deciding who is signed in.
// The operator's password is supplied by the caller, from the environment
// or a prompt, and this package never writes it anywhere. Nor does it
// print one: Config.BaseURL refuses userinfo outright, and every address
// this package renders goes through url.URL.Redacted, because the whole
// reason BaseURL exists is that somebody prints it.
//
// What this client believes about the engine is versioned and cheap to
// re-establish rather than assumed. A 401 on an authenticated call clears
// the belief and the call is retried once, because the engine holds
// sessions in memory and a restart signs everybody out while the CLI still
// holds a cookie; and Probe answers "is an engine there, and does it know
// me" for the price of one GET, so #542 can decide and report the mode
// without spending an Argon2id verification against the engine's own login
// rate limiter. That limiter is ten logins a minute per remote IP, so a
// 429 on an operation the contract declares one for is waited out, with
// backoff, up to a whole window. Adding a CLI-only credential to dodge it
// would be the very second authority this package exists not to be.
//
// # Shapes
//
// Every request and response body is a type from core/apicontract, which
// scripts/api/generate.sh produces from api/v1/openapi.json. Nothing here
// declares a wire struct. That binding lives in core rather than beside
// the handlers precisely so this package can reach it: core may not import
// apps, and hand-writing a second copy of every body is the drift the
// generator exists to prevent.
//
// The contract is load-bearing at runtime too, not only at compile time.
// Paths come from apicontract.Endpoints rather than from string literals,
// and so do the answers each operation is allowed to give: a status the
// operation does not declare, or an error code outside the set it declares
// for that status, is refused as a ContractViolation rather than decoded
// into a zero value and reported as success.
//
// # Failures
//
// Five things happen to a call and they are five different types, listed
// in errors.go, which also carries the argument for why they are separate.
// The list is exhaustive: every failure this package returns is one of
// them, so a command in #543 or #544 can switch on them and have no
// default branch to fill in with a sentence that says nothing.
//
// # Both routes, and the transport
//
// Config.BaseURL is the address and Config.HTTPClient is how to reach it.
// The second exists because the product terminates no TLS itself: a host
// on the published port over https is going through the operator's own
// reverse proxy, very often with a certificate no public root signed, and
// without a transport hook that operator's only working option is plain
// http. Supply a client with the right roots and nothing else here
// changes, which is the same claim the two routes rest on.
package apiclient
