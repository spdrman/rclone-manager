package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/spdrman/rclone-manager/core/internal/app"
	"github.com/spdrman/rclone-manager/core/internal/mediumcheck"
	"github.com/spdrman/rclone-manager/core/service"
)

// mediumVerb is one verb `medium` dispatches: its operand shape, and the
// body that runs once the arguments have been checked.
//
// It replaced a bare `func(ctx, svc, id) int` when G2.2 (#594) gave this
// command six more verbs, and the two things it added are the two things
// that had stopped being true of every verb at once.
//
// Operand shape, because they no longer share one. `preflight` and `show`
// name a medium; `list` and `import-credentials` name nothing; the arity
// refusal has to say which of those it wanted rather than always asking
// for an id.
//
// And the verb opens its own door rather than being handed a service,
// because which door is a property of the verb. `preflight <id>` reads
// this host's configuration and probes what it finds, which is what it has
// always done and is never refused beside a running engine. The writes go
// through openConfigWriteRoute, which hands the change to the process
// serving this deployment where it can and refuses where it cannot,
// because a configuration write left in the file beside a running engine
// is a change that process would never read (#538, #543). Handing every
// verb one pre-opened service would have quietly put the writes on the
// wrong side of that.
type mediumVerb struct {
	// operand is what this verb takes after its own name, spelled the way
	// usage() spells it. Empty for a verb that takes none.
	operand string

	// run is the verb's whole body, given the configuration path rather
	// than anything opened from it.
	run func(ctx context.Context, cfgPath, id string, f mediumFlags) int
}

// mediumVerbs is every verb `medium` dispatches, keyed by the word an
// operator types.
//
// A table rather than a string literal, because a literal is invisible to
// everything that checks this binary's surface.
// TestUsage_EveryRegisteredCommandIsPinned reads the dispatch map in
// main.go, which holds one entry for `medium` and cannot see a level below
// it, so a verb added here with no line in usage() would be undiscoverable
// to an operator and pinned by nothing, which is the failure #549 was
// filed about and the very example that test's own doc reaches for.
// `backup-set` has had backupSetVerbNames since #391 for the same reason.
// TestUsage_NamesEveryMediumVerb holds this table against usage(), so the
// next verb is caught without anybody remembering that test is there.
var mediumVerbs = map[string]mediumVerb{
	"list":               {operand: "", run: mediumList},
	"show":               {operand: "<medium-id>", run: mediumShow},
	"import-credentials": {operand: "", run: mediumImportCredentials},
	"add":                {operand: "<medium-id>", run: mediumAdd},
	"edit":               {operand: "<medium-id>", run: mediumEdit},
	"remove":             {operand: "<medium-id>", run: mediumRemove},
	// `test-connection` and `preflight` are ONE check under two names
	// (H2.2, issue #622), and the two entries are the whole of how that
	// is kept true: they name the same function, so there is no second
	// implementation to drift.
	//
	// The same idea used to be called three things. "Verify" on the
	// destinations card, `preflight` here, and "Test connection" on the
	// source side of this very product, which is one operator learning
	// three words for one button. `test-connection` is the name that
	// wins, because it is the one already in use for the other half of
	// the same question and the one an operator would guess.
	//
	// `preflight` stays, forever, and not as a deprecation. It is in
	// shell scripts and deployment steps that were written against it,
	// its removal would buy nobody anything, and the usage block lists
	// both so somebody reading the only reference the binary gives them
	// finds whichever they came looking for. What CHANGED is which name
	// the product says first: the button, the echoed command line and
	// this table's own preferred entry all say test-connection now.
	"test-connection": {operand: "<medium-id>", run: mediumPreflightVerb},
	"preflight":       {operand: "<medium-id>", run: mediumPreflightVerb},
	"default":         {operand: "<medium-id>", run: mediumDefault},
}

// mediumVerbNames is every verb `medium` dispatches, sorted, so a refusal
// that lists them reads the same way twice.
func mediumVerbNames() []string {
	names := make([]string, 0, len(mediumVerbs))
	for verb := range mediumVerbs {
		names = append(names, verb)
	}
	sort.Strings(names)
	return names
}

