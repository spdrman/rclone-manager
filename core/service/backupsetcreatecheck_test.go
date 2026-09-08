// Two review findings on the create half of #624, both about one
// sentence in that feature's own description: a set written unproven is
// marked (PR #628 review).
//
// The first shape of the feature ran the candidate check in the CLI and
// gated the wizard's Save on it, and carried the MARK on the create
// request as a statement about what the caller had done. The service
// wrote whatever it was told. So POST /api/v1/backup-sets and POST
// /system/first-run both accepted an unproven, unmarked set and answered
// 201, while PATCH ran the check server-side and refused: one --no-verify
// flag mapped to two request fields with opposite meanings, and any
// client that was not this repository's own CLI or wizard could omit the
// field and produce a set indistinguishable on every screen from one
// checked against a real server, which is verbatim the state the mark
// exists to end.
//
// Both create paths now run the check themselves, refuse on a failure,
// and write the mark only when told to skip. These cases are that, asked
// of the service, because every surface writes through here.
//
// The host in every failing case is a loopback port nothing listens on,
// for the reason the CLI's own cases give: a closed port fails on a TCP
// refusal in microseconds where a name that does not resolve spends the
// resolver's timeout, and on a machine with a wildcard-answering resolver
// is not even a failure. A red that depends on the developer's resolver
// is a red that goes green for the wrong reason.
package service

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// aClosedLoopbackPort binds a port on 127.0.0.1, reads the number and
// closes it again, so the dial in the check is refused rather than
// answered. Racy in principle and not in practice; every way it can be
// wrong still fails the dial, which is all a case here needs.
func aClosedLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("closing the probe listener: %v", err)
	}
	return port
}

// unreachableCreateReq is validCreateReq pointed at a port nothing
// answers on, with the skip that fixture carries turned back off, so the
// check in front of the write really runs and really fails.
func unreachableCreateReq(t *testing.T, svc *BackupService, name string) CreateBackupSetRequest {
	t.Helper()
	req := validCreateReq(t, svc, name)
	req.SkipConnectionCheck = false
	req.Host = "127.0.0.1"
	req.Port = aClosedLoopbackPort(t)
	return req
}

