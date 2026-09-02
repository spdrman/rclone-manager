package mediumcontract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/transport"
)

// FilesystemStore is a transport.MediumStore over a local directory, and it
// exists so this package's contract suite has a second implementation.
//
// # Why a second implementation is not busywork
//
// A contract suite with exactly one implementation is not a contract suite,
// it is that implementation's own tests wearing a different package name.
// Every assumption the implementation makes is an assumption the suite is
// free to inherit without anyone noticing, and the first time a genuinely
// different backend arrives, half the cases turn out to have been about S3
// all along.
//
// So this one is deliberately as unlike the rclone s3 adapter as it can be
// while still satisfying the same interface: no rclone, no network, no
// object store, no shared code with the thing under test. If a case passes
// here and fails against MinIO, the difference is real and the suite has
// found it. If a case cannot be written against both, it was never a
// contract case.
//
// It also means the whole suite runs on every gate with no container
// anywhere, which is what FR-28 asks for when it says the suite runs
// "against the local backend in-tree and against a MinIO fixture in
// integration".
//
// # This is a test fixture and not a storage medium
//
// It is not registered anywhere, no configuration can name it, and
// transport.MediumTypeS3 remains the only medium type this product knows.
// A local directory is already a medium in this product's vocabulary; it is
// spelled config.MediumLocal and it is served by artifactstore.Local, which
// carries FR-20's safety proofs and this does not.
type FilesystemStore struct {
	root string

	// attest decides which half of the FR-31 ladder this store claims. It
	// is a knob rather than a fixed answer specifically so the contract
	// suite exercises BOTH branches of its checksum case: the real S3
	// adapter can only ever demonstrate the refusal, so without this the
	// attesting branch would be code nothing runs.
	attest bool
}

// NewFilesystemStore returns a store keeping objects under root.
func NewFilesystemStore(root string, attest bool) *FilesystemStore {
	return &FilesystemStore{root: root, attest: attest}
}

// AttestsChecksums reports what this store claims, so a Fixtures built
// around it can answer the suite honestly.
func (s *FilesystemStore) AttestsChecksums() bool { return s.attest }

var _ transport.MediumStore = (*FilesystemStore)(nil)

// pathFor maps a medium and a key onto a local path.
//
// The key is validated rather than trusted, even here. A key arrives as a
// "/"-joined string and turns into a filesystem path, which is exactly the
// conversion transport.MediumKey refuses to produce a traversing key for;
// re-checking on the way back is the same both-ends discipline sftpConfig
// applies to a Source.
func (s *FilesystemStore) pathFor(medium transport.Medium, key string) (string, error) {
	if medium.ID == "" {
		return "", errors.New("mediumcontract: a medium needs an id")
	}
	if key == "" {
		return "", errors.New("mediumcontract: an empty key names no object")
	}
	for _, segment := range strings.Split(key, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", fmt.Errorf("mediumcontract: key %q has a segment this store will not turn into a path", key)
		}
	}
	return filepath.Join(s.root, medium.ID, filepath.FromSlash(key)), nil
}

// wrap turns a local error into the manager-owned vocabulary. The contract
// suite asserts categories, so a store that returned bare errors would fail
// every case for the wrong reason.
func (s *FilesystemStore) wrap(op string, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return transport.NewError(transport.Cancelled, op, err)
	case errors.Is(err, os.ErrNotExist):
		return transport.NewError(transport.NotFound, op, err)
	case errors.Is(err, os.ErrExist):
		return transport.NewError(transport.Conflict, op, err)
	case errors.Is(err, os.ErrPermission):
		return transport.NewError(transport.PermissionDenied, op, err)
	default:
		return transport.NewError(transport.Permanent, op, err)
	}
}

func (s *FilesystemStore) StatObject(ctx context.Context, medium transport.Medium, key string) (transport.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return transport.ObjectInfo{}, s.wrap("stat_object", err)
	}
	p, err := s.pathFor(medium, key)
	if err != nil {
		return transport.ObjectInfo{}, s.wrap("stat_object", err)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return transport.ObjectInfo{}, s.wrap("stat_object", err)
	}
	if !fi.Mode().IsRegular() {
		return transport.ObjectInfo{}, s.wrap("stat_object", fmt.Errorf("%s is not a regular file (mode %s)", p, fi.Mode()))
	}
	return transport.ObjectInfo{
		Key:          key,
		Size:         fi.Size(),
		ModTime:      fi.ModTime().Unix(),
		StorageClass: medium.StorageClass,
	}, nil
}