// quotedMediumVerbs renders the table the way this command's refusal has
// always named it: `"preflight"` with one verb, `"a" or "b"` with two,
// `"a", "b" or "c"` with more, which is the shape quarantine.go spells
// out by hand. Rendered rather than typed, so a verb added above turns up
// in the refusal without a second edit somebody has to remember.
func quotedMediumVerbs() string {
	names := mediumVerbNames()
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, strconv.Quote(name))
	}
	switch len(quoted) {
	case 0:
		// Unreachable while mediumVerbs has an entry, and a sentence is a
		// better way to find out it does not than an index panic.
		return "no verb at all, which means mediumVerbs is empty"
	case 1:
		return quoted[0]
	}
	return strings.Join(quoted[:len(quoted)-1], ", ") + " or " + quoted[len(quoted)-1]
}

// mediumFlags is every flag `medium` declares, in the one struct the verbs
// read them out of.
//
// # There is no flag here that takes a secret, and there will not be
//
// Search this struct for an access key or a secret access key; you will
// not find one, on purpose, which is config.MediumCredentials' own
// enforcement applied to a command line. The reason is sharper here than
// in the config file. EPIC G requires every action taken in the browser to
// print its `backup-manager` equivalent into the global terminal, and that
// terminal is copy-to-clipboard and exportable, so anything printed there
// ends up pasted into a chat window eventually. A surface with a flag to
// redact is a surface that will one day forget to; a surface with no such
// flag cannot.
//
// So the material arrives on STDIN, through `import-credentials --stdin`,
// and stdin is not in the process table and not in shell history. Every
// other verb names a credential REFERENCE, and the echoed command line is
// therefore the command that actually works, byte for byte, with nothing
// starred out.
type mediumFlags struct {
	json      *bool
	stdin     *bool
	candidate *bool
	noVerify  *bool

	mediumType         *string
	region             *string
	endpoint           *string
	bucket             *string
	prefix             *string
	storageClass       *string
	uploadVerification *string

	credentialsID      *string
	credentialsFile    *string
	credentialsEnv     *string
	credentialsCommand *string

	fs *flag.FlagSet
}

// declareMediumFlags puts every medium flag on fs. One flag set for all
// seven verbs, the way `backup-set`'s create/patch/remove share
// declareBackupSetFlags: parseFlagsAroundOperands has to know every flag
// before it sees the verb, since a flag may legally be written on either
// side of the operand.
func declareMediumFlags(fs *flag.FlagSet) mediumFlags {
	return mediumFlags{
		fs:        fs,
		json:      fs.Bool("json", false, "list/show: print the destinations as JSON instead of as text"),
		stdin:     fs.Bool("stdin", false, "import-credentials: read AWS shared-credentials text from standard input. This is the only way material ever reaches this command, and it is deliberately not a flag: a secret on a command line is in `ps` output for every user on the box and in shell history"),
		candidate: fs.Bool("candidate", false, "preflight: prove a destination described by the flags below, which is NOT declared, and write nothing whatever the report says"),
		noVerify:  fs.Bool("no-verify", false, "add/edit: write the destination without proving it works first. The output says in so many words that nothing was proven; it exists for building configuration offline, against a bucket this host cannot reach"),

		mediumType:         fs.String("type", "s3", "add/edit/preflight --candidate: the backend. `s3` is the only value"),
		region:             fs.String("region", "", "add/edit/preflight --candidate: the provider region, passed to the backend unexamined"),
		endpoint:           fs.String("endpoint", "", "add/edit/preflight --candidate: an endpoint override for an S3-compatible service; empty means the provider's own endpoint for the region"),
		bucket:             fs.String("bucket", "", "add/edit/preflight --candidate: the bucket artifacts are written into"),
		prefix:             fs.String("prefix", "", "add/edit/preflight --candidate: the key namespace inside the bucket; empty puts the key layout at the root"),
		storageClass:       fs.String("storage-class", "", "add/edit/preflight --candidate: the S3 storage class objects are written with; empty means STANDARD"),
		uploadVerification: fs.String("upload-verification", "", "add/edit/preflight --candidate: readback or attested; empty means readback"),

		credentialsID:      fs.String("credentials-id", "", "add/edit/preflight --candidate: an id an earlier `medium import-credentials` returned"),
		credentialsFile:    fs.String("credentials-file", "", "add/edit/preflight --candidate: a path to an AWS shared-credentials file on this host. The preferred source when you already have one: rclone opens it itself, so the secret never enters this process"),
		credentialsEnv:     fs.String("credentials-env", "", "add/edit/preflight --candidate: the NAME of an environment variable the credentials are read from at connection time. Never a value"),
		credentialsCommand: fs.String("credentials-command", "", "add/edit/preflight --candidate: a command whose stdout is the credentials, split on spaces and run directly, never through a shell"),
	}
}