// trustAnchorsIn lists what the deployment's known_hosts.d holds, or
// nothing when the directory has never been created.
func trustAnchorsIn(t *testing.T, configPath string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(configPath), "known_hosts.d"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("ReadDir(known_hosts.d): %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestCreateBackupSet_ProvesTheConnectionBeforeItWrites is the finding
// itself: a create that names a source this manager cannot reach is
// refused by the service, whoever the caller is, and leaves nothing
// behind.
//
// Three things have to be true of the refusal and the test asks all
// three. The error is ErrConnectionNotProven, so the API answers the
// same 409 it already answers for an unprovable edit. The configuration
// file is byte for byte what it was, which is the same promise every
// other refusal on this path makes. And known_hosts.d gained nothing,
// because the check runs before newBackupSetFor writes the set's trust
// anchor; a refusal that left a trusted line on disk for a set that does
// not exist would be a refusal with a side effect.
func TestCreateBackupSet_ProvesTheConnectionBeforeItWrites(t *testing.T) {
	svc, configPath := openTestService(t)
	before := mustRead(t, configPath)
	anchorsBefore := trustAnchorsIn(t, configPath)

	req := unreachableCreateReq(t, svc, "unreachable")
	_, err := svc.CreateBackupSet(context.Background(), req)
	if !errors.Is(err, ErrConnectionNotProven) {
		t.Fatalf("CreateBackupSet against a port nothing answers on returned %v, want ErrConnectionNotProven: the service wrote whatever it was told, and the mark was the caller's to set or omit", err)
	}

	if after := mustRead(t, configPath); after != before {
		t.Errorf("a refused create still wrote the configuration:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if _, err := svc.GetBackupSet(context.Background(), "api/unreachable"); !errors.Is(err, ErrBackupSetNotFound) {
		t.Errorf("GetBackupSet after a refused create = %v, want ErrBackupSetNotFound: the set exists in this process even though the file was not written", err)
	}
	if got := trustAnchorsIn(t, configPath); strings.Join(got, ",") != strings.Join(anchorsBefore, ",") {
		t.Errorf("a refused create left a trust anchor behind: known_hosts.d = %v, was %v", got, anchorsBefore)
	}
}

// TestCreateBackupSet_SkippingTheCheckMarksTheSet is the escape hatch,
// and the mark is the service's own record of using it.
//
// The host is one the check would refuse, so the skip is what does the
// work: a build that ran the check regardless would refuse here, and a
// build that wrote the mark from something the caller said would need a
// field this request no longer has.
//
// The control is on newBackupSetFor rather than on a second create,
// because a create that PASSES its check needs an SSH server answering
// sftp, which this suite does not have in-process. What can be shown is
// that the mark comes from the skip and nothing else: the same request
// with the skip off produces a set with no mark, and the encoder leaves
// the key out of the file entirely rather than writing false, so absence
// keeps meaning what it meant in every configuration written before this
// field existed.
func TestCreateBackupSet_SkippingTheCheckMarksTheSet(t *testing.T) {
	svc, configPath := openTestService(t)

	req := unreachableCreateReq(t, svc, "offline-set")
	req.SkipConnectionCheck = true
	if _, err := svc.CreateBackupSet(context.Background(), req); err != nil {
		t.Fatalf("CreateBackupSet with the check skipped: %v", err)
	}
	got, err := svc.GetBackupSet(context.Background(), "api/offline-set")
	if err != nil {
		t.Fatalf("GetBackupSet: %v", err)
	}
	if !got.ConnectionUnverified {
		t.Error("a set written with the check skipped reads back as one that was checked")
	}
	if raw := mustRead(t, configPath); !strings.Contains(raw, "connection_unverified: true") {
		t.Errorf("the mark did not reach the configuration file:\n%s", raw)
	}

	proven := req
	proven.SkipConnectionCheck = false
	keyFile, err := resolveSSHKeyFileIn(configPath, proven.SSHKeyID)
	if err != nil {
		t.Fatalf("resolveSSHKeyFileIn: %v", err)
	}
	set, err := newBackupSetFor(configPath, "api", keyFile, proven)
	if err != nil {
		t.Fatalf("newBackupSetFor: %v", err)
	}
	if set.ConnectionUnverified {
		t.Error("a create that did not skip the check is marked as unverified; the mark has to come from the skip and from nothing else")
	}
	encoded, err := yaml.Marshal(set)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	if strings.Contains(string(encoded), "connection_unverified") {
		t.Errorf("an ordinary create writes the key; absence has to keep meaning what it meant in every file written before this field existed:\n%s", encoded)
	}
}

// TestFirstRun_CreateInitialConfigProvesTheConnection is the same finding
// on the other create path. A fresh install's first set goes through
// CreateInitialConfig rather than CreateBackupSet, and POST
// /system/first-run took the same caller-asserted mark, so the setup
// wizard's own gate was the only thing between an unproven first set and
// a 201.
//
// The refusal here has a sharper consequence than the configured path's:
// there is no configuration at all before this call, and a first
// configuration written and then reported as failed is the shape a retry
// silently folds into. So the case asserts the file does not exist
// afterwards, not merely that it is unchanged.
func TestFirstRun_CreateInitialConfigProvesTheConnection(t *testing.T) {
	t.Run("refused and nothing written", func(t *testing.T) {
		fr, configPath, _ := newTestFirstRun(t)
		req := firstRunCreateReq(t, fr, "postgres")
		req.SkipConnectionCheck = false
		req.Host = "127.0.0.1"
		req.Port = aClosedLoopbackPort(t)

		_, err := fr.CreateInitialConfig(context.Background(), req)
		if !errors.Is(err, ErrConnectionNotProven) {
			t.Fatalf("CreateInitialConfig against a port nothing answers on returned %v, want ErrConnectionNotProven", err)
		}
		if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a refused first create left a configuration at %s (stat err = %v), so a retry would fold into a file nobody meant to write", configPath, err)
		}
		if fr.Configured() {
			t.Error("the first-run surface reports itself configured after a refusal")
		}
	})

	t.Run("skipped and marked", func(t *testing.T) {
		fr, configPath, _ := newTestFirstRun(t)
		req := firstRunCreateReq(t, fr, "postgres")
		req.SkipConnectionCheck = true
		req.Host = "127.0.0.1"
		req.Port = aClosedLoopbackPort(t)

		set, err := fr.CreateInitialConfig(context.Background(), req)
		if err != nil {
			t.Fatalf("CreateInitialConfig with the check skipped: %v", err)
		}
		if !set.ConnectionUnverified {
			t.Error("the first set, written with the check skipped, reads back as one that was checked")
		}
		if raw := mustRead(t, configPath); !strings.Contains(raw, "connection_unverified: true") {
			t.Errorf("the mark did not reach the first configuration:\n%s", raw)
		}
	})
}
