package rclone

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	rclonefs "github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"

	"github.com/spdrman/rclone-manager/core/internal/transport"

	"github.com/spdrman/rclone-manager/core/internal/testenv"
	"github.com/spdrman/rclone-manager/core/tests/bwlimit"
)

// ---------------------------------------------------------------------------
// Unit-level classification: no Docker, exercised against errors this
// adapter's local backend and the standard library actually return. These
// exist mainly so the fast, always-available case for each category has a
// test that never depends on Docker being present, and they double-check the
// os.ErrNotExist/os.ErrPermission fallback paths transport_test.go's
// "translation shape" tests already found survive unwrapped alongside
// rclone's own sentinels (see the comment in errors.go's PermissionDenied
// case).
// ---------------------------------------------------------------------------

// TestClassify_Nil pins the one input that has no failure to describe.
// Unclassified rather than Permanent, because Permanent is a verdict and
// there is nothing here to have a verdict about; CategoryOf makes the same
// distinction from the other side.
func TestClassify_Nil(t *testing.T) {
	if got := classify(nil); got != transport.Unclassified {
		t.Fatalf("classify(nil) = %v, want Unclassified", got)
	}
}

// TestClassify_IsIdempotentOnAnAlreadyWrappedError matters because this
// adapter wraps at every layer an error passes through, so double-wrapping
// is a mistake somebody will make. An error that already carries a verdict
// keeps it rather than being re-derived from a chain that now has a
// classified error in the middle of it, where the sentinel checks below
// would reach a different answer.
func TestClassify_IsIdempotentOnAnAlreadyWrappedError(t *testing.T) {
	wrapped := transport.NewError(transport.PermissionDenied, "stat", errors.New("boom"))
	if got := classify(wrapped); got != transport.PermissionDenied {
		t.Fatalf("classify(already-wrapped) = %v, want the category it already carried (PermissionDenied)", got)
	}
}

// TestClassify_NotFound_RealLocalError runs the same absent path through
// three adapter methods rather than one, because each reaches rclone
// differently and what comes back is not the same sentinel in every case:
// rclone's own fs.ErrorObjectNotFound in some, the standard library's
// os.ErrNotExist surviving unwrapped in others. errors.go checks for both,
// and this is the case that says why it has to.
func TestClassify_NotFound_RealLocalError(t *testing.T) {
	ctx := context.Background()
	adapter := New()
	source := transport.Source{ID: "cls-notfound", Type: "local", Root: t.TempDir()}

	cases := map[string]func() error{
		"Stat": func() error {
			_, err := adapter.Stat(ctx, source, "missing.txt")
			return err
		},
		"CopyToLocal": func() error {
			_, err := adapter.CopyToLocal(ctx, source, "missing.txt", filepath.Join(t.TempDir(), "out"))
			return err
		},
		"DeleteRemote": func() error {
			return adapter.DeleteRemote(ctx, source, "missing.txt")
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil {
				t.Fatalf("%s succeeded against a missing object", name)
			}
			if got := classify(err); got != transport.NotFound {
				t.Fatalf("classify(%v) = %v, want NotFound", err, got)
			}
		})
	}
}