// spec builds the destination description add, edit and preflight
// --candidate all submit, out of the flags that were actually written.
//
// fs.Visit rather than the flags' own values, for the reason
// buildSettingsPatch reads its own that way: on an EDIT, "this flag was
// not passed" and "this flag was passed as empty" are different requests,
// and only the first one means "leave it alone". It matters most for the
// credential, where an unnamed one means "keep the credential already
// configured" and an empty one would mean "this destination reaches
// nowhere".
func (f mediumFlags) spec(id string) service.StorageMediumSpec {
	spec := service.StorageMediumSpec{ID: id}
	f.fs.Visit(func(fl *flag.Flag) {
		switch fl.Name {
		case "type":
			spec.Type = *f.mediumType
		case "region":
			spec.Region = *f.region
		case "endpoint":
			spec.Endpoint = *f.endpoint
		case "bucket":
			spec.Bucket = *f.bucket
		case "prefix":
			spec.Prefix = *f.prefix
		case "storage-class":
			spec.StorageClass = *f.storageClass
		case "upload-verification":
			spec.UploadVerification = *f.uploadVerification
		case "credentials-id":
			spec.Credentials.ID = *f.credentialsID
		case "credentials-file":
			spec.Credentials.File = *f.credentialsFile
		case "credentials-env":
			spec.Credentials.Env = *f.credentialsEnv
		case "credentials-command":
			spec.Credentials.Command = strings.Fields(*f.credentialsCommand)
		}
	})
	return spec
}

// cmdMedium is `backup-manager medium <verb> [<medium-id>] [flags]`: the
// CLI's own half of the storage-destination surface.
//
// `preflight` (issue #443) was the first verb and was the only one until
// G2.2 (#594) added list, show, import-credentials, add, edit and remove,
// which is what makes a destination something an operator can configure
// from a terminal rather than only by editing config.yaml. Every one of
// them goes through internal/app or core/service, so this command and the
// API cannot disagree about what a probe found or about what was written
// (FR-34), which is the rule this command's doc has stated for preflight
// since it shipped.
//
// # Which world each verb acts in
//
// `preflight <id>` reads THIS host's configuration file and probes what it
// finds. That is what it has always done, it is a read, and it is never
// refused beside a running engine.
//
// Everything else goes through openConfigWriteRoute, which hands the work
// to the process serving this deployment where a route was given and
// refuses where one was not, with nothing written. `add`, `edit` and
// `remove` are configuration writes, and a write left in the file beside a
// running engine is a change that process would never read: there is still
// no config watcher and no SIGHUP reload in this build (#538, #543).
//
// `import-credentials` and `preflight --candidate` are routed too, and
// each for its own reason rather than by association. The id
// `import-credentials` mints names a file on the host that wrote it, so an
// id minted here and used in a write that goes to the engine would name a
// file on the wrong machine. And `preflight --candidate` is the check
// `add` runs before it writes, so it has to happen where the add would
// happen: proving this host's route to a bucket and then declaring the
// destination somewhere else proves nothing about the destination that
// gets used.
//
// `list` and `show` are NOT routed, and that is a correction rather than
// an omission. They answer from this host's configuration file, exactly as
// `settings` on its own does, because openConfigWriteRoute refuses when
// something is serving this deployment and no route was named: routing
// them would have made looking at your own destinations impossible on the
// ordinary install, which has an engine running and no route configured.
// See withMediumRead.
//
// It needs a real transport, unlike most commands here, because reaching a
// bucket is the entire point.
func cmdMedium(args []string) int {
	fs, cfgPath := newFlagSet("medium")
	flags := declareMediumFlags(fs)
	operands, err := parseFlagsAroundOperands(fs, args)
	if err != nil {
		return 2
	}
	if len(operands) == 0 {
		return usageError("medium: expected %s", mediumOperandShapes())
	}
	verb := operands[0]
	spec, ok := mediumVerbs[verb]
	if !ok {
		return usageError("medium: unknown subcommand %q (expected %s)", verb, quotedMediumVerbs())
	}

	// Arity is checked per verb rather than once, because they no longer
	// share one. A verb that names a medium and was given none is a
	// different mistake from one that names nothing and was given
	// something, and telling somebody "expected list|show|... <medium-id>"
	// for `medium list nas-backups` would send them looking for an id they
	// were right not to have.
	want := 1
	if spec.operand != "" {
		want = 2
	}
	if len(operands) != want {
		if spec.operand == "" {
			return usageError("medium: %s takes no argument", verb)
		}
		return usageError("medium: expected %s", mediumOperandShapes())
	}
	id := ""
	if want == 2 {
		id = operands[1]
	}

	// Both refusals above happen before anything is opened, which is what
	// TestMediumPreflightIsRefusedBeforeAnythingIsOpened checks by running
	// them against a config path that does not exist. Looking the verb up
	// and checking its arity before the open rather than after keeps that
	// true for a mistyped verb as well as a missing one.
	return spec.run(context.Background(), *cfgPath, id, flags)
}

