# The `/api/v1` contract

`api/v1/openapi.json` is the definition of `/api/v1`. Not the Go handlers, not
the TypeScript client: the document. Both sides are generated from it, and both
are held to it by checks that fail CI rather than by review.

Issue #166 (B6.2) is where this came from. Before it, the boundary existed but
nothing described it: the Go handlers were the only statement of the request and
response shapes, `ui/shared/src/api/contracts.ts` held a hand-transcribed copy of
the error-code list, and `ui/shared/src/api/client.ts` held a second
hand-transcribed copy of every wire shape. Three copies of one thing, each free
to drift, and one of them already had.

## The format, and why it is not something else

**OpenAPI 3.1, encoded as JSON.**

The issue asked for OpenAPI 3.1 "unless this repository already standardizes an
equivalent format", so I looked first. It does not. There is no protobuf, no
JSON Schema, no IDL of any kind, and no generated client anywhere in the tree.
The nearest thing to a machine-readable contract is
`distribution/canonical.json`, which pins packaging metadata rather than an API,
and `docs/conformance/phase-4-matrix.md`, which is a generated-and-diffed
document rather than a schema. So there was no incumbent standard to preserve,
and OpenAPI 3.1 it is.

JSON rather than YAML for one reason that matters here: the generator and the
drift check read the contract with Go's `encoding/json` and Python's `json`,
both of which are already available everywhere this repository builds. A YAML
contract would put a parser dependency in front of the one gate that has to be
reproducible byte for byte on a fresh clone. OpenAPI 3.1 documents are valid in
either encoding, so nothing is given up.

## The direction, and why it is not the other one

**Spec-first. The contract is written by hand; both bindings are generated.**

The alternative was Go-first: reflect over the handler structs, emit an OpenAPI
document, generate TypeScript from that. It is tempting, because the Go structs
already exist and the TypeScript is what has actually drifted. I did not take
it, for a reason specific to what this issue is for.

