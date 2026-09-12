// Package source is the production bridge between a backup source this
// process can only reach over a transport and the backup engine that
// stores its bytes: remote object, stream, engine chunker, repository,
// with nothing on the floor in between.
//
// The anti-pattern it exists to make unavailable is the obvious one:
// copy the source to a staging directory and let the engine read the
// staging directory. That needs as much free disk as the source, writes
// every byte twice, and turns a 100 GB source into 200 GB of I/O for a
// backup that changed nothing. Nothing in this package has a local path
// to offer, which is why nothing in it can accidentally acquire one -
// the only thing that crosses from the transport is an io.ReadCloser.
//
// # What it refuses, and why refusal is the interesting half
//
// Every decision this package makes about a source is read off the
// backend capability matrix (core/internal/backend) rather than assumed,
// and the answer to a capability a backend has not declared is a refusal
// by name, before anything is dialed. A Profile is the projection of one
// backend's matrix onto the questions this adapter asks:
//
//   - streaming_open says whether an object can be read as a stream at
//     all. A backend that cannot is refused rather than fetched whole.
//   - bounded_listing says whether the source can be walked in memory
//     proportional to a buffer. A backend that cannot is refused, which
//     is why an SFTP source is backed up from a path list and never from
//     a walk (see Adapter.BackupPaths).
//   - mtime_precision says what a modification time means, and a
//     modification time no backend claims is recorded as no modification
//     time at all, not as the zero of the Unix epoch.
//   - generation_identity says whether the backend names an object's
//     CONTENT. Only "versioned" does; a path is a slot, and using a slot
//     id as a content identity is how a rewritten file passes as
//     unchanged.
//   - symlink_semantics says what a symbolic link is on the backend.
//     SymlinkPreserve on a backend that declares "skip" is a
//     configuration refusal, because storing one would mean inventing a
//     capability the backend does not have.
//
// # The three properties a run holds
//
// Bounded. One directory chunk per level of the tree (the enumerator's
// bound, see core/internal/transport.LocalEnumerator), one buffer per
// in-flight object, and a report of counters and a capped sample of
// reasons rather than one record per file. This package deliberately
// does not accumulate a sourceconsistency.Run: that type retains one
// Capture per file, which is the allocation this whole path exists to
// avoid on a source with a million of them. It reuses that package's
// vocabulary - Kind, Outcome, Stat, DescribeMovement - so an operator
// cannot tell which code path noticed that their file moved.
//
// Cancellable. A cancelled context stops the walk within one chunk,
// stops every worker, and closes every reader that was mid-read; a read
// blocked on a remote socket is unblocked by closing it from the
// outside, because a context is not something a blocked syscall
// consults.
//
// Retried in exactly one place. The adapter owns the retry bound and
// nothing below it retries: the engine is asked for one attempt per
// call. Three retrying layers stacked - the manager, the engine, the
// transport - multiply, so a bound of three at each is twenty-seven
// reads of a failing object and a backup window spent on one file.
//
// # Path safety
//
// Every path is untrusted, including the ones that came out of the
// source's own directory listing, because a source is somebody else's
// filesystem and a name in it is data. SafeRelPath is the one gate:
// traversal, absolute injection, NUL, ambiguous separators and
// non-canonical spellings are refused by name and reported, never
// cleaned into something plausible and stored. The source root cannot be
// escaped, and a fuzz target says so over arbitrary input rather than
// over the cases somebody thought of.
//
// # Mutation during read
//
// A streamed object is read once, so the confirmation a second read
// gives (sourceconsistency.Reader) is not available: the bytes have
// already gone into the repository by the time anything can be compared.
// What is available is the metadata on both sides of the read window,
// and this package uses it the only way that is honest - if size,
// modification time or generation identity moved across the read, the
// object stored on that attempt is DISCARDED from the repository and the
// read is retried within a bound. A torn read is never left behind as a
// restore point, and an object that will not hold still makes the run
// incomplete instead of being recorded as verified.
//
// # What it never does
//
// It never deletes anything on the source. Nothing in this package holds
// a transport surface that could: the two capabilities it asks for are
// "open this for reading" and "stat this", and a test asserts that the
// adapter, driven over a real tree, leaves every byte of it where it
// was.
package source
