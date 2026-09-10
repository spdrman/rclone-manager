package cliecho

import (
	"net/url"
	"strings"
	"testing"

	"github.com/spdrman/rclone-manager/core/apicontract"
)

// A gap is a promise that a verb does not exist, and this tree ships five
// verbs that gap entries said it did not: `activity`, `activity --follow`,
// `retention apply`, `backup-set edit-hold` and `settings patch
// --policy-file`. Each of those routes printed "there is no verb that..."
// at an operator who could have run one.
//
// The mechanical guard against a gap outliving the thing it describes is
// in core/cmd/backup-manager, which is the only package that can see the
// verb tables. This is the other half: the commands themselves, pinned
// here where the builders are.
func TestTheRoutesWhoseVerbsNowExistNameThem(t *testing.T) {
	for _, tc := range []struct {
		name   string
		action Action
		want   string
	}{
		{
			name:   "the durable journal",
			action: Action{Method: "GET", Route: "/activity"},
			want:   Binary + " activity",
		},
		{
			name:   "the durable journal, with a limit",
			action: Action{Method: "GET", Route: "/activity", Query: mustQuery("limit=50")},
			want:   Binary + " activity --limit 50",
		},
		{
			name:   "the live feed",
			action: Action{Method: "GET", Route: "/activity/live"},
			want:   Binary + " activity --follow",
		},
		{
			name:   "the live feed for one set",
			action: Action{Method: "GET", Route: "/activity/live", Query: mustQuery("backup_set=api-server%2Fvar-backups&limit=25")},
			want:   Binary + " activity --follow --backup-set api-server/var-backups --limit 25",
		},
		{
			name: "applying a retention plan",
			action: Action{Method: "POST", Route: "/backup-sets/{source}/{set}/retention/apply",
				Params: map[string]string{"source": "api-server", "set": "var-backups"},
				Body:   []byte(`{"plan_id":"plan_01HX"}`)},
			want: Binary + " retention apply api-server/var-backups --acknowledge",
		},
		{
			name: "reading an edit hold",
			action: Action{Method: "GET", Route: "/backup-sets/{source}/{set}/edit-hold",
				Params: map[string]string{"source": "api-server", "set": "var-backups"}},
			want: Binary + " backup-set edit-hold api-server/var-backups",
		},
		{
			name: "releasing an edit hold",
			action: Action{Method: "POST", Route: "/backup-sets/{source}/{set}/edit-hold/release",
				Params: map[string]string{"source": "api-server", "set": "var-backups"}},
			want: Binary + " backup-set edit-hold api-server/var-backups --release",
		},
		{
			// The scalars this request also carried are in the block
			// rather than beside the flag, because `settings patch`
			// refuses --policy-file next to --timezone: the file carries
			// the whole retention section.
			name: "replacing the deployment's tier chain",
			action: Action{Method: "PATCH", Route: "/settings",
				Body: []byte(`{"retention":{"tiers":[{"name":"daily","granularity":"day","keep":7}],"timezone":"Europe/Berlin"},"acknowledge_medium_disclosure":true}`)},
			want: Binary + " settings patch --policy-file <a file holding this retention: block> --acknowledge-medium-disclosure",
		},
		{
			name: "a tier chain beside a capacity setting, which may share a line",
			action: Action{Method: "PATCH", Route: "/settings",
				Body: []byte(`{"retention":{"tiers":[{"name":"daily","granularity":"day","keep":7}]},"capacity":{"cap_bytes":1099511627776}}`)},
			want: Binary + " settings patch --policy-file <a file holding this retention: block> --cap-bytes 1099511627776",
		},
		{
			name: "a backup set's own tier chain",
			action: Action{Method: "PUT", Route: "/backup-sets/{source}/{set}/retention",
				Params: map[string]string{"source": "api-server", "set": "var-backups"},
				Body:   []byte(`{"tiers":[{"name":"daily","granularity":"day","keep":7}],"timezone":"Europe/Berlin","acknowledge_medium_disclosure":true}`)},
			want: Binary + " backup-set retention api-server/var-backups --policy-file <a file holding this whole retention: block> --acknowledge-medium-disclosure",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := Echo(tc.action)
			if line.Gap != "" {
				t.Fatalf("this route still says there is no equivalent: %s\n  %s", line.Gap, line.GapDetail)
			}
			if got := line.Shell(); got != tc.want {
				t.Errorf("prints\n  %s\nwant\n  %s", got, tc.want)
			}
		})
	}

	// Taking a hold is the one gap on this surface that is still true, and
	// usage() says so in as many words: "There is no verb that TAKES a
	// hold". A fix that turned every neighbouring gap into a command and
	// took this one with it would be inventing a verb.
	take := Echo(Action{Method: "POST", Route: "/backup-sets/{source}/{set}/edit-hold",
		Params: map[string]string{"source": "api-server", "set": "var-backups"}})
	if len(take.Command) != 0 {
		t.Errorf("taking an edit hold printed the command %v, and no verb takes one", take.Command)
	}
}