// UploadFromLocal discharges the interface's three obligations the way
// artifactstore.Local.Put discharges the identical three, because on a
// filesystem there is only one right answer and it is already written down
// there: a temp file for atomicity, an fsync for durability, a hard link
// rather than a rename so an occupied key is REFUSED instead of clobbered,
// and a directory fsync so the name that now exists survives a power loss
// rather than merely a process exit.
func (s *FilesystemStore) UploadFromLocal(ctx context.Context, medium transport.Medium, localPath, key string) (transport.UploadResult, error) {
	if err := ctx.Err(); err != nil {
		return transport.UploadResult{}, s.wrap("upload_from_local", err)
	}
	p, err := s.pathFor(medium, key)
	if err != nil {
		return transport.UploadResult{}, s.wrap("upload_from_local", err)
	}
	src, err := os.Open(localPath)
	if err != nil {
		return transport.UploadResult{}, s.wrap("upload_from_local", err)
	}
	defer src.Close()

	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return transport.UploadResult{}, s.wrap("upload_from_local", err)
	}
	tmp, err := os.CreateTemp(dir, ".mediumcontract-*")
	if err != nil {
		return transport.UploadResult{}, s.wrap("upload_from_local", err)
	}
	tmpName := tmp.Name()
	linked := false
	defer func() {
		tmp.Close()
		if !linked {
			os.Remove(tmpName)
		}
	}()

	written, err := io.Copy(tmp, src)
	if err != nil {
		return transport.UploadResult{}, s.wrap("upload_from_local", err)
	}
	if err := tmp.Sync(); err != nil {
		return transport.UploadResult{}, s.wrap("upload_from_local", err)
	}
	if err := tmp.Close(); err != nil {
		return transport.UploadResult{}, s.wrap("upload_from_local", err)
	}
	if err := os.Link(tmpName, p); err != nil {
		return transport.UploadResult{}, s.wrap("upload_from_local", err)
	}
	linked = true
	if err := os.Remove(tmpName); err != nil {
		return transport.UploadResult{}, s.wrap("upload_from_local", err)
	}
	if err := fsyncDir(dir); err != nil {
		return transport.UploadResult{}, s.wrap("upload_from_local", err)
	}

	// Read back off the stored object, for the reason the interface gives:
	// a count of what was sent is not a fact about what was stored.
	fi, err := os.Stat(p)
	if err != nil {
		return transport.UploadResult{}, s.wrap("upload_from_local", err)
	}
	if fi.Size() != written {
		return transport.UploadResult{}, s.wrap("upload_from_local", fmt.Errorf("stored %d bytes but %d were written", fi.Size(), written))
	}
	return transport.UploadResult{BytesUploaded: fi.Size(), StorageClass: medium.StorageClass}, nil
}

func (s *FilesystemStore) OpenObject(ctx context.Context, medium transport.Medium, key string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, s.wrap("open_object", err)
	}
	p, err := s.pathFor(medium, key)
	if err != nil {
		return nil, s.wrap("open_object", err)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, s.wrap("open_object", err)
	}
	return f, nil
}

// ObjectChecksum computes a real SHA-256 over the stored bytes when this
// store claims to attest, and returns an explicit capability refusal when
// it does not.
//
// Computing it by reading the whole object would be dishonest for a real
// medium: FR-31's `attested` class is "one metadata call, no egress", and
// an implementation that quietly downloaded the object to answer would be
// charging read-back's price while reporting attestation's class. It is
// fine HERE only because this store is a local directory where there is no
// egress and no metadata call to distinguish, and because the point is to
// give the contract suite's attesting branch something to run against.
func (s *FilesystemStore) ObjectChecksum(ctx context.Context, medium transport.Medium, key string, alg transport.HashAlgorithm) (transport.ChecksumAttestation, error) {
	if err := ctx.Err(); err != nil {
		return transport.ChecksumAttestation{}, s.wrap("object_checksum", err)
	}
	if alg != transport.SHA256 {
		return transport.ChecksumAttestation{}, transport.NewError(transport.UnsupportedCapability, "object_checksum",
			fmt.Errorf("mediumcontract: this store attests %s and not %q", transport.SHA256, alg))
	}
	if !s.attest {
		return transport.ChecksumAttestation{}, transport.NewError(transport.UnsupportedCapability, "object_checksum",
			fmt.Errorf("mediumcontract: this store is configured not to attest checksums"))
	}
	p, err := s.pathFor(medium, key)
	if err != nil {
		return transport.ChecksumAttestation{}, s.wrap("object_checksum", err)
	}
	f, err := os.Open(p)
	if err != nil {
		return transport.ChecksumAttestation{}, s.wrap("object_checksum", err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return transport.ChecksumAttestation{}, s.wrap("object_checksum", err)
	}
	return transport.ChecksumAttestation{Algorithm: transport.SHA256, Value: hex.EncodeToString(h.Sum(nil))}, nil
}

func (s *FilesystemStore) DeleteObject(ctx context.Context, medium transport.Medium, key string) error {
	if err := ctx.Err(); err != nil {
		return s.wrap("delete_object", err)
	}
	p, err := s.pathFor(medium, key)
	if err != nil {
		return s.wrap("delete_object", err)
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return s.wrap("delete_object", err)
	}
	return nil
}

func (s *FilesystemStore) ListObjects(ctx context.Context, medium transport.Medium, prefix string) ([]transport.ObjectInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, s.wrap("list_objects", err)
	}
	base := filepath.Join(s.root, medium.ID)
	start := base
	if prefix != "" {
		start = filepath.Join(base, filepath.FromSlash(prefix))
	}

	var out []transport.ObjectInfo
	err := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A prefix nothing has been written under is an empty answer,
			// not a missing thing: on an object store there are no
			// directories to be missing, and a first upload to a new
			// backup set would otherwise look like a broken medium.
			if errors.Is(err, os.ErrNotExist) {
				return filepath.SkipAll
			}
			return err
		}
		if d.IsDir() || strings.HasPrefix(d.Name(), ".mediumcontract-") {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		rel, rerr := filepath.Rel(base, p)
		if rerr != nil {
			return rerr
		}
		out = append(out, transport.ObjectInfo{
			Key:          path.Clean(filepath.ToSlash(rel)),
			Size:         info.Size(),
			ModTime:      info.ModTime().Unix(),
			StorageClass: medium.StorageClass,
		})
		return nil
	})
	if err != nil {
		return nil, s.wrap("list_objects", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// fsyncDir is the durability step Local.Put's own doc calls "the step
// people skip": a directory is a separate inode from the file it names,
// with its own writeback state, so fsyncing the content says nothing about
// whether the NAME survives a power loss.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}
