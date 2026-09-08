package cliecho

import (
	"sort"
	"strings"
	"testing"
)

// The mechanical form of "a command never carries a key, a password or a
// token", and it is mechanical in the one way that matters: it does not
// know what a credential is called.
//
// # Why the old shape of this test could not hold
//
// It drove every builder with a body carrying eight hard-coded field
// names (private_key_pem, passphrase, password, token, secret, secret_key,
// access_key, credentials) and grepped the argv for those values. A secret
// arriving in any other field walked straight through, and two real ones
// did:
//
//   - an S3 endpoint with userinfo in it
//     (https://AKIA...:wJalr...@minio.internal:9000), which is a spelling
//     rclone accepts and this contract deliberately does not validate; and
//   - --credentials-command, whose words are the caller's and can be
//     `printf %s AKIA...:secret`.
//
// Neither field is credential-SHAPED by name, which is exactly why a
// name-based list missed them, and nothing about the list said what it was
// missing.
//
// So this asserts the opposite property. Every string field on every
// schema in the contract carries a distinct canary, and a canary that
// reaches the argv is a failure UNLESS the flag it sits behind is on the
// list below. The list is the review artefact: adding to it is a claim,
// in writing, that this flag carries something a request said out loud and
// an operator has to be able to retype.
func TestNoRequestFieldReachesTheArgvUnlessItIsEchoedOnPurpose(t *testing.T) {
	const marker = "cliechoCANARY"
	canary := func(path string) string {
		var b strings.Builder
		b.WriteString(marker)
		for _, r := range path {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	bodies := canaryBodies(t, canary)

	// Realistic path parameters, because a path parameter is not a field
	// of the request body and the question here is what the BODY can put
	// on a line. Quoting of a hostile path parameter is quoting_test.go's.
	params := map[string]string{"source": "api-server", "set": "var-backups", "name": "dump.tar", "id": "offsite_s3"}

	usedFlag := map[string]bool{}
	usedOperand := map[string]bool{}
	built := 0
	for _, route := range Routes() {
		method, path, _ := strings.Cut(route, " ")
		for _, body := range bodies {
			line := Echo(Action{Method: method, Route: path, Params: params, Body: body.body})
			if len(line.Command) > 0 {
				built++
			}
			for i, arg := range line.Command {
				if !strings.Contains(arg, marker) {
					continue
				}
				flag := ""
				if i > 0 && strings.HasPrefix(line.Command[i-1], "--") {
					flag = line.Command[i-1]
				}
				if flag == "" {
					if _, ok := operandsThatNameTheSubject[route]; ok {
						usedOperand[route] = true
						continue
					}
					t.Errorf("%s puts a %s field on the command line as an OPERAND:\n  %s\nAdd an entry to operandsThatNameTheSubject saying which id this is, or stop printing it.",
						route, body.schema, line.Shell())
					continue
				}
				if _, ok := flagsThatEchoWhatTheRequestSaid[flag]; ok {
					usedFlag[flag] = true
					continue
				}
				t.Errorf("%s prints a %s field after %s:\n  %s\nThis panel is exportable and copy-to-clipboard, so a value that reaches it reaches whatever issue tracker the export lands in. Either name the flag with a placeholder instead, or add %s to flagsThatEchoWhatTheRequestSaid with a sentence saying why an operator has to be able to retype it.",
					route, body.schema, flag, line.Shell(), flag)
			}
		}
	}
	if built == 0 {
		t.Fatal("no route built a command from any canary body, so this test proves nothing")
	}

	// A dead exemption is worse than none: it reads as a reviewed
	// decision about a line nothing prints any more, and it is what the
	// next person adds to.
	for flag := range flagsThatEchoWhatTheRequestSaid {
		if !usedFlag[flag] {
			t.Errorf("%s is exempt and no builder prints a request field after it any more. Delete the entry rather than leaving a standing permission nothing needs.", flag)
		}
	}
	for route := range operandsThatNameTheSubject {
		if !usedOperand[route] {
			t.Errorf("%s is exempt from the operand rule and no longer puts a request field in an operand. Delete the entry.", route)
		}
	}
}

// flagsThatEchoWhatTheRequestSaid is the exemption list the test above
// reads, and every entry is a claim about one flag: the request said this
// out loud, an operator retyping the command has to say it too, and it is
// not credential material.
//
// A flag that is not here and prints something the request carried fails.
// That is the direction the rule has to run in: the set of fields a
// secret can arrive in is not knowable, and the set of flags this package
// prints is right here.
var flagsThatEchoWhatTheRequestSaid = map[string]string{
	// A backup set's own description. Every one of these is a
	// connection detail an operator types when they create the set by
	// hand, and none of them is a secret: the key is named by id and the
	// host key is public.
	"--host":                "the source host, which is what the set connects to",
	"--user":                "the login name on that host; the key behind it is named by id and never printed",
	"--remote-path":         "the directory on the source being collected",
	"--local-path":          "where the copies land on this deployment",
	"--include":             "the filename patterns the set collects",
	"--completion-strategy": "which rule decides a file is finished",
	"--validator-id":        "the id of a registered validator",
	"--ssh-key-id":          "an id this deployment minted for a key it already holds, never the key",
	"--known-hosts-line":    "a PUBLIC host key, deliberately printed in full: an operator pasting the command has to pin the same one",

	// A storage destination's description. mediumSpecFlags' own doc has
	// the reasoning; what matters here is that all four credential
	// spellings name a REFERENCE and the material reaches the binary on
	// standard input.
	"--type":                "which kind of destination this is",
	"--region":              "the destination's region",
	"--endpoint":            "the destination's address, with any userinfo stripped out of it first (see endpointWithoutUserinfo)",
	"--bucket":              "the bucket copies are written to",
	"--prefix":              "the key prefix under it",
	"--storage-class":       "the storage class objects land in",
	"--upload-verification": "which verification class this destination promises",
	"--credentials-id":      "an id this deployment minted for credentials it already holds, never the credentials",
	"--credentials-file":    "a PATH to a credentials file, never its contents",
	"--credentials-env":     "the NAME of an environment variable, never its value",

	// Retention and settings scalars.
	"--timezone":       "the IANA zone retention decisions are made in",
	"--week-starts-on": "which weekday a retention week begins on",

	// The rest.
	"--medium": "the id of the destination a restore reads from",
	"--note":   "the operator's own note on a retry, which they wrote and can read back",
}

// operandsThatNameTheSubject is the same list for the positional argument
// a verb takes, and every entry is a route whose command names its subject
// with an id the request body carried rather than with the path.
//
// They are per-route rather than one blanket permission because an
// operand has no flag name in front of it to say what it is, so the only
// place the claim can be made is against the route that prints it.
var operandsThatNameTheSubject = map[string]string{
	"POST /system/first-run":          "source/name, the backup set being created",
	"POST /backup-sets":               "source/name, the backup set being created",
	"POST /operations":                "the artifact id a restore names",
	"POST /storage-mediums":           "the destination's own id",
	"POST /storage-mediums/preflight": "the candidate destination's id",
}

// A credential in an endpoint URL, which is the leak the name-based test
// could not see: the field is not credential-shaped, the contract does not
// validate it, and rclone takes the spelling.
//
// The whole userinfo is dropped rather than starred out. A line reading
// --endpoint https://***:***@minio.internal:9000 still says a credential
// was there and still has to be edited before it can be run; the
// destination without it is the address, which is the part an operator
// needs, and the credential reaches the CLI through one of the four
// credential flags instead.
func TestAnEndpointsUserinfoNeverReachesTheLine(t *testing.T) {
	// Obviously fake, and the AWS documentation's own example key.
	const key = "AKIAIOSFODNN7EXAMPLE"
	const secretPart = "notARealSecret"

	for _, tc := range []struct {
		name     string
		endpoint string
		want     string
	}{
		{"a key and a secret", "https://" + key + ":" + secretPart + "@minio.internal:9000", "https://minio.internal:9000"},
		{"a bare user", "https://" + key + "@minio.internal:9000", "https://minio.internal:9000"},
		{"a path as well", "https://" + key + ":" + secretPart + "@minio.internal:9000/backups", "https://minio.internal:9000/backups"},
		{"no scheme at all", key + ":" + secretPart + "@minio.internal:9000", "minio.internal:9000"},
		{"nothing to strip", "https://s3.eu-central-1.example.net", "https://s3.eu-central-1.example.net"},
		{"an at sign in the path only", "https://minio.internal/a@b", "https://minio.internal/a@b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := Echo(Action{Method: "POST", Route: "/storage-mediums",
				Body: []byte(`{"id":"offsite_s3","type":"s3","bucket":"acme-backups","endpoint":"` + tc.endpoint + `"}`)})
			shell := line.Shell()
			if strings.Contains(shell, key) || strings.Contains(shell, secretPart) {
				t.Errorf("a credential in the endpoint URL reached the line:\n  %s", shell)
			}
			if !strings.Contains(shell, "--endpoint "+tc.want) {
				t.Errorf("the endpoint prints as\n  %s\nand does not carry --endpoint %s, so the destination it names was lost along with the credential", shell, tc.want)
			}
		})
	}
}