// The /operations builder switched on `action == "restore"`, which is not
// one of the three values the contract defines and not one any client has
// ever sent. So no real request matched that arm: every restore and every
// per-set run fell through to a default whose sentence is about
// `rbm run`, a different verb for a different act. The
// example body said "restore" too, so the end-to-end parse test certified
// a branch production never reaches.
//
// This drives the contract's own constants, which is also what the
// builder now switches on.
func TestOperationsReadsTheActionsTheContractDefines(t *testing.T) {
	restore := Echo(Action{Method: "POST", Route: "/operations",
		Body: []byte(`{"action":"` + apicontract.ActionRestorePlacement + `","config_revision":"r1","restore":{"artifact_id":"api-server/var-backups/dump.tar","medium":"offsite_s3","window_days":7,"acknowledged":true}}`)})
	if got, want := restore.Shell(), Binary+" restore api-server/var-backups/dump.tar --medium offsite_s3 --days 7 --acknowledge"; got != want {
		t.Errorf("a %s prints\n  %s\nwant\n  %s", apicontract.ActionRestorePlacement, got, want)
	}

	// A per-set run is its own action and gets its own sentence. Whatever
	// that sentence says, it must not be run_cycle's: those two are
	// different acts, and a gap that describes the wrong one sends an
	// operator to the wrong verb.
	perSet := Echo(Action{Method: "POST", Route: "/operations",
		Body: []byte(`{"action":"` + apicontract.ActionRunBackupSet + `","config_revision":"r1","backup_set_id":"api-server/var-backups"}`)})
	cycle := Echo(Action{Method: "POST", Route: "/operations",
		Body: []byte(`{"action":"` + apicontract.ActionRunCycle + `","config_revision":"r1"}`)})
	if len(perSet.Command) == 0 && perSet.GapDetail == cycle.GapDetail {
		t.Errorf("%s and %s print the same sentence:\n  %s\nOne runs every enabled set and the other runs exactly one, so a gap written for the first is not an answer about the second.",
			apicontract.ActionRunBackupSet, apicontract.ActionRunCycle, perSet.GapDetail)
	}
	if len(perSet.Command) == 0 && !strings.Contains(perSet.GapDetail, "fetch") {
		t.Errorf("the %s gap does not name the verb an operator would reach for (`fetch`), so it does not say why that verb is not the answer:\n  %s",
			apicontract.ActionRunBackupSet, perSet.GapDetail)
	}
	if len(cycle.Command) != 0 {
		t.Errorf("%s printed the command %v; `"+Binary+" run` opens the service in the operator's own process and runs a cycle THERE", apicontract.ActionRunCycle, cycle.Command)
	}

	// And the examples drive all three, because an arm no example visits
	// is an arm the dispatcher-driven parse test never sees.
	seen := map[string]bool{}
	for _, ex := range Examples() {
		if ex.Method != "POST" || ex.Route != "/operations" {
			continue
		}
		var req apicontract.SubmitOperationRequest
		if !decode(ex.Body, &req) {
			t.Errorf("an operations example does not decode as the contract's own request type: %s", ex.Body)
			continue
		}
		seen[req.Action] = true
	}
	for _, action := range []string{apicontract.ActionRunCycle, apicontract.ActionRunBackupSet, apicontract.ActionRestorePlacement} {
		if !seen[action] {
			t.Errorf("no example carries action %q, so nothing drives that arm", action)
		}
	}
	for action := range seen {
		switch action {
		case apicontract.ActionRunCycle, apicontract.ActionRunBackupSet, apicontract.ActionRestorePlacement:
		default:
			t.Errorf("an example carries action %q, which the contract does not define; the last one of those certified a branch no client can reach", action)
		}
	}
}

// Gaps() is what the guard in core/cmd/backup-manager reads, and a
// declared list is only worth what it covers. This is the coverage half:
// every sentence Echo can actually produce has to be in it.
func TestEveryGapSentenceEchoCanPrintIsDeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, gap := range Gaps() {
		declared[gap.Why] = true
	}
	if len(declared) == 0 {
		t.Fatal("Gaps() is empty, so the guard that reads it checks nothing")
	}

	seen := 0
	check := func(a Action) {
		t.Helper()
		line := Echo(a)
		if line.GapDetail == "" {
			return
		}
		seen++
		if !declared[line.GapDetail] {
			t.Errorf("%s %s prints a gap sentence Gaps() does not declare:\n  %s\nThe guard in core/cmd/backup-manager reads that list, so an undeclared sentence is one nothing checks against the verb tables.",
				a.Method, a.Route, line.GapDetail)
		}
	}
	for _, route := range Routes() {
		method, path, _ := strings.Cut(route, " ")
		check(Action{Method: method, Route: path})
		check(Action{Method: method, Route: path, Query: url.Values{}})
	}
	for _, ex := range Examples() {
		check(ex)
	}
	if seen == 0 {
		t.Fatal("no route printed a gap at all, so this test compared nothing")
	}
}