// TestClassify_PermissionDenied_RealLocalError is the case
// errors.go's PermissionDenied comment is about: Stat cannot observe a
// chmod-000 file on a POSIX filesystem (no read permission on the file
// itself is needed to stat it, only traversal permission on its containing
// directories), so this has to actually open and read the file, which is
// what RemoteHash does for the local backend.
func TestClassify_PermissionDenied_RealLocalError(t *testing.T) {
	testenv.RequirePermissionBitsApply(t)

	ctx := context.Background()
	root := t.TempDir()
	full := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(full, []byte("shh"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chmod(full, 0o000); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	defer func() { _ = os.Chmod(full, 0o644) }()

	adapter := New()
	source := transport.Source{ID: "cls-permdenied", Type: "local", Root: root}
	_, err := adapter.RemoteHash(ctx, source, "secret.txt", transport.SHA256)
	if err == nil {
		t.Fatalf("RemoteHash succeeded reading a chmod 000 file")
	}
	if got := classify(err); got != transport.PermissionDenied {
		t.Fatalf("classify(%v) = %v, want PermissionDenied", err, got)
	}
}

// TestClassify_Cancelled_RealContextError reuses contract.go's testCancel
// technique directly against the adapter: a context cancelled before the
// call starts must classify as Cancelled, not as some flavour of failure the
// adapter itself caused.
func TestClassify_Cancelled_RealContextError(t *testing.T) {
	root := t.TempDir()
	content := bytes.Repeat([]byte("cancel-me-"), 4096)
	if err := os.WriteFile(filepath.Join(root, "big.bin"), content, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	adapter := New()
	source := transport.Source{ID: "cls-cancelled", Type: "local", Root: root}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := adapter.CopyToLocal(ctx, source, "big.bin", filepath.Join(t.TempDir(), "big.bin.partial"))
	if err == nil {
		t.Fatalf("CopyToLocal succeeded against an already-cancelled context")
	}
	if got := classify(err); got != transport.Cancelled {
		t.Fatalf("classify(%v) = %v, want Cancelled", err, got)
	}
}

// TestClassify_Transient_RealConnectionRefused drives the real sftp backend
// (through this adapter, exactly as production code would) at a TCP port
// nothing is listening on. The dial failure this produces is genuine
// OS/network behaviour, not anything rclone or this adapter manufactures,
// and it is exactly the shape of error rclone's own fs/fserrors.ShouldRetry
// exists to recognize, which is what classify defers to for Transient.
func TestClassify_Transient_RealConnectionRefused(t *testing.T) {
	dir := t.TempDir()
	keyPath, _ := generateClientSSHKeyPair(t) // needs to parse, never needs to authenticate
	knownHostsPath := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(knownHostsPath, nil, 0o600); err != nil {
		t.Fatalf("writing empty known_hosts: %v", err)
	}

	port := freeTCPPort(t) // reserved and released immediately: nothing listens here
	source := transport.Source{
		ID:         "cls-transient",
		Type:       "sftp",
		Host:       "127.0.0.1",
		Port:       port,
		User:       "backup",
		KeyFile:    keyPath,
		KnownHosts: knownHostsPath,
	}

	adapter := New()
	_, err := adapter.List(context.Background(), source)
	if err == nil {
		t.Fatalf("List succeeded against a port nothing is listening on")
	}
	if got := classify(err); got != transport.Transient {
		t.Fatalf("classify(%v) = %v, want Transient", err, got)
	}
}

// ---------------------------------------------------------------------------
// Issue #388: rclone's own connect timeout is a network condition, not a
// cancellation, and the caller's context is the only thing that can tell the
// two apart.
//
// rclone builds both of its dials with --contimeout: fs/fshttp's NewDialer
// sets net.Dialer.Timeout from ci.ConnectTimeout, and backend/sftp sets
// ssh.ClientConfig.Timeout from the same value. When one of those fires, the
// *net.OpError underneath usually carries *net.timeoutError, whose Is method
// answers true for context.DeadlineExceeded, so a plain errors.Is check reads
// a timeout nobody asked for as a cancellation somebody did.
//
// "Usually" is the other half of why this test asserts a category and never
// an error identity. Both of net.Dialer's deadlines (the socket's and the
// context's) expire on the same instant, and net's connect() returns
// whichever one it notices first: *net.timeoutError when the context timer
// has already run, *poll.DeadlineExceededError (which has no Is method at
// all, so errors.Is never sees a deadline in it) when it has not.
//
// Measured against 192.0.2.1 on the machine this was written on: a bare
// net.Dialer, one dial at a time, 29 of 30 drew *net.timeoutError; with 30
// dials in flight that fell to 122 of 300, and with 50 in flight to 416 of
// 1000. Through rclone's own sftp dial, one at a time, it was 33 of 40. Same
// network condition, two error shapes, and before this fix two different
// categories, with the odds depending on how busy the machine is.
// ---------------------------------------------------------------------------

// The address row 1 dials is TEST-NET-1 (RFC 5737), reserved for
// documentation and routed nowhere, because it has to blackhole rather than
// refuse: a closed port refuses instantly, which is a different error on a
// different code path and never reaches a connect timeout at all.
const (
	blackholedHost = "192.0.2.1"
	blackholedPort = 9000
	// contimeoutForTest is what this test hands rclone as --contimeout. It
	// is the deadline row 1 expects to be enforced, and the floor row 1's
	// elapsed-time precondition checks against.
	contimeoutForTest = 500 * time.Millisecond
	// connectTimeoutSamples is how many times row 1 dials. One dial samples
	// one side of the race described above, so it dials several times and
	// demands the same category from every one of them, which is the part
	// of the claim that holds whichever side each dial draws.
	//
	// It used to carry a second job: at least one dial had to draw the
	// shape that used to be misclassified, or the row proved nothing. That
	// worked out at three in a hundred thousand on the machine it was
	// written on (33 of 40 dials there) and nothing like it on a busy one,
	// where the same measurement fell to roughly two in five, and it failed
	// twice running once the gate's Go suites moved under -race (#417). So
	// that job moved to connectTimeoutShapeThatUsedToBeCancelled, which
	// makes the same claim with no dial in it, and this number is back to
	// being only about sampling both sides of a real dial.
	connectTimeoutSamples = 6
)

func TestClassify_ConnectTimeoutRcloneImposedIsTransient(t *testing.T) {
	cases := []struct {
		name string
		// run performs one or more real operations, returns the caller
		// context they were given and every error they produced, and fails
		// the test itself if its own preconditions did not hold (see each
		// one: an assertion about the category is worthless if the scenario
		// it names never happened). Row 1 returns several errors on purpose,
		// because one dial only samples one side of the race described
		// above; every one of them has to land in the same category.
		run  func(t *testing.T) (context.Context, []error)
		want transport.Category
	}{
		{
			name: "rclone's own connect timeout, caller context never asked for anything",
			run:  rcloneImposedConnectTimeout,
			want: transport.Transient,
		},
		{
			// Row 1's discriminating shape, with the network's coin
			// toss taken out of it. See the function's own doc.
			name: "an error that carries a deadline nobody in this process set",
			run:  connectTimeoutShapeThatUsedToBeCancelled,
			want: transport.Transient,
		},
		{
			name: "caller context cancelled before the call started",
			run:  callerCancelledBeforeTheCall,
			want: transport.Cancelled,
		},
		{
			name: "caller deadline that genuinely fires mid-copy",
			run:  callerDeadlineFiresMidCopy,
			want: transport.Cancelled,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, errs := tc.run(t)
			if len(errs) == 0 {
				t.Fatal("the operation produced no error, so there is nothing to classify")
			}
			for i, err := range errs {
				if err == nil {
					t.Fatalf("attempt %d succeeded, so there is nothing to classify", i+1)
				}
				if got := ClassifyCtx(ctx, err); got != tc.want {
					t.Fatalf("attempt %d: ClassifyCtx = %v, want %v (error was: %v)", i+1, got, tc.want, err)
				}
			}
		})
	}
}

// TestClassifyCtx_ADoneContextDoesNotOverrideADefiniteError pins the
// precedence #388 forced a decision about, Docker-free and in every
// direction at once.
//
// ClassifyCtx consults the caller's context because an expired deadline in
// an error cannot say whose deadline it was. That is the only thing the
// context is for. An error that already says something definite keeps
// saying it, however done the context is, because the alternative loses a
// host-key or authentication refusal to any caller that works under a
// deadline, and app/halt.go is the one place that turns those into
// something an operator reads.
//
// The values here are real: two rclone sentinels, rclone's own
// corrupted-on-transfer wording (see integrityFailurePrefix), and this
// package's own ErrUnsupportedHash. The changed-host-key case gets the same
// assertion against a live fixture in
// TestSFTPClassificationAgainstRealFixture.
func TestClassifyCtx_ADoneContextDoesNotOverrideADefiniteError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want transport.Category
	}{
		{
			name: "this package's own unsupported-hash sentinel",
			err:  fmt.Errorf("%w: backend %q cannot compute %s", ErrUnsupportedHash, "sftp", transport.SHA256),
			want: transport.UnsupportedCapability,
		},
		{
			name: "rclone's own object-not-found sentinel",
			err:  fmt.Errorf("stat %q: %w", "missing.dump", rclonefs.ErrorObjectNotFound),
			want: transport.NotFound,
		},
		{
			name: "rclone's own corrupted-on-transfer wording",
			err:  fmt.Errorf("corrupted on transfer: sizes differ src(%s) %d vs dst(%s) %d", "srcFs", 10, "dstFs", 5),
			want: transport.IntegrityFailure,
		},
		{
			name: "a deadline the error carries and cannot attribute, which is what the context is for",
			err:  fmt.Errorf("copy_to_local: %w", context.DeadlineExceeded),
			want: transport.Cancelled,
		},
		{
			name: "an explicit cancellation, which needs no context to read",
			err:  fmt.Errorf("copy_to_local: %w", context.Canceled),
			want: transport.Cancelled,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyCtx(alreadyCancelledContext(), tc.err); got != tc.want {
				t.Fatalf("ClassifyCtx(done ctx, %v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// rcloneImposedConnectTimeout drives the real sftp backend, through this
// adapter exactly as production code does, at an address that blackholes,
// with --contimeout set and a caller context that has no deadline and is
// never cancelled. The only deadline anywhere in this scenario is rclone's
// own, which is the whole point.
func rcloneImposedConnectTimeout(t *testing.T) (context.Context, []error) {
	t.Helper()

	dir := t.TempDir()
	keyPath, _ := generateClientSSHKeyPair(t) // needs to parse, never needs to authenticate
	knownHostsPath := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(knownHostsPath, nil, 0o600); err != nil {
		t.Fatalf("writing empty known_hosts: %v", err)
	}

	ctx, ci := rclonefs.AddConfig(context.Background())
	ci.ConnectTimeout = rclonefs.Duration(contimeoutForTest)
	// One attempt, so the elapsed-time precondition below reads as one
	// connect timeout rather than however many rclone felt like stacking up.
	ci.LowLevelRetries = 1

	source := transport.Source{
		ID:         "cls-388-contimeout",
		Type:       "sftp",
		Host:       blackholedHost,
		Port:       blackholedPort,
		User:       "backup",
		KeyFile:    keyPath,
		KnownHosts: knownHostsPath,
	}

	var (
		errs               []error
		carriedDeadlineErr int
	)
	for attempt := 1; attempt <= connectTimeoutSamples; attempt++ {
		start := time.Now()
		_, err := New().List(ctx, source)
		elapsed := time.Since(start)

		if err == nil {
			t.Fatalf("attempt %d: List succeeded against %s:%d, which is supposed to be unroutable", attempt, blackholedHost, blackholedPort)
		}
		// Preconditions first, because both of them are ways this row could
		// go green without ever reaching a connect timeout.
		//
		// A network that answers "no route to host" or "connection refused"
		// instead of blackholing returns in milliseconds, and that error is
		// Transient today with or without this fix, so the row would prove
		// nothing at all.
		if elapsed < contimeoutForTest {
			t.Fatalf("attempt %d: List gave up after %s, well short of the %s connect timeout it was given; "+
				"this network answers %s:%d instead of blackholing it, so nothing here reached a connect timeout: %v",
				attempt, elapsed, contimeoutForTest, blackholedHost, blackholedPort, err)
		}
		// And the caller context has to be genuinely untouched, or
		// "Transient" would just be describing a context that never expired
		// for a different reason than the one this row is about.
		if ctxErr := ctx.Err(); ctxErr != nil {
			t.Fatalf("attempt %d: the caller context is done (%v), so this row cannot say anything about rclone's own deadline", attempt, ctxErr)
		}

		if errors.Is(err, context.DeadlineExceeded) {
			carriedDeadlineErr++
		}
		errs = append(errs, err)
		t.Logf("attempt %d: connect timeout after %s; errors.Is(err, context.DeadlineExceeded) = %v; err = %v",
			attempt, elapsed, errors.Is(err, context.DeadlineExceeded), err)
	}

	// Which shape this run drew, reported and not asserted on.
	//
	// It used to be asserted on: at least one of these dials had to carry
	// context.DeadlineExceeded, because the other shape was already
	// Transient before #388 and a run that drew only that one proved
	// nothing about the fix. The trouble is that which shape a dial draws
	// is decided by which of two deadlines the kernel notices first, and
	// that is a property of how busy the machine is. This row's own
	// comment records it falling from 29-in-30 on a quiet machine to
	// roughly 2-in-5 with dials in flight, and putting the gate's Go
	// suites under -race (#417) pushed it further the same way: the
	// assertion failed on both of the first two full runs under the
	// detector, having drawn 0 of 6, on a tree with nothing wrong with
	// it.
	//
	// So the claim it was carrying moved somewhere it can be made
	// without a coin toss. connectTimeoutShapeThatUsedToBeCancelled is
	// that row: the exact shape, classified against a live context,
	// every time. This one keeps the part only a real dial can prove,
	// that rclone's own connect timeout is reached at all and is
	// Transient whichever shape it arrives in, and reports the draw for
	// whoever is reading a failure.
	t.Logf("%d of %d connect timeouts carried context.DeadlineExceeded (the shape that read as Cancelled before #388)", carriedDeadlineErr, connectTimeoutSamples)
	return ctx, errs
}

// connectTimeoutShapeThatUsedToBeCancelled is the deterministic half of
// row 1: an error that carries an expired deadline, handed to ClassifyCtx
// with a context that never asked for one.
//
// That pair is the whole of #388. rclone sets its own --contimeout on
// both dials, and when one fires the error underneath answers
// errors.Is(err, context.DeadlineExceeded) true, so a classifier reading
// the error alone calls it Cancelled: "the operator decided", which
// retry.DefaultIsTransient will not retry. The fix was to consult the
// caller's context, and the only input that exercises it is an error
// carrying a deadline while the context is live.
//
// Fabricated on purpose. The classifier sees an error, not a network, and
// what it keys on is exactly what is built here; going through a real
// dial to obtain it buys nothing and costs the coin toss the live row
// above documents. The live row is what proves this shape is reachable in
// the first place.
func connectTimeoutShapeThatUsedToBeCancelled(t *testing.T) (context.Context, []error) {
	t.Helper()

	err := fmt.Errorf("NewFs: couldn't connect SSH: dial tcp %s:%d: %w",
		blackholedHost, blackholedPort, context.DeadlineExceeded)
	// The precondition, and it is the same one the live row samples for:
	// an error that does not carry the deadline is not this shape and
	// would prove nothing.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("this row's own error does not carry context.DeadlineExceeded, so it is not the shape #388 is about: %v", err)
	}
	// And the context has to be genuinely live, or Cancelled would be the
	// right answer and the row would be asserting the opposite of the fix.
	ctx := context.Background()
	if ctx.Err() != nil {
		t.Fatalf("the caller context is done (%v) before anything happened", ctx.Err())
	}
	return ctx, []error{err}
}

// callerCancelledBeforeTheCall is the positive control from the other side:
// a context the caller cancelled itself, which must still read as Cancelled.
func callerCancelledBeforeTheCall(t *testing.T) (context.Context, []error) {
	t.Helper()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "big.bin"), bytes.Repeat([]byte("cancel-me-"), 4096), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	ctx := alreadyCancelledContext()
	_, err := New().CopyToLocal(ctx,
		transport.Source{ID: "cls-388-cancelled", Type: "local", Root: root},
		"big.bin", filepath.Join(t.TempDir(), "big.bin.partial"))
	return ctx, []error{err}
}

// callerDeadlineFiresMidCopy is the row that separates a real fix from a
// lazy one. Dropping the context.DeadlineExceeded check and stopping there
// makes row 1 pass, and makes this row fail: a caller's own deadline
// expiring during a transfer arrives as a bare context.DeadlineExceeded and
// is a cancellation, whatever the classifier can see of the error alone.
//
// The copy is throttled rather than made huge, which is gate_test.go's
// MidTransferCancellation technique: rclone's accounting layer checks
// ctx.Err() before every chunk read (fs/accounting's checkReadBefore), so a
// slow small transfer proves an interruption as well as a fast big one, and
// the accounting group below is what proves bytes had actually started
// moving rather than the deadline having fired before the copy began.
func callerDeadlineFiresMidCopy(t *testing.T) (context.Context, []error) {
	t.Helper()

	const (
		payload  = 1 << 20 // 1 MiB
		bwLimit  = "64Ki"  // bytes/sec, so ~16s for the whole payload
		deadline = 400 * time.Millisecond
	)
	estimatedFullDuration := payload / (64 * 1024) * time.Second

	// --bwlimit is process-global in rclone, so it gets put back afterwards,
	// which is what tests/bwlimit is for: the obvious way of putting it back
	// does not work, and every test that tried it was leaking its limit into
	// the next one. The unit suffix is that package's other reason to exist,
	// and it is load-bearing here: a bare 65536 is 64Mi, which at this
	// payload throttles nothing at all (#414).
	bwlimit.Throttle(t, context.Background(), bwLimit)

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "slow.bin"), make([]byte, payload), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	group := fmt.Sprintf("cls-388-mid-copy-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(accounting.WithStatsGroup(context.Background(), group), deadline)
	t.Cleanup(cancel)

	start := time.Now()
	_, err := New().CopyToLocal(ctx,
		transport.Source{ID: "cls-388-mid-copy", Type: "local", Root: root},
		"slow.bin", filepath.Join(t.TempDir(), "slow.bin.partial"))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("a %s deadline did not interrupt a copy estimated to need ~%s; this environment is not throttled", deadline, estimatedFullDuration)
	}
	if elapsed >= estimatedFullDuration {
		t.Fatalf("the copy ran for %s, at or past the ~%s an uninterrupted one needs, so nothing was interrupted mid-flight", elapsed, estimatedFullDuration)
	}
	// The precondition that makes this "mid-copy" rather than "before the
	// copy": bytes actually moved before the deadline landed.
	if moved := accounting.StatsGroup(ctx, group).GetBytes(); moved <= 0 {
		t.Fatalf("no bytes were transferred before the deadline fired, so this is a preflight refusal, not a mid-copy interruption")
	} else if moved >= payload {
		t.Fatalf("all %d bytes moved, so the copy finished and the deadline interrupted nothing", moved)
	} else {
		t.Logf("deadline fired after %s with %d of %d bytes moved; err = %v", elapsed, moved, payload, err)
	}
	return ctx, []error{err}
}

// ---------------------------------------------------------------------------
// Unit-level classification against real rclone sentinel values, for the two
// categories (Conflict, IntegrityFailure) this adapter's current surface has
// no reachable path to provoke live. These are real values exported by
// rclone (fs.ErrorDirExists) or real, cited wording from rclone's own source
// (see integrityFailurePrefix's doc comment in errors.go), not something
// invented for the test. See the PR description for why a live scenario
// wasn't achievable within this issue's file scope.
// ---------------------------------------------------------------------------

func TestClassify_Conflict_RealRcloneSentinel(t *testing.T) {
	if got := classify(rclonefs.ErrorDirExists); got != transport.Conflict {
		t.Fatalf("classify(fs.ErrorDirExists) = %v, want Conflict", got)
	}
	// Also through a wrapping layer, the way it would actually arrive
	// through this adapter's own fmt.Errorf("...: %w", err) wrapping.
	wrapped := fmt.Errorf("copy %q: %w", "some/path", rclonefs.ErrorDirExists)
	if got := classify(wrapped); got != transport.Conflict {
		t.Fatalf("classify(wrapped fs.ErrorDirExists) = %v, want Conflict", got)
	}
}

func TestClassify_IntegrityFailure_RealRcloneWording(t *testing.T) {
	// These are the literal fmt.Errorf format strings from rclone v1.75.0's
	// fs/operations/copy.go (lines 289 and 296), reproduced with sample
	// values rather than invented text, and rclone's own manual documents
	// "corrupted on transfer" as the exact phrase this signals.
	sizeDiffer := fmt.Errorf("corrupted on transfer: sizes differ src(%s) %d vs dst(%s) %d", "srcFs", 10, "dstFs", 5)
	hashesDiffer := fmt.Errorf("corrupted on transfer: %v hashes differ src(%s) %q vs dst(%s) %q", "sha256", "srcFs", "aaa", "dstFs", "bbb")

	for _, err := range []error{sizeDiffer, hashesDiffer} {
		if got := classify(err); got != transport.IntegrityFailure {
			t.Fatalf("classify(%v) = %v, want IntegrityFailure", err, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Docker-backed classification: real authentication failure, real host-key
// failures (both shapes) and real permission/capability failures against a
// disposable SFTP server. This reuses the exact fixture image, sshd_config
// and helper functions ssh_test.go already built and proved for FR-6
// (buildSFTPFixtureImage, startFixtureContainer, generateClientSSHKeyPair,
// writeKnownHosts, freeTCPPort, requireDocker), rather than a second copy of
// them: same package, same fixture, a different set of assertions layered on
// top (classify's category, not just the raw rclone error text).
// ---------------------------------------------------------------------------

// TestClassify_Docker used to live here. It is
// core/tests/machinegate/classify_test.go now (#448): it needed a real
// sshd, a real chmod-000 file and two real host keys, which is a machine,
// and running it from this package meant `go test ./internal/...` needed a
// Docker daemon to say anything about a classifier that is otherwise pure.
//
// Everything above this line is the pure half and stays: the classification
// tables, the real-local-error cases, the rclone-imposed-timeout cases and
// the structural guard below.

// alreadyCancelledContext is a context that is already done before anything
// is asked of it, which is how the #388 precedence cases put a caller's
// cancellation up against a definite error.
func alreadyCancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// ---------------------------------------------------------------------------
// Structural guard: no exported, context-free classifier.
// ---------------------------------------------------------------------------

// TestNoExportedContextFreeClassifier is the reason issue #388 cannot come
// back through the front door.
//
// #388 was a connect timeout rclone imposed on itself being reported as
// transport.Cancelled, which reads everywhere as "the operator decided" and
// which retry.DefaultIsTransient will not retry. The fix was to stop deciding
// that from the error alone, because the error cannot say whose deadline
// expired, and to decide it from the caller's context instead. Two spellings
// could still get it wrong: the old context-free Wrap, which that fix deleted
// outright rather than keeping as an alias, and classify, which that fix left
// exported. Both were the same trap. A caller reaching for either gets an
// answer built without the one input that makes it correct, and gets it
// silently, because a Category is just an int and a wrong one looks exactly
// like a right one.
//
// So the rule this pins is a shape, not a name: anything this package exports
// that hands back a transport.Category has to take a context.Context. Pinning
// the shape rather than the name is the whole point, since renaming classify
// to ClassifyError or CategoryFor would defeat a name check while
// reintroducing the identical bug.
//
// It reads the package's own source rather than using reflection because an
// unexported function is invisible to reflect, and because the counts below
// are what stop this passing on a technicality.
func TestNoExportedContextFreeClassifier(t *testing.T) {
	const (
		transportPath = "github.com/spdrman/rclone-manager/core/internal/transport"
		contextPath   = "context"
	)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading this package's directory: %v", err)
	}

	scanned, returningCategory, exportedChecked := 0, 0, 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++

		parsed, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}

		transportName := importedAsInThisPackage(parsed, transportPath)
		if transportName == "" {
			continue
		}
		contextName := importedAsInThisPackage(parsed, contextPath)

		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !returnsCategory(fn, transportName) {
				continue
			}
			returningCategory++

			if !fn.Name.IsExported() || !receiverIsReachable(fn) {
				continue
			}
			exportedChecked++

			if contextName == "" || !takesContext(fn, contextName) {
				t.Errorf("%s exports %s, which returns a transport.Category without taking a context.Context. "+
					"A classifier that judges the error alone cannot tell a deadline the caller set from one rclone set for itself, "+
					"and answering either way is issue #388 in one direction or the other. "+
					"Take a context like ClassifyCtx does, or keep the function unexported like classify is",
					name, fn.Name.Name)
			}
		}
	}

	// Three positive controls, because every failure mode of this gate is a
	// silent zero. No files means the walk is pointed somewhere wrong; no
	// matches means returnsCategory stopped recognising the shape it exists
	// to recognise; no exported matches means the branch that actually
	// enforces anything never executed, which is the state this gate would
	// sit in forever if ClassifyCtx were ever unexported too.
	if scanned == 0 {
		t.Fatalf("found no non-test Go files in this package, so this gate would pass vacuously")
	}
	if returningCategory == 0 {
		t.Fatalf("no function in this package returns transport.Category any more, so this gate no longer recognises the shape it exists to police; fix returnsCategory or delete the gate")
	}
	if exportedChecked == 0 {
		t.Fatalf("nothing this package exports returns a transport.Category any more, so the enforcing branch never ran; ClassifyCtx was the live control, and if it is gone this gate needs pointing somewhere real")
	}
}

// importedAsInThisPackage reports the identifier file uses for importPath, or
// "" if file does not import it. It handles a renamed import, because a guard
// that only recognises the default name is a guard one alias defeats.
func importedAsInThisPackage(file *ast.File, importPath string) string {
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != importPath {
			continue
		}
		if spec.Name != nil {
			return spec.Name.Name
		}
		return importPath[strings.LastIndex(importPath, "/")+1:]
	}
	return ""
}

// returnsCategory reports whether fn hands back a transport.Category in any
// result position, named or not, alone or alongside other values.
func returnsCategory(fn *ast.FuncDecl, transportName string) bool {
	if fn.Type.Results == nil {
		return false
	}
	for _, result := range fn.Type.Results.List {
		if isSelector(result.Type, transportName, "Category") {
			return true
		}
	}
	return false
}

// takesContext reports whether fn accepts a context.Context in any parameter
// position.
func takesContext(fn *ast.FuncDecl, contextName string) bool {
	if fn.Type.Params == nil {
		return false
	}
	for _, param := range fn.Type.Params.List {
		if isSelector(param.Type, contextName, "Context") {
			return true
		}
	}
	return false
}

// receiverIsReachable reports whether an exported method on fn's receiver can
// actually be called from outside this package, which it cannot if the
// receiver type is unexported. Plain functions are always reachable.
func receiverIsReachable(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return true
	}
	expr := fn.Recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if index, ok := expr.(*ast.IndexExpr); ok { // a generic receiver, Recv[T]
		expr = index.X
	}
	ident, ok := expr.(*ast.Ident)
	return ok && ident.IsExported()
}

func isSelector(expr ast.Expr, pkg, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == pkg
}
