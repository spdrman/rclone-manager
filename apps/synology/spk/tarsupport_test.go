package spk

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"testing"
)

// tarEntry is one member of an archive, read fully into memory. Every
// archive this package builds is small (two binaries and a handful of
// text files), so the tests rewrite them whole rather than streaming.
type tarEntry struct {
	hdr  tar.Header
	body []byte
}

func readTar(t *testing.T, r io.Reader) []tarEntry {
	t.Helper()
	var out []tarEntry
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read tar body %s: %v", hdr.Name, err)
		}
		out = append(out, tarEntry{hdr: *hdr, body: body})
	}
}

func writeTar(t *testing.T, w io.Writer, entries []tarEntry) {
	t.Helper()
	tw := tar.NewWriter(w)
	for _, e := range entries {
		hdr := e.hdr
		hdr.Size = int64(len(e.body))
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatalf("write header %s: %v", hdr.Name, err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatalf("write body %s: %v", hdr.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
}

// mutateSPK reads the outer .spk tar, hands its members to fn, and writes
// whatever fn returns back to a NEW path, leaving the original intact so
// one fixture can seed several controls.
func mutateSPK(t *testing.T, path string, fn func([]tarEntry) []tarEntry) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	entries := readTar(t, f)
	_ = f.Close()

	out := t.TempDir() + "/mutated.spk"
	w, err := os.Create(out)
	if err != nil {
		t.Fatalf("create %s: %v", out, err)
	}
	defer func() { _ = w.Close() }()
	writeTar(t, w, fn(entries))
	return out
}

// mutateInnerPayload is mutateSPK's counterpart for the members inside
// package.tgz, which is where the core binaries and the DSM UI files live.
func mutateInnerPayload(t *testing.T, path string, fn func([]tarEntry) []tarEntry) string {
	t.Helper()
	return mutateSPK(t, path, func(outer []tarEntry) []tarEntry {
		for i, e := range outer {
			if e.hdr.Name != PayloadName {
				continue
			}
			zr, err := gzip.NewReader(bytes.NewReader(e.body))
			if err != nil {
				t.Fatalf("open %s: %v", PayloadName, err)
			}
			inner := readTar(t, zr)
			_ = zr.Close()

			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			writeTar(t, zw, fn(inner))
			if err := zw.Close(); err != nil {
				t.Fatalf("close gzip: %v", err)
			}
			outer[i].body = buf.Bytes()
			return outer
		}
		t.Fatalf("%s not found in the package", PayloadName)
		return outer
	})
}

func dropEntry(entries []tarEntry, name string) []tarEntry {
	out := entries[:0:0]
	for _, e := range entries {
		if e.hdr.Name != name {
			out = append(out, e)
		}
	}
	return out
}

func addEntry(entries []tarEntry, name string, body []byte) []tarEntry {
	return append(entries, tarEntry{
		hdr:  tar.Header{Name: name, Mode: 0o644, Typeflag: tar.TypeReg},
		body: body,
	})
}

func replaceBody(entries []tarEntry, name string, body []byte) []tarEntry {
	for i := range entries {
		if entries[i].hdr.Name == name {
			entries[i].body = body
		}
	}
	return entries
}

// gzipFile writes a gzip-compressed copy of path and returns the new
// path. Used to prove the verifier notices an outer archive that is not
// the plain `tar cf` output pkg_make_spk produces.
func gzipFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	out := t.TempDir() + "/compressed.spk"
	if err := os.WriteFile(out, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", out, err)
	}
	return out
}

func infoBody(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	for _, e := range entries {
		if e.hdr.Name == INFOName {
			return e.body
		}
	}
	t.Fatalf("%s not found in the package", INFOName)
	return nil
}