// mediumOperandShapes renders the arity refusal, naming each verb with the
// operand it actually takes rather than asking for an id on behalf of the
// four verbs that do not want one.
//
// It reads mediumVerbs, so a verb added there turns up here without a
// second edit, which is the same property quotedMediumVerbs has and the
// reason both are rendered rather than typed.
func mediumOperandShapes() string {
	names := mediumVerbNames()
	shapes := make([]string, 0, len(names))
	for _, name := range names {
		if operand := mediumVerbs[name].operand; operand != "" {
			shapes = append(shapes, name+" "+operand)
		} else {
			shapes = append(shapes, name)
		}
	}
	return strings.Join(shapes, " | ")
}

// withMediumRoute opens the door a routed medium verb goes through and
// runs body against it. See cmdMedium's own doc for which verbs come here
// and why each of them does.
func withMediumRoute(ctx context.Context, cfgPath string, body func(route configWriteRoute) int) int {
	route, cleanup, err := openConfigWriteRoute(ctx, cfgPath)
	if err != nil {
		return fail(err)
	}
	defer cleanup()
	logStartup(ctx, logger(), app.BuildVersionInfo(version, commit))
	return body(route)
}

// withMediumRead opens this host's own configuration and journal, and is
// never refused beside a running engine.
//
// The two read verbs go through here rather than through
// openConfigWriteRoute, and the difference is #538's rule rather than a
// convenience. openConfigWriteRoute refuses when something is serving this
// deployment and no route was named, which is right for a WRITE (a change
// left in the file is one that process would never read) and wrong for a
// read: an operator on a stock install has an engine running and no
// BACKUP_MANAGER_API_URL set, so routing `medium list` would have made
// looking at your own destinations impossible on the ordinary deployment.
// `settings` on its own already reads this way for the same reason, and
// #544 routed four reads and deliberately stopped there.
//
// So these answer about THIS HOST's configuration file, which can differ
// from what a serving process loaded. That difference is the one cmdMedium's
// own doc has always stated for `preflight`, and the way into it is a
// hand-edited config.yaml rather than anything this binary writes.
func withMediumRead(ctx context.Context, cfgPath string, body func(svc *service.BackupService) int) int {
	svc, cleanup, err := openBackupService(ctx, cfgPath, readsConfig)
	if err != nil {
		return fail(err)
	}
	defer cleanup()
	return body(svc)
}

// mediumList is `medium list`: every declared destination, in declaration
// order.
func mediumList(ctx context.Context, cfgPath, _ string, f mediumFlags) int {
	return withMediumRead(ctx, cfgPath, func(svc *service.BackupService) int {
		mediums, err := svc.ListStorageMediums(ctx)
		if err != nil {
			return fail(err)
		}
		if *f.json {
			return printMediumJSON(mediums)
		}
		if len(mediums) == 0 {
			// Unreachable since #622 made the local hard drive part of
			// every list, and kept as a hole rather than deleted: a build
			// that reached it has broken the "there is never zero
			// destinations" invariant, and an empty list printed as
			// nothing at all would be that break arriving silently.
			fmt.Println("this deployment lists no storage destinations at all, which should be impossible: the drive backups land on is always one of them")
			return 1
		}
		for _, m := range mediums {
			printMedium(m)
		}
		return 0
	})
}

// mediumShow is `medium show <medium-id>`: one destination, plus what the
// journal says is currently on it.
//
// The usage report is here rather than behind a verb of its own because it
// is the fact an operator is actually asking about when they look one of
// these up: whether anything is there, and whether it is the only copy.
func mediumShow(ctx context.Context, cfgPath, id string, f mediumFlags) int {
	return withMediumRead(ctx, cfgPath, func(svc *service.BackupService) int {
		medium, err := svc.GetStorageMedium(ctx, id)
		if err != nil {
			return fail(err)
		}
		usage, err := svc.StorageMediumUsage(ctx, id)
		if err != nil {
			return fail(err)
		}
		if *f.json {
			return printMediumJSON(struct {
				Medium service.StorageMediumSummary `json:"medium"`
				Usage  service.StorageMediumUsage   `json:"usage"`
			}{medium, usage})
		}
		printMedium(medium)
		printMediumUsage(usage)
		return 0
	})
}