// --credentials-command is the second thing the name-based test could not
// see. The CLI runs those words directly rather than through a shell, and
// they are the caller's words: `printf %s AKIA...:secret` is a valid
// credentials command and a credential on a command line.
//
// So the flag is named and the words are not, which is what the
// placeholder machinery is for. The line says it is not runnable as
// printed, because it is not: an operator has to supply the command
// themselves, exactly as they would for a key or a passphrase.
func TestACredentialsCommandIsNamedAndNeverPrinted(t *testing.T) {
	const secret = "AKIAIOSFODNN7EXAMPLE:notARealSecret"
	line := Echo(Action{Method: "POST", Route: "/storage-mediums",
		Body: []byte(`{"id":"offsite_s3","type":"s3","bucket":"acme-backups","credentials":{"command":["printf","%s","` + secret + `"]}}`)})
	shell := line.Shell()
	if strings.Contains(shell, secret) || strings.Contains(shell, "printf") {
		t.Fatalf("the credentials command reached the line:\n  %s", shell)
	}
	if !strings.Contains(shell, "--credentials-command <") {
		t.Errorf("the line does not name the flag at all, so an operator cannot tell what the destination reads its credentials from:\n  %s", shell)
	}
	if !line.Placeholder {
		t.Error("the line carries a placeholder and does not say it is not runnable as printed")
	}
}

