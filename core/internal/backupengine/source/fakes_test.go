package source_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/backupdproject/backupd/core/internal/backend"
	"github.com/backupdproject/backupd/core/internal/backupengine/source"
	"github.com/backupdproject/backupd/core/internal/transport"
)

// The stand-ins in this file exist for the cases a real filesystem cannot
// produce on demand: a source that mutates a file at a chosen byte offset
// of its own read, a backend that declares a capability nothing this
// build ships declares, and a sink that fails a discard. Everything that
// CAN be proven against real rclone and a real repository is, in
// integration_test.go; these are for the rest.

// fakeObject is one object on a fake source.
type fakeObject struct {
	data    []byte
	modTime int64
	id      string
	kind    transport.EntryKind
	link    string
	openErr error
}

// fakeSource is a source that can be made to move under a reader.
//
// It has no delete, no write and no rename, which is not an omission: the
// adapter is handed this and nothing else, so "a backup never deletes
// from the source" is a property of what the adapter can reach rather
// than a rule it is trusted to follow.
type fakeSource struct {
	mu      sync.Mutex
	objects map[string]*fakeObject

	opens  atomic.Int64
	stats  atomic.Int64
	inturn atomic.Int64
	peak   atomic.Int64

	// duringRead runs once per read, after n bytes of that read have been
	// delivered, holding the source's lock. It is how a mutation is
	// ordered against a read rather than raced against one.
	duringReadAfter int
	duringRead      func(s *fakeSource)
}

func newFakeSource() *fakeSource {
	return &fakeSource{objects: map[string]*fakeObject{}}
}

func (f *fakeSource) put(path string, data []byte, modTime int64) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.objects[path] = &fakeObject{data: data, modTime: modTime, kind: transport.EntryKindRegular}
}

func (f *fakeSource) putKind(path string, kind transport.EntryKind, link string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.objects[path] = &fakeObject{kind: kind, link: link, modTime: 1000}
}

func (f *fakeSource) get(path string) (*fakeObject, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	o, ok := f.objects[path]

	return o, ok
}

func (f *fakeSource) artifact(path string, o *fakeObject) transport.RemoteArtifact {
	return transport.RemoteArtifact{
		Path:    path,
		Size:    int64(len(o.data)),
		ModTime: o.modTime,
		ID:      o.id,
		Kind:    o.kind,
	}
}

func (f *fakeSource) OpenSourceStream(ctx context.Context, _ transport.Source, remotePath string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	o, ok := f.get(remotePath)
	if !ok {
		return nil, transport.NewError(transport.NotFound, "open_source_stream", errors.New("object not found"))
	}
	if o.openErr != nil {
		return nil, o.openErr
	}

	f.opens.Add(1)

	live := f.inturn.Add(1)
	for {
		peak := f.peak.Load()
		if live <= peak || f.peak.CompareAndSwap(peak, live) {
			break
		}
	}

	f.mu.Lock()
	data := o.data
	f.mu.Unlock()

	return &fakeReader{src: f, data: data}, nil
}

func (f *fakeSource) StatSource(ctx context.Context, _ transport.Source, remotePath string) (transport.RemoteArtifact, error) {
	if err := ctx.Err(); err != nil {
		return transport.RemoteArtifact{}, err
	}

	f.stats.Add(1)

	o, ok := f.get(remotePath)
	if !ok {
		return transport.RemoteArtifact{}, transport.NewError(transport.NotFound, "stat", errors.New("object not found"))
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	return f.artifact(remotePath, o), nil
}

func (f *fakeSource) ReadSourceLink(_ context.Context, _ transport.Source, remotePath string) (string, error) {
	o, ok := f.get(remotePath)
	if !ok {
		return "", transport.NewError(transport.NotFound, "read_link", errors.New("object not found"))
	}

	return o.link, nil
}

// Enumerate yields every object in sorted order, which a real enumerator
// does not promise and this one does only so a test's expectations can be
// written down.
func (f *fakeSource) Enumerate(ctx context.Context, _ transport.Source, opts transport.EnumerateOptions, yield func(transport.RemoteArtifact) error) error {
	f.mu.Lock()
	paths := make([]string, 0, len(f.objects))
	for p := range f.objects {
		paths = append(paths, p)
	}
	f.mu.Unlock()
	sort.Strings(paths)

	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}

		o, ok := f.get(p)
		if !ok {
			continue
		}
		if o.kind != transport.EntryKindRegular && !opts.ReportNonRegular {
			continue
		}

		f.mu.Lock()
		art := f.artifact(p, o)
		f.mu.Unlock()

		if err := yield(art); err != nil {
			return err
		}
	}

	return nil
}

// fakeReader delivers an object's bytes and gives a test a place to stand
// in the middle of the read.
type fakeReader struct {
	src    *fakeSource
	data   []byte
	at     int
	closed bool
	fired  bool
}

func (r *fakeReader) Read(p []byte) (int, error) {
	if r.at >= len(r.data) {
		return 0, io.EOF
	}

	n := copy(p, r.data[r.at:])
	r.at += n

	if !r.fired && r.src.duringRead != nil && r.at >= r.src.duringReadAfter {
		r.fired = true
		r.src.duringRead(r.src)
	}

	return n, nil
}