// mediumImportCredentials is `medium import-credentials --stdin`: the one
// command on this binary that ever holds an S3 secret.
//
// It reads AWS shared-credentials text from standard input, hands it
// straight to the service (which validates it through the same parser a
// connection would, and writes it 0600 beside config.yaml), and prints the
// id. It never prints, logs or echoes the material, exactly as POST
// /ssh-keys answers with a reference and never a key.
//
// --stdin is required rather than assumed. Reading a terminal's stdin by
// default would hang with no explanation on `medium import-credentials`
// typed by somebody exploring, and the flag is also the thing that makes
// the shape of this command obvious in the usage block and in an echoed
// command line: there is nowhere else the material could be coming from.
func mediumImportCredentials(ctx context.Context, cfgPath, _ string, f mediumFlags) int {
	if !*f.stdin {
		return usageError("medium import-credentials: --stdin is required; credentials are read from standard input and never from a flag, because a secret on a command line is in `ps` output for every user on this host and in shell history")
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, maxImportedCredentialsBytes+1))
	if err != nil {
		return fail(fmt.Errorf("reading credentials from standard input: %w", err))
	}
	if len(raw) > maxImportedCredentialsBytes {
		// The size, never the bytes. A refusal that quoted what it read
		// would be the one place this command prints a secret.
		return fail(fmt.Errorf("the credentials on standard input are larger than %d bytes, which is far more than a shared-credentials file with one profile; nothing was written", maxImportedCredentialsBytes))
	}

	return withMediumRoute(ctx, cfgPath, func(route configWriteRoute) int {
		id, err := importCredentialsThrough(ctx, route, raw)
		if err != nil {
			return fail(err)
		}
		fmt.Printf("credentials imported\n")
		fmt.Printf("  id: %s\n", id)
		fmt.Println("  the material was written to a 0600 file beside this deployment's configuration and is never read back out")
		fmt.Printf("  name it with --credentials-id %s\n", id)
		return 0
	})
}

// maxImportedCredentialsBytes bounds what this command will read off
// standard input. A shared-credentials file with one profile is a few
// hundred bytes; this is wide margin over that, meant to stop a pipe that
// is not what the operator thought it was from being read into memory
// whole, not to be a realistic ceiling.
const maxImportedCredentialsBytes = 64 << 10 // 64 KiB

// importCredentialsThrough parses the shared-credentials text and hands
// the two values to the route.
//
// The parse is here rather than in core/service because the route takes an
// access key and a secret, which is the shape the API's own import takes,
// and this is the one caller that starts from text. core/service validates
// what it is given either way, through the same parser a connection uses,
// so this parse is a translation and never a second opinion about what
// usable credentials are.
func importCredentialsThrough(ctx context.Context, route configWriteRoute, raw []byte) (string, error) {
	accessKeyID, secretAccessKey, sessionToken, err := parseSharedCredentialsText(raw)
	if err != nil {
		return "", err
	}
	ref, err := route.ImportStorageCredentials(ctx, accessKeyID, secretAccessKey, sessionToken)
	if err != nil {
		return "", err
	}
	return ref.ID, nil
}

// parseSharedCredentialsText pulls the three values out of AWS
// shared-credentials text.
//
// It reports the SHAPE of a problem and never the bytes that failed, which
// is internal/transport/rclone's rule for the identical parse and matters
// more here: this runs in a terminal whose transcript an operator exports
// and pastes into a support thread.
func parseSharedCredentialsText(raw []byte) (accessKeyID, secretAccessKey, sessionToken string, err error) {
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "[") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "aws_access_key_id":
			accessKeyID = strings.TrimSpace(value)
		case "aws_secret_access_key":
			secretAccessKey = strings.TrimSpace(value)
		case "aws_session_token":
			sessionToken = strings.TrimSpace(value)
		}
	}
	if accessKeyID == "" || secretAccessKey == "" {
		return "", "", "", fmt.Errorf(
			"standard input does not carry AWS shared-credentials text: expected aws_access_key_id and aws_secret_access_key lines under a [default] profile. Nothing was written and nothing about what was read is reported here")
	}
	return accessKeyID, secretAccessKey, sessionToken, nil
}

