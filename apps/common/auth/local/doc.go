// Package local will implement the reusable local-account authentication
// fallback described in docs/EPIC-B-multi-nas.md §3.6: Argon2id password
// hashing, secure HTTP-only session cookies, CSRF protection, rate
// limiting, a one-time bootstrap/enrollment flow, and no plaintext
// password persistence. It is the auth mode every provider except UGOS
// uses until (or unless) it gains a native provider-auth adapter.
//
// That implementation is out of scope for #106 (B1.1, core/contract
// extraction only); it belongs to the same follow-on work as
// apps/common/webhost (see that package's doc comment). This file
// reserves the location apps/common/auth/local/ that §7's target
// repository structure calls for.
package local