A Go struct can express a field's name and type. It cannot express whether the
operation needs a session, whether it needs a CSRF token, whether it requires an
`Idempotency-Key`, which optimistic-concurrency token it reads, whether the
destructive gate stands in front of it, or which typed errors it can return.
Those are precisely the things #166 requires to become contract *data*
("authentication requirements become explicit contract data rather than handler
convention, which makes an unauthenticated destructive endpoint a contract
violation a test can catch"). Under Go-first they would have to live in
annotations beside the structs, which is a second hand-maintained format with
none of OpenAPI's tooling and all of its drift risk.

There is a second reason. #90's adapter conformance gate has to compare an
adapter's declared API compatibility against the canonical runtime's contract.
That comparison needs a document that exists independently of building Go. A
contract that is an output of `go build` is not that.

So: the document is the source, and it is written the way any other source is.
Two consequences follow, and both are deliberate.

- The handlers are **not** generated. They still declare their own structs, and
  a reflection test compares those structs to the generated ones field by field.
  Making the handlers use the generated types is a behaviour-preserving move
  worth doing, but #81 is explicit that behaviour is not rewritten in the same
  step that a boundary is introduced, so it is not done here.
- The generator is in this repository (`scripts/api/gen-bindings.go`), stdlib
  only, no module. Every other gate here is shaped the same way, and an
  off-the-shelf OpenAPI generator would add a network fetch and a second
  language runtime to the one job that has to produce identical bytes on every
  machine. It reads only the subset of OpenAPI this contract speaks and refuses
  anything else rather than emitting something plausible.

## Regenerating

```
scripts/api/generate.sh
```

Edit `api/v1/openapi.json`, run that, commit both generated files:

| generated file | consumed by |
|---|---|
| `apps/common/webhost/apicontract/contract.gen.go` | the Go conformance tests today; the handlers themselves in a later issue |
| `ui/shared/src/api/generated/contract.ts` | `ui/shared/src/api/contracts.ts` and `client.ts`, directly |

Both carry a `DO NOT EDIT` banner. Editing either one by hand fails CI.

## What a drift failure means

There are five gates, and each answers a different question. Which one failed
tells you what to fix.

### `scripts/api/check-contract-drift.sh` says the Go or TypeScript binding does not match

The checked-in generated file is not what the contract produces. Either someone
edited generated output by hand (fix the contract instead), or someone edited
the contract and did not regenerate (run `scripts/api/generate.sh`). The failure
prints the diff.

This is the gate that catches **TypeScript drift**, and it catches it in both
directions: a hand-edited `generated/contract.ts`, and a contract change nobody
regenerated. It needs no npm install at all, because the TypeScript binding is
produced by the same Go program as the Go one.

### `scripts/api/check-contract-drift.sh` says an implementation type reached the public schema

A schema name, property name, enum value, path or operation id in the contract
names rclone, SQLite, a filesystem path or a provider SDK. #81's standing
constraint forbids that on `/api/v1`, and the issue is explicit that if a public
shape leaks one, the contract is wrong, not the check. Rename the field to say
what it means rather than what implements it.

Descriptions are deliberately exempt: a schema is allowed to say "this is not an
rclone remote", and a check that could not tell a description from an identifier
would be watered down until it fired on nothing.

### `scripts/api/check-client-paths.sh` says a client path is not a declared operation

`ui/shared/src/api/client.ts` asks for a `(method, path)` that
`api/v1/openapi.json` does not declare. On a real backend that request is a 404
or a 405, and the shared UI turns either one into "The backup service returned
an unexpected response."

This gate exists because the two checks above only cover *generated* files, and
`client.ts` is not one. It is hand-written on top of the generated module and
imports nothing from it but types and the error-code registry, so every request
path it builds is a string literal that the binding comparison cannot see. Issue
#211 measured what that cost: fourteen such pairs, four of the six shipped pages
failing outright against a real engine, and every suite in the repository green,
because the browser tests run against `createMockApi`, which implements whatever
the client asks for.

The check is static. It reads `client.ts`, strips its comments (they quote paths,
and a gate a comment can satisfy is not a gate), and reduces each request
expression back to a pattern: an interpolated value becomes `{}`, so
`"/backup-sets/" + id` reduces to `/backup-sets/{}` and the contract's
`/backup-sets/{id}` normalises to the same thing. A conditional yields both of
its branches rather than one guess, which is how `listArtifacts`' optional query
string is checked as the two URLs it can really build. No npm install, no
bundler, no browser.

Three properties are worth knowing before changing it:

- **It fails closed.** A path expression it cannot reduce is a failure, and every
  method on `httpApi` must produce at least one request. A silent skip here would
  be indistinguishable from a pass.
- **It refuses to pass vacuously.** A client with no calls, or a contract with no
  operations, is a failure rather than an empty comparison.
- **It has no allowlist, on purpose.** `contract.conformance.test.ts` already
  pinned these fourteen paths exactly, as recorded debt, and that pin is why the
  suite stayed green for as long as it did. An allowlist turns a gate into a
  ledger. Either the contract gains the operation (add it, run
  `scripts/api/generate.sh`, then implement it in
  `apps/common/webhost/router.go`), or the client stops calling it.

### `go test ./webhost/` says the contract hashes to something else

`api/v1/openapi.json` was edited and the bindings were not regenerated. This is
the same failure the drift script reports, caught in the ordinary test run so a
contributor is told about it without having to know the dedicated gate exists.
Run `scripts/api/generate.sh`.

It is a digest comparison rather than a byte-for-byte one, because a test cannot
regenerate without shelling out to the generator. The full comparison, and the
only thing that also catches a hand edit to the *body* of a generated file, is
the drift script.

### `go test ./webhost/` says a field is in the handler type but not in the contract

A handler's request or response shape moved and the contract did not. This is
the **Go drift** gate. Add the field to `api/v1/openapi.json`, regenerate, and
commit; or take it back out of the handler. The failure names the operation and
the field.

The neighbouring failures from the same file mean:

- *a route the contract does not declare*: a new endpoint exists that no client
  and no adapter conformance run can see. Declare it.
- *an error code the contract does not register as a wire code*: a handler
  emits a token the UI has never heard of, so it degrades to `unknown` and every
  comparison against it silently fails. Register it, with an `x-code-origins`
  entry.
- *the contract registers a wire code no handler emits*: the opposite, a
  registry entry teaching clients to handle a failure that cannot happen.
- *differs between the generic and ugos profiles*: a runtime profile changed
  what a backup endpoint means. That is the fork this whole phase exists to
  prevent. A profile may change the auth bridge, notifications, launch behaviour
  and capability reporting, and nothing else.

### `npm test` says the shared UI does not consume the generated module

`ui/shared/src/api/contract.conformance.test.ts` checks that `contracts.ts`
re-exports the generated error-code array *by identity*, that
`PlatformCapabilities` declares exactly the capabilities the contract reports,
that every request `httpApi` makes is an operation the contract declares, and
that no file in `ui/shared/src` branches on a platform NAME instead of on
capability data.

It also checks that the hand-written list of client calls in that file covers
`httpApi` itself. Without that, a method nobody remembered to add to the list
was invisible to the whole check: it could call any path it liked and no
assertion ever saw it. The Go half never had that hole, because it enumerates
the real chi route table rather than a list somebody maintains.

### `scripts/api/selftest.sh`

Twenty-seven mutation controls, each planting one real violation in a copy of
the real tree and asserting the check fails **with the message that names the
planted reason**, plus two negative controls proving the static gates are clean
on the unmutated tree. A gate nobody has watched fail is a gate that might not
be able to.

Three of them plant the removal of a single line from `scripts/ci-local.sh`.
That script is what `.husky/pre-commit` runs, and GitHub Actions is
`workflow_dispatch`-only on this repository, so a check wired only into
`.github/workflows/ci.yml` runs on no commit at all: `check-contract-drift.sh`
therefore also asserts that `ci-local.sh` invokes all three of itself,
`check-client-paths.sh` and this self-test. A check nothing invokes cannot be
told apart from a check that does not exist.

## Compatibility and deprecation rules for `/api/v1`

Adapters, packaged targets and the shared UI all depend on this boundary now, so
what may change under `v1` and what may not is a rule rather than a judgement
call.

**Additive under `v1`, no new version needed:**

- a new operation;
- a new **optional** request field (one the server treats as absent when
  omitted);
- a new response field (clients must ignore fields they do not know);
- a new error code, provided existing codes keep their meaning and their status;
- a new value in a capability set.

**Breaking, and therefore `/api/v2` rather than a change here:**

- removing or renaming an operation, a field, or an error code;
- narrowing a type, or making an optional request field required;
- changing which HTTP status an existing failure returns;
- changing what an existing error code means;
- changing an operation's authentication, CSRF, idempotency or
  optimistic-concurrency requirements in the direction of *more* being required.

Loosening a requirement (dropping a mandatory CSRF token, say) is not a
compatibility question at all. It is a security change, and it needs the review
that goes with one, regardless of what this section says about versions.

**Deprecating without breaking:** mark the operation or field
`"deprecated": true` in the contract, keep serving it, and say in its
description what replaces it. It comes out in `v2`, never before.

`api_version` on `GET /system/version` reports which version a running instance
serves, so a client and an adapter can both check rather than assume.

## Recorded decision: the shape of `submitOperation`'s 409 body

`POST /operations` refuses with three different codes at 409, and they do not
all carry the same body. `CONFIG_REVISION_STALE` carries the current revision
in a structured top-level `config_revision` field, so a client retries against
a value it can rely on instead of one parsed out of prose (#118 item 5);
`IDEMPOTENCY_KEY_CONFLICT` and `OPERATION_ALREADY_RUNNING` go through the
ordinary error envelope and have no such field.

The 409 body is therefore `oneOf(ConfigRevisionStaleResponse, ErrorResponse)`.
Two alternatives were available and both were rejected:

- **One schema with `config_revision` optional.** Simplest to generate, and it
  throws away the #118 item 5 guarantee at the type level: every client reading
  `.config_revision` after a `CONFIG_REVISION_STALE` would be reading an
  optional field, which is the thing that guarantee exists to prevent.
- **Giving the stale case its own status.** Cleanest typing, but changing which
  HTTP status an existing failure returns is a `v2` break by this document's own
  rules above, and it is only cheap right now because nothing has consumed `v1`
  yet. Not worth spending the exception on.

`oneOf` is accepted by `scripts/api/gen-bindings.go` on an error response body
and refused everywhere else, because that is the one position from which
neither binding generates a type: both represent an error response by its status
and `x-error-codes`. A named schema using `oneOf` would be dropped from both
bindings in silence, so the generator now fails on one rather than emitting it.

## Recorded decision: live progress is a nested object that disappears

Issue #221. `Operation` is a durable, crash-safe record: submitted, started,
finished, plus a result or an error. Live transfer progress is the opposite
kind of thing, and the contract keeps the two apart rather than growing the
record a set of counters.

**Where it lives.** `OperationProgress` is held in memory by the process
executing the cycle, keyed by operation id, and it is never written to the
operations table. Writing it there would put a tick-rate write path on the one
record whose durability guarantees exist so the few writes it does take can be
trusted, and it would leave the last reading on disk for the next process to
serve as live. FR-9 lists what the journal persists, and a transfer's
instantaneous byte count is not on it.

**How a client gets it.** As an optional `progress` object on the operation
representation `GET /operations/{id}` and `GET /operations` already serve.
EPIC-B §52 settles the transport ("V1 SHALL use authenticated polling against
durable operation state") and most of the field list, and the UI polls today, so
a separate endpoint would double a client's request count to assemble one row
and a stream would be a transport the shipped client cannot consume.

**What a finished or interrupted operation reports.** No `progress` key at all.
Not `null`, and above all not an object of zeroes: an absent key is the only
encoding a client can reliably tell apart from a measurement, and a zeroed one
renders as a transfer that exists and has stalled. That covers three cases at
once: an operation that has finished, one that is queued, and one that was
running when the process died (whose reading died with it, and whose row the
startup sweep moves to failed).

**Why there is no percentage of the operation.** A run cycle is a pass over
every enabled backup set, and the artifacts it will find are discovered set by
set as it goes, so no honest denominator for the whole exists at any moment
before the end. The byte counters describe the ONE artifact being copied, and
the counters beside them (`backup_sets_done` of `backup_sets_total`,
`artifacts_done`) say where in the cycle that artifact sits. `backup_sets_total`
is exact rather than estimated: it is a count of the enabled sets in the
configuration snapshot the cycle started with. Issue #211 removed nine fields
the UI displayed that nothing computed, and a cycle-level percentage would have
been the tenth.

**Why the byte fields are `x-go-pointer`.** Zero is a real reading. A copy that
has started and moved nothing is a measured zero; `omitempty` on a plain
`int64` would encode that identically to a field nobody measured.

## Recorded decision: a placement is a copy, and absence is not presence

Issue #240 (EPIC E, FR-34). An artifact's copies reach a client as
`Artifact.placements`, and the shape is built so three different facts stay
three different facts. They all read the same if a schema is careless, and the
one that gets rendered as a green tick is the one an operator acts on.

**A row means a DURABLE copy, so no row means no copy.** A placement exists
because the journal recorded a finished copy. An artifact still transferring has
none, and the `.partial` file `local_path` names is deliberately not one. The
array is REQUIRED for the same reason: an optional array has three readings (a
copy exists, no copy exists, the server did not say), and only two of them are
ever true, so a client is always handed `[]` rather than a missing key it has to
interpret.

**A copy the journal knows is gone is not served at all.** `status` admits
`ACTIVE` and `DELETE_PENDING` and nothing else. `DELETE_PENDING` is on the wire
because the bytes are probably still there and an operator is entitled to know a
copy is on its way out. A `GONE` row would read as a copy in every layout anyone
would write for a list of copies, so it never leaves the read model.

**`unreachable` is not "gone".** `access` says whether a copy can be READ right
now, from the closed set `immediate` / `requires_restore` / `restoring` /
`unreachable`, derived only from held facts. `unreachable` means this deployment
cannot currently get to the place the copy is in, which today means the running
configuration no longer declares that medium: no bucket, no endpoint, no
credential. The product can then neither confirm the copy nor deny it, and those
two call for opposite responses. Nothing here reaches the network to answer the
question: this is rendered on every artifact list, so a network-backed answer
would either be slow or be cached, and a cached answer about reachability is a
stale answer presented as a current one.

**An unverified copy has NO `verification_class`, rather than the weakest one.**
The field is absent when nothing has verified the copy. The ladder's weakest
rung, `existence`, is a claim that an object was seen at the recorded size, and
for a copy nobody has looked at that claim is false. The enum does not admit the
empty string either, so there is no way to spell a rung named nothing.
`verified_at` is absent exactly when the class is.

**`size_bytes` is `x-go-pointer`.** Same rule as `OperationProgress`'s byte
fields above: an artifact can genuinely be zero bytes, so a recorded zero must
not encode identically to a size nobody recorded.

**Nothing is a price and nothing is an estimate.** There is no cost field and no
restore ETA on any of these schemas, because the engine has no price list, does
not know what an operator negotiated, and gets no progress report from the
provider while a restore runs. What it serves instead is the bytes, the storage
class, whether reads require a restore, and the two disclosure sentences, in the
engine's own words. `apps/common/webhost/placements_contract_test.go` holds this
structurally: it walks everything reachable from `Artifact`,
`SettingsResponse`, `UpdateSettingsRequest`, `Operation` and `RetentionPlan`
and refuses a number named for money or metered bandwidth, a field shaped like
a prediction, and any property a credential could travel under. The same file pins the closed vocabularies to the engine's constants, so
the generated TypeScript unions cannot drift from the strings the journal
stores.

## Recorded decision: on a retention verdict, no medium means local

Issue #430 (EPIC E, FR-27 and FR-30). The retention preview carries three
placement facts: `RetentionPlan.moves`, `RetentionPlan.unconfirmed_placements`,
and `medium` on every verdict. Two of the three are spelled by absence, and I
want the reason on the record rather than inferred from a diff.

**A verdict on the implicit local medium carries no `medium` key.** The obvious
alternative was to serve `"medium": "local"` everywhere, the way
`Placement.medium` already does, and I did not take it. Every deployment
written before EPIC E holds every copy locally, so that spelling would have put
a new key on every verdict of every response those deployments serve, to say
the only thing that was ever true of them. Absence says the same thing and
leaves them byte for byte as they were. `backup-manager retention` made the
same call for the same reason (`mediumSuffix`), so the two operator surfaces
now read alike instead of each having its own convention.

A move is the other way round: `from_medium` and `to_medium` are both verbatim,
`"local"` included. A verdict answers "where would this happen", which has an
implicit default, and a move answers "from where to where", which has none.

**A deployment with no storage medium carries neither placement array.** This
one is not a wire decision at all, it is `core/service` declining to compute
them, and the boundary just carries the answer through with `omitempty`. A
placement row records a DURABLE copy, so nothing ingested before FR-29's table
existed has one, and the move planner reads a missing row as "I could not
confirm where this is" rather than as absence, which is the only reading that
cannot lose data. Serve that unfiltered and the first preview after an upgrade
greets an operator with every backup they already had, listed under a heading
that reads like a fault, in a deployment where there is nowhere else a copy
could possibly be.

**The moves array is inside the plan, not behind a second call.** An apply is
confirmed against a `plan_id`, and what that id commits to has to be the whole
of what was shown. A moves section fetched separately could be rendered beside
verdicts it does not belong to. All three facts are already inside the plan's
staleness fingerprint, so an apply naming a plan whose moves have changed since
the operator read them is refused like any other stale plan.

## Recorded decision: the settings write carries a consent, not a setting

Issue #240 (FR-27). `UpdateSettingsRequest.acknowledge_medium_disclosure` is the
only field on that request that asks for no change. It permits one.

A write that newly sends a retention tier's artifacts to a non-local medium is
refused with `MEDIUM_DISCLOSURE_REQUIRED` unless it carries the acknowledgment,
and the refusal message IS the disclosure text. A client that renders the
message has therefore shown the operator the right words by construction, rather
than by keeping its own copy of them, which is the arrangement that goes stale.
The same text is also served on `SettingsSchema.storage.medium_disclosure`, so a
form can show it before the operator presses anything.

The code is its own, not `INVALID_REQUEST`, because what a client does with the
two is opposite: one is a field to fix, the other is a paragraph to put in front
of a human. And the gate is server-side because what is being consented to is
the deletion of the copy on the operator's own machine: a form that hid its Save
button would be a gate one `curl` walks around.

`acknowledge_medium_disclosure` is not counted towards a request naming
something to change. A body carrying nothing but the tick still asks for
nothing, and honouring it would rewrite the operator's configuration file and
move `config_revision`, invalidating every outstanding retention preview, for a
request with no content in it.

**The same gate stands in front of a backup set's own chain.** `PUT
/backup-sets/{source}/{set}/retention` takes the same field on
`RetentionOverride` and refuses with the same code, because an override is a
whole chain and can name a medium per tier exactly as the deployment's policy
can (the config layer resolves per-set medium references for precisely this
reason). A gate on the settings write alone would be a gate one `PUT` walks
around. What counts as "first" there is decided against the chain currently
deciding for THAT set: a set inheriting a policy that already sends `monthly`
to a medium is not asked about `monthly` again, and a set whose override sends
`daily` somewhere new is. The field is request-only in practice: it is never
written to the file and never appears on the `override` half of a response,
so a client that round-trips what it was served cannot re-consent by accident.

## Recorded decision: the two shapes `POST /system/first-run` declares

Issue #176 adds the first-run setup pair, `GET` and `POST /system/first-run`.
Both shapes below were settled when they were declared rather than after,
because a shape in this document is fixed until `v2`, and the two questions
had answers that were not obvious.

**The response is an envelope, not a flattened set.** `CompleteFirstRunResponse`
carries `backup_set` nested, which is deliberately not what
`CreateBackupSetResponse` beside it does: that one flattens `BackupSet` and adds
`operation`/`run_error` on top. The two operations take the same body, so the
divergence needs a reason, and it is this. What a first run creates is a whole
CONFIGURATION; the backup set is its first member, and `restart_required` is a
fact about the instance rather than about the set. Keeping `backup_set` as
exactly a `BackupSet` means a later `v1` addition describing the configuration
lands beside it instead of arriving as something a reader cannot tell from a
backup-set field, and it can never collide with a field `BackupSet` itself
grows. Flattening was the alternative, and it buys one decoder shared with
`createBackupSet` at the cost of both of those.

**The request omits `run_immediately`, by carrying a different schema.** The
create request is now `allOf(BackupSetSpec, { run_immediately })`, and
`POST /system/first-run` declares `BackupSetSpec` alone. The field cannot be
honoured on a first run: there is no service to submit a run to until the
configuration this call writes has been opened, which happens after the write.
Two alternatives were rejected. Declaring the shared `CreateBackupSetRequest`
and ignoring the field is the worst of them, because a request field the
contract declares and the endpoint drops is invisible to the client that sent
it. Giving the operation a standalone copy of the fifteen other fields would
work and would go stale, since nothing would hold the two lists together. The
shared base holds them together by construction: a field added to a backup set
appears on both operations or on neither.

## Migration record: what was removed and what replaced it

| removed | replaced by |
|---|---|
| the literal `API_ERROR_CODES` array in `ui/shared/src/api/contracts.ts` | a re-export of the generated array, which the contract's `ApiErrorCode` enum defines |
| `ApiErrorCode` declared in `contracts.ts` | the generated type, re-exported from the same place so no import moved |
| `WireCreatedBackupSet`, `WireBackupSet`, `WireListBackupSetsResponse`, `WireRetentionVerdict`, `WireRetentionPlan`, `WireRetentionTier`, `WireSettings` in `client.ts` | the generated `Wire*` interfaces, imported from `generated/contract` |

Nothing else in `ui/shared` changed. The `wireX`/`fromWireX` translation
convention established in Phase 3 stays exactly as it was: the generated types
describe the wire, and those helpers still map the wire onto the camelCase
domain types the rest of the app speaks. What changed is only where the wire
half comes from.

`apps/common/webhost/handlers_system.go` gained one field,
`capabilities_response.platform`, because the capability shape #166 documents
reports which platform the capabilities belong to. It is additive, and it is
explicitly not something to branch on: the UI-side check above fails on a
platform-name conditional in `ui/shared`.

## Recorded, deliberately not fixed here

#166 says an endpoint whose shape is genuinely wrong gets recorded rather than
redesigned in this issue. Three things qualify.

1. **`httpApi` calls fourteen paths the runtime does not serve.** `getVersion()`
   requests `/api/v1/version` while the runtime serves `/api/v1/system/version`,
   so the shared UI's version banner cannot ever have worked against a real
   server; `getHealth`, `listOperations`, `listActivity`, `listQuarantine`,
   `listArtifacts`, `getArtifact`, `runSet`, `setEnabled`,
   `testConnection(id)`, `revalidate`, `retryIngestion`, `scanCatalog` and
   `rebuildCatalog` are the same story. Every one of them works today only
   against `mock.ts`. They are pinned exactly in
   `UNIMPLEMENTED_CLIENT_PATHS`, so the list can only shrink and a *new*
   unbacked path fails CI on the commit that adds it (which holds only
   because the same file now asserts its call list covers `httpApi`, see
   above).

2. **Two error envelopes.** `apps/common/webhost` returns
   `{"error":{"code","message"}}` with the correlation id in a header;
   `apps/common/auth/local` returns `{"code","message","correlationId"}` flat.
   The contract records both (`ErrorResponse` and `AuthErrorResponse`) rather
   than pretending there is one. Unifying them changes what every existing
   client parses, which is a breaking change and belongs in its own issue.

3. **Two JSON naming conventions.** The `/auth` schemas are camelCase
   (`currentPassword`); everything else is snake_case. Same reasoning: recorded
   in the contract as it is, not silently normalised.

The capability response is also flat (`platform` and the five booleans as
siblings) rather than nesting the booleans under a `capabilities` key the way
#166's illustrative JSON does. Nesting would break every existing reader for no
gain the contract needs, and #166 forbids reshaping endpoints beyond what the
contract requires.