// mediumAdd is `medium add <medium-id> [flags]`: declare a destination.
//
// It VERIFIES BY DEFAULT and refuses to write when verification fails.
// That is the non-interactive equivalent of the wizard's Save button being
// disabled, and it is what makes this scriptable across a fleet: an
// operator who runs this over fifty hosts finds out about the one whose
// bucket policy denies PutObject from the exit code, rather than from a
// cycle three days later.
//
// --no-verify exists for the operator building configuration offline
// against a bucket this host cannot reach, and it says in its own output
// that nothing was proven, because a line that did not say so would be
// indistinguishable from a green check.
func mediumAdd(ctx context.Context, cfgPath, id string, f mediumFlags) int {
	return mediumWrite(ctx, cfgPath, id, f, false)
}

// mediumEdit is `medium edit <medium-id> [flags]`: replace a destination's
// description.
//
// A flag left off keeps whatever the destination already says, which
// matters most for the credential: this product never reports a
// destination's credential, not even its kind, so an edit that demanded
// one back would mean re-importing an access key to change a region.
func mediumEdit(ctx context.Context, cfgPath, id string, f mediumFlags) int {
	return mediumWrite(ctx, cfgPath, id, f, true)
}

// mediumWrite is add and edit's shared body. The only difference between
// them is which route method is called; everything else, the verification
// in front of the write included, has to be identical or a destination
// could be creatable in a shape it could not be edited into.
func mediumWrite(ctx context.Context, cfgPath, id string, f mediumFlags, update bool) int {
	return withMediumRoute(ctx, cfgPath, func(route configWriteRoute) int {
		spec := f.spec(id)

		if !*f.noVerify {
			// An EDIT that names no credential cannot be verified as a
			// candidate: a candidate carries its own credential reference
			// by definition, and this deployment does not report the one
			// already configured, so there is nothing to check with. The
			// write is still made, and the honest thing is to say what was
			// and was not proven rather than to skip the check silently.
			if update && spec.Credentials.ID == "" && spec.Credentials.File == "" && spec.Credentials.Env == "" && len(spec.Credentials.Command) == 0 {
				fmt.Println("not verified before writing: this edit names no credential, so it keeps the one already configured, and a candidate cannot be proven with a credential this command cannot read")
				fmt.Printf("  prove it afterwards with: backup-manager medium preflight %s\n", id)
			} else {
				report, err := route.PreflightStorageMediumCandidate(ctx, spec)
				if err != nil {
					return fail(err)
				}
				printMediumReport(report)
				if !report.OK {
					fmt.Println("nothing was written: a destination that cannot be proven is not declared")
					return 1
				}
			}
		} else {
			// Said out loud, every time. A --no-verify write that printed
			// the same thing a verified one printed would be a green line
			// standing behind a check nobody ran.
			fmt.Println("not verified: --no-verify was given, so no credential was obtained, no endpoint was contacted and no object was written or read back")
			fmt.Printf("  prove it afterwards with: backup-manager medium preflight %s\n", id)
		}

		var (
			medium service.StorageMediumSummary
			err    error
		)
		if update {
			medium, err = route.UpdateStorageMedium(ctx, spec)
		} else {
			medium, err = route.CreateStorageMedium(ctx, spec)
		}
		if err != nil {
			return fail(err)
		}
		printMedium(medium)
		return 0
	})
}

// mediumRemove is `medium remove <medium-id>`: un-declare a destination.
//
// FR-30 makes the engine refuse while any copy names it, and this prints
// what is on it when that happens rather than only the refusal, because
// the operator's next question is always which backup sets.
func mediumRemove(ctx context.Context, cfgPath, id string, _ mediumFlags) int {
	return withMediumRoute(ctx, cfgPath, func(route configWriteRoute) int {
		if err := route.RemoveStorageMedium(ctx, id); err != nil {
			code := fail(err)
			if usage, uerr := route.StorageMediumUsage(ctx, id); uerr == nil && usage.Placements > 0 {
				printMediumUsage(usage)
			}
			return code
		}
		fmt.Printf("storage destination %s is no longer declared\n", id)
		fmt.Println("  nothing was deleted: this removes the declaration and no backup datum")
		return 0
	})
}