// The control for the property test: a negative assertion over a table is
// the shape that most easily degrades into checking nothing at all, so
// this is the proof that a value on a command line can be seen at all,
// and that a placeholder is not the same as a value.
func TestTheCredentialSearchWouldCatchOne(t *testing.T) {
	leak := newCmd("backup-set", "create", "a/b").flag("ssh-key-file", "cliechoCANARYpem")
	if !strings.Contains(strings.Join(leak.argv, " "), "cliechoCANARYpem") {
		t.Fatal("a command built with a secret as a flag value does not contain it, so the search above could never fire")
	}
	safe := newCmd("backup-set", "create", "a/b").placeholderFlag("ssh-key-file", "the private key file you chose")
	if strings.Contains(strings.Join(safe.argv, " "), "cliechoCANARYpem") {
		t.Fatal("a placeholder still carries the value")
	}
	if !safe.placeholder {
		t.Fatal("a command carrying a placeholder does not say it is not runnable as printed, so an operator would paste it and get a file called <the private key file you chose>")
	}

	// And the corpus really does reach the fields the old hard-coded list
	// named, so the property test is a superset of the test it replaced
	// rather than a different, narrower one.
	fields := map[string]bool{}
	for _, body := range canaryBodies(t, func(path string) string { return path }) {
		for _, name := range []string{"private_key_pem", "passphrase", "password", "token", "secret", "access_key", "credentials", "endpoint", "command"} {
			if strings.Contains(string(body.body), `"`+name+`":`) {
				fields[name] = true
			}
		}
	}
	missing := make([]string, 0, 4)
	for _, name := range []string{"private_key_pem", "passphrase", "endpoint", "credentials"} {
		if !fields[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the generated corpus carries no %v field at all, so the property test never drives the fields the hard-coded one did", missing)
	}
}