func (r *fakeReader) Close() error {
	if !r.closed {
		r.closed = true
		r.src.inturn.Add(-1)
	}

	return nil
}

// recordingSink stores what it is given in memory and records everything
// that happened to it.
type recordingSink struct {
	mu       sync.Mutex
	stored   map[string][]byte
	live     map[string]string // id -> path
	discards []string
	next     int

	storeErr    func(path string, attempt int) error
	discardErr  error
	underreport bool

	peak   atomic.Int64
	inturn atomic.Int64
}

func newRecordingSink() *recordingSink {
	return &recordingSink{stored: map[string][]byte{}, live: map[string]string{}}
}

func (s *recordingSink) Store(ctx context.Context, obj source.Object) (source.Stored, error) {
	live := s.inturn.Add(1)
	defer s.inturn.Add(-1)

	for {
		peak := s.peak.Load()
		if live <= peak || s.peak.CompareAndSwap(peak, live) {
			break
		}
	}

	if s.storeErr != nil {
		if err := s.storeErr(obj.Path, obj.Attempt); err != nil {
			return source.Stored{}, err
		}
	}

	rc, err := obj.Stream.Open(ctx)
	if err != nil {
		return source.Stored{}, err
	}
	defer rc.Close() //nolint:errcheck // the adapter closes it too; this is the ordinary owner.

	buf := make([]byte, 32<<10)

	var body []byte

	var n int64

	for {
		read, err := rc.Read(buf)
		if read > 0 {
			n += int64(read)
			body = append(body, buf[:read]...)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return source.Stored{}, err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.next++
	id := fmt.Sprintf("snap-%d", s.next)
	s.stored[obj.Path] = body
	s.live[id] = obj.Path

	reported := n
	if s.underreport {
		reported = n - 1
	}

	return source.Stored{ID: id, Bytes: reported, UploadedBytes: n}, nil
}

func (s *recordingSink) Discard(_ context.Context, id string) error {
	if s.discardErr != nil {
		return s.discardErr
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.discards = append(s.discards, id)
	if p, ok := s.live[id]; ok {
		delete(s.live, id)
		delete(s.stored, p)
	}

	return nil
}

func (s *recordingSink) content(path string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, ok := s.stored[path]

	return b, ok
}

func (s *recordingSink) paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]string, 0, len(s.stored))
	for p := range s.stored {
		out = append(out, p)
	}
	sort.Strings(out)

	return out
}

func (s *recordingSink) discardCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.discards)
}

// countingSink reads a stream to its end with a fixed buffer and keeps
// none of it. It is what a bounded-memory measurement needs: a sink that
// consumes the whole object without being the thing that allocates.
type countingSink struct {
	bytes atomic.Int64
	next  atomic.Int64
}

func (s *countingSink) Store(ctx context.Context, obj source.Object) (source.Stored, error) {
	rc, err := obj.Stream.Open(ctx)
	if err != nil {
		return source.Stored{}, err
	}
	defer rc.Close() //nolint:errcheck // nothing to report from a discarded read.

	buf := make([]byte, 1<<20)

	var n int64

	for {
		read, err := rc.Read(buf)
		n += int64(read)

		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return source.Stored{}, err
		}
	}

	return source.Stored{ID: fmt.Sprint(s.next.Add(1)), Bytes: n, UploadedBytes: n}, nil
}

func (s *countingSink) Discard(context.Context, string) error { return nil }

// capabilityProfile builds a profile for a backend this build does not
// ship, so the refusals for one can be exercised.
func capabilityProfile(id string, caps backend.Capabilities) func(string) (source.Profile, error) {
	return func(string) (source.Profile, error) {
		return source.NewProfile(id, caps), nil
	}
}

// localLikeCapabilities is bundled/local_volume.json's matrix, restated
// here so a test can vary one key at a time without editing a shipped
// manifest.
func localLikeCapabilities() backend.Capabilities {
	return backend.Capabilities{
		BoundedListing:     true,
		RecursiveListing:   false,
		StreamingOpen:      true,
		RangeOpen:          true,
		MTimePrecision:     backend.MTimeSecond,
		HashSupport:        nil,
		StableSize:         true,
		SymlinkSemantics:   backend.SymlinksSkipped,
		MetadataSupport:    backend.MetadataFull,
		CaseSensitivity:    backend.CaseUnknown,
		CasePreservation:   backend.CasePreservationUnknown,
		GenerationIdentity: backend.GenerationNone,
	}
}

// patternBytes is a deterministic filler that does not compress to
// nothing, so a size assertion measures something real.
func patternBytes(n int, seed uint32) []byte {
	out := make([]byte, n)
	x := seed | 1

	for i := range out {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		out[i] = byte(x)
	}

	return out
}

func reasonsContain(reasons []string, substr string) bool {
	for _, r := range reasons {
		if strings.Contains(r, substr) {
			return true
		}
	}

	return false
}

func fakeTransportSource() transport.Source {
	return transport.Source{ID: "fake", Type: "local", Root: "/does/not/matter"}
}

var _ = time.Second