// mediumPreflightVerb is `medium preflight <medium-id>` and `medium
// preflight --candidate <medium-id> [the add flags]`.
//
// The two are one verb because they are one question asked about two
// things, and they go through different doors for the reason cmdMedium's
// own doc gives: the by-id form is a read of this host's configuration and
// is never refused beside a running engine, and the candidate form is the
// check `add` runs before it writes, so it has to happen where the add
// would.
func mediumPreflightVerb(ctx context.Context, cfgPath, id string, f mediumFlags) int {
	if *f.candidate {
		return withMediumRoute(ctx, cfgPath, func(route configWriteRoute) int {
			report, err := route.PreflightStorageMediumCandidate(ctx, f.spec(id))
			if err != nil {
				return fail(err)
			}
			printMediumReport(report)
			fmt.Println("nothing was written: a candidate preflight declares no destination whatever the report says")
			if !report.OK {
				return 1
			}
			return 0
		})
	}

	svc, _, cleanup, err := openService(ctx, cfgPath, true)
	if err != nil {
		return fail(err)
	}
	defer cleanup()
	logStartup(ctx, svc.Logger, app.BuildVersionInfo(version, commit))
	return mediumPreflight(ctx, svc, id)
}

// mediumDefault is `medium default <medium-id>`: move the destination a
// NEWLY CREATED retention tier starts on (H2.2, issue #622).
//
// It is EPIC G's parity rule applied to the one control #622 adds to the
// settings page that is not a picker. The web UI's "Make default" button
// echoes this exact line, so an operator who moved a default by clicking
// has, by the end, read the command that moves it on the next fifty
// hosts.
//
// It moves nothing else, and the output says so out loud. An operator
// reading "the default is now offsite_s3" beside a settings page could
// reasonably fear their backups just started moving, and the sentence
// that costs one line here is the one that stops somebody reaching for a
// rollback.
//
// It goes through openConfigWriteRoute like every other write in this
// command, for #538's reason: a configuration change left in the file
// beside a running engine is a change that process would never read.
func mediumDefault(ctx context.Context, cfgPath, id string, _ mediumFlags) int {
	return withMediumRoute(ctx, cfgPath, func(route configWriteRoute) int {
		medium, err := route.SetDefaultStorageMedium(ctx, id)
		if err != nil {
			return fail(err)
		}
		fmt.Printf("%s is now this deployment's default storage destination\n", medium.ID)
		fmt.Println("  a retention tier created from here on starts on it")
		fmt.Println("  nothing moved: every tier that already names a destination goes on naming it, and no backup was relocated")
		printMedium(medium)
		return 0
	})
}

// printMedium renders one destination, and carries no credential of any
// kind, because service.StorageMediumSummary has no field one could be in.
//
// The local hard drive is rendered differently, and it has to be: it has
// no bucket, no region, no endpoint and no storage class, so the S3 shape
// printed against it would be six empty labels. What it does have is the
// drive it writes to, which is the fact #622 says the list has to carry.
func printMedium(m service.StorageMediumSummary) {
	fmt.Printf("%s\n", m.ID)
	if m.IsDefault {
		fmt.Println("  default: a retention tier created from here on starts on this destination")
	}
	if m.IsLocal {
		fmt.Println("  type: local (this deployment's own hard drive, which every retention tier that names no destination means)")
		if m.Path != "" {
			fmt.Printf("  path: %s\n", m.Path)
		} else {
			fmt.Println("  path: not known yet: this configuration has no backup set to derive one from, or its sets are on different volumes")
		}
		fmt.Println("  not declared: it is not a storage_mediums entry, so it cannot be edited or removed")
		return
	}
	fmt.Printf("  type: %s\n", m.Type)
	fmt.Printf("  bucket: %s\n", m.Bucket)
	if m.Prefix != "" {
		fmt.Printf("  prefix: %s\n", m.Prefix)
	}
	if m.Region != "" {
		fmt.Printf("  region: %s\n", m.Region)
	}
	if m.Endpoint != "" {
		fmt.Printf("  endpoint: %s\n", m.Endpoint)
	}
	fmt.Printf("  storage_class: %s\n", m.StorageClass)
	fmt.Printf("  upload_verification: %s\n", m.UploadVerification)
	if m.ReadsRequireRestore {
		fmt.Println("  reads need a restore: this class holds objects that cannot be read until an explicit restore has finished")
	}
}

// printMediumUsage renders FR-30's report: what is on a destination, per
// backup set, and how much of it is the only copy anywhere.
//
// The sets are listed and not only counted, because a count with nothing
// named is a number an operator cannot act on. The words matter as much as
// the numbers: an unreachable copy is one this deployment cannot ask
// about, and it is emphatically not a copy that is gone.
func printMediumUsage(u service.StorageMediumUsage) {
	if u.Placements == 0 {
		fmt.Println("  nothing on record is stored here")
		return
	}
	fmt.Printf("  %d cop%s on record here:\n", u.Placements, map[bool]string{true: "y", false: "ies"}[u.Placements == 1])
	for _, s := range u.BackupSets {
		fmt.Printf("    %s: %d\n", s.Set, s.Placements)
		if s.OnlyCopyHere > 0 {
			fmt.Printf("      %d of them are the only confirmed copy of their artifact anywhere\n", s.OnlyCopyHere)
		}
	}
}

// printMediumReport renders one preflight, every step in the engine's own
// order, skipped ones included.
//
// Never a single OK or FAILED. A surface that collapsed eight steps into
// one verdict would throw away the whole diagnosis, and a skipped write
// rendered as anything but "this was never tried" tells an operator their
// bucket is writable on the strength of a credential nobody obtained.
func printMediumReport(report service.MediumPreflight) {
	fmt.Printf("storage medium %s: %s\n", report.Medium, verdictWord(report.OK))
	for _, c := range report.Checks {
		fmt.Printf("  %-14s %-8s %s\n", c.Step, outcomeWordOf(c.Outcome, c.Category), c.Detail)
	}
}

// printMediumJSON is --json's whole body, so `medium list --json` and
// `medium show --json` cannot format differently.
func printMediumJSON(v any) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fail(err)
	}
	return 0
}

// mediumPreflight is the `preflight` verb's half of the command above:
// everything from the opened service onward, once cmdMedium has decided
// which verb it is.
func mediumPreflight(ctx context.Context, svc *app.Service, id string) int {
	// The local hard drive answers this too (H2.2, #622). It is dispatched
	// on the id here rather than inside app.PreflightMedium because the
	// two checks share nothing below this line: there is no declared
	// medium behind the local id, no MediumStore that could reach it, and
	// the resolver refuses it in so many words. What they share is the
	// report, which is everything this function does with the answer.
	//
	// Without this arm, `medium test-connection local` refused with "this
	// configuration does not declare that storage medium", which is both
	// true and useless: local is never declared, and the destination an
	// operator just picked in a tier would be the one destination they
	// could not check.
	report, err := preflightAnyDestination(ctx, svc, id)
	if err != nil {
		return fail(err)
	}

	fmt.Printf("storage medium %s: %s\n", report.Medium, verdictWord(report.OK))
	for _, c := range report.Checks {
		fmt.Printf("  %-14s %-8s %s\n", c.Step, outcomeWord(c), c.Detail)
	}
	if !report.OK {
		// A non-zero exit, so `medium preflight` composes into a script
		// the way `check` and `validate` already do: an operator wiring
		// this into a deployment step needs the shell to know.
		return 1
	}
	return 0
}

// preflightAnyDestination runs the right check for the destination id it
// is given: the local hard drive's, or a declared storage medium's.
//
// One function so the verb above has one answer to render. Both produce a
// mediumcheck.Report, which is the whole reason the local check was
// written into that package rather than beside a surface.
func preflightAnyDestination(ctx context.Context, svc *app.Service, id string) (mediumcheck.Report, error) {
	if id == service.StorageMediumLocalID {
		return svc.PreflightLocalMedium(ctx)
	}
	return svc.PreflightMedium(ctx, id)
}

// verdictWord renders the whole report's answer as something an operator
// reads rather than as a boolean.
func verdictWord(ok bool) string {
	if ok {
		return "ready for a backup"
	}
	return "NOT ready; see the failing checks below"
}

// outcomeWord renders one check's outcome, folding in the transport
// category where there is one. The category is the machine-readable half
// and belongs beside the word rather than buried in the sentence: an
// operator scanning this column is deciding whose problem it is.
func outcomeWord(c mediumcheck.Check) string {
	return outcomeWordOf(string(c.Outcome), c.Category)
}

// outcomeWordOf is the same rendering over plain strings, for the reports
// that come back over a route as core/service's shape rather than as the
// engine's. Two spellings of this column would be two answers to "did this
// step pass", which is the one column an operator reads first.
func outcomeWordOf(outcome, category string) string {
	if category == "" {
		return outcome
	}
	return fmt.Sprintf("%s(%s)", outcome, category)
}
