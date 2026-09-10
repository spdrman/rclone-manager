/**
 * What one manifest-declared value may be, checked in the browser
 * (issue #669; the declarations are #665's, EPIC I #664).
 *
 * # These are the engine's rules, not a second, stricter set
 *
 * Every function here mirrors `backend.validateFieldValue`
 * (core/internal/backend/validate.go:246) rule for rule, and that is the
 * whole design constraint. A browser check exists so an operator is told
 * which box is wrong before a request goes out, which is worth having;
 * it is worth nothing if it refuses something the engine would have
 * accepted, because then the product has two definitions of a legal path
 * and the stricter one is the one nobody wrote down. So a rule here is a
 * transcription and never an improvement, and where the two could drift
 * the engine is the one that decides: it validates again on the way in
 * (`Registry.ValidateInstance`) and its refusal is the one that counts.
 *
 * # A message names the field and the shape, never the value
 *
 * `mediumcreds.go`'s doctrine, quoted by validate.go's own docblock:
 * refusals by SHAPE, never by content. Not one message below
 * interpolates what the operator typed - not to be tidy, but because
 * these sentences end up in screenshots and support threads, and a
 * message that quotes the value back is a message that will one day
 * quote back a bucket name, a host path, or the contents of a box
 * somebody pasted a secret into by mistake.
 *
 * # A credential holds no value to check
 *
 * `fieldProblem` answers null for a KindCredential field, always, and
 * `emptyValues` gives it no entry. That is not a gap: credential
 * material never enters the value bag at all (see ManifestFields, which
 * routes it to its own callback), and what the engine eventually
 * validates is an opaque REFERENCE this deployment minted, which the
 * browser exchanges the material for and never inspects.
 */
import type { BackendManifest, BackendManifestField } from "@shared/api/contracts";

/**
 * One instance's collected values, keyed by manifest field id.
 *
 * A bool arrives as a boolean because a checkbox has no empty state;
 * everything else arrives as the string that was typed. Nothing else can
 * be in here, and in particular no credential material: see this
 * module's docblock.
 */
export type ManifestFieldValues = Record<string, string | boolean>;

/**
 * A fresh value bag for a manifest: empty, and with no entry at all for
 * a credential field.
 *
 * The absence is the point rather than a convenience. A bag with an
 * empty string under the credential's id is a slot, and a slot is
 * somewhere a later caller can put material without changing a type; a
 * missing key means the review step has nothing to render even if it
 * tried.
 */
export function emptyValues(manifest: BackendManifest): ManifestFieldValues {
  const values: ManifestFieldValues = {};
  for (const field of manifest.fields) {
    if (field.kind === "credential") continue;
    values[field.id] = field.kind === "bool" ? false : "";
  }
  return values;
}

/** The one credential field a manifest may declare, if it declares one.
 *  Validation refuses a manifest with two (validate.go:121), so callers
 *  never have to handle more than one - the same reason
 *  Manifest.CredentialField exists on the Go side. */
export function credentialField(manifest: BackendManifest): BackendManifestField | undefined {
  return manifest.fields.find((f) => f.kind === "credential");
}

/**
 * What is wrong with one value, in a sentence, or null when nothing is.
 *
 * The kind switch is exhaustive and deliberately has no `default`. An
 * eighth kind on the wire arrives here as a compile error naming this
 * function, rather than as a value that quietly validates: a field
 * nobody wrote a rule for must not be able to read as a field that
 * passed. That is MediumPreflightChecks.mark's argument, applied to the
 * other closed vocabulary this screen renders.
 */
export function fieldProblem(field: BackendManifestField, raw: string | boolean): string | null {
  if (field.kind === "credential") return null;
  if (field.kind === "bool") return null;

  const value = typeof raw === "string" ? raw : "";
  if (value === "") {
    return field.required ? "required: this backend cannot be configured without it" : null;
  }

  switch (field.kind) {
    case "path":
      return absolutePathProblem(value);

    case "url":
      return urlProblem(value);

    case "key_prefix":
      return keyPrefixProblem(value);

    case "enum":
      // The declared choices are the schema's own words, so naming them
      // back is not echoing anything an operator typed. A UI that got
      // here has a bug anyway - a <select> cannot produce a value it
      // was not given - but the value bag is also filled from a saved
      // instance, and a manifest that dropped a choice its own
      // destinations were configured with is exactly the case worth a
      // sentence rather than a silent pass.
      return (field.values ?? []).some((v) => v.value === value)
        ? null
        : "not one of the choices this backend declares: " +
            (field.values ?? []).map((v) => v.value).join(", ");

    case "string":
      return stringPatternProblem(field, value);
  }

  const unhandled: never = field.kind;
  throw new Error("unhandled manifest field kind: " + String(unhandled));
}

/** Every field with a problem, keyed by field id, and nothing for the
 *  fields that are fine. A caller renders it per field and gates its
 *  own Next button on it being empty. */
export function manifestProblems(
  manifest: BackendManifest,
  values: ManifestFieldValues
): Record<string, string> {
  const problems: Record<string, string> = {};
  for (const field of manifest.fields) {
    const problem = fieldProblem(field, values[field.id] ?? "");
    if (problem !== null) problems[field.id] = problem;
  }
  return problems;
}

/**
 * The `values` a configuration request carries, from what the form
 * collected.
 *
 * Two rules, and both of them are refusals rather than repairs.
 *
 * An entry the manifest does not declare THROWS, naming the field id.
 * The engine refuses the same thing on the way in
 * (`Registry.ValidateInstance` walks the bag and refuses a key no field
 * declares), and the browser could instead have quietly dropped it - but
 * a bag with a stale key in it means the form and the manifest have come
 * apart, which happens when a manifest changes under a page that was
 * already open. Dropping it saves something the operator did not
 * describe and the review screen did not show, which is the "verifies
 * green, then saves something slightly different" defect
 * StorageMediumSpec's own docblock says it exists to prevent. So it is
 * loud, and it stays loud even now that the wire can carry every
 * declared field: it costs nothing and it is what catches the next field
 * somebody adds to a manifest and forgets to plumb.
 *
 * An UNSET optional field is omitted rather than sent as an empty
 * string. `unset_means` is resolved by an accessor at read time, so
 * sending the resolved value would freeze a product default into the
 * operator's file at the next save (#294), and sending "" would be a
 * caller asserting emptiness where it means "nobody said".
 *
 * A bool goes out as "true" or "false", which is what
 * `backend.validateFieldValue`'s KindBool case reads, and a credential
 * field contributes nothing: material travels in its own reference
 * object and never in this bag.
 */
export function configurationValues(
  manifest: BackendManifest,
  values: ManifestFieldValues
): Record<string, string> {
  const declared: Record<string, true> = {};
  for (const field of manifest.fields) declared[field.id] = true;
  for (const key of Object.keys(values)) {
    if (!declared[key]) {
      throw new Error(
        `the ${manifest.id} backend declares no field ${key}, so this configuration cannot be sent; ` +
          "reload the page to pick up the manifest this manager is running"
      );
    }
  }

  const out: Record<string, string> = {};
  for (const field of manifest.fields) {
    if (field.kind === "credential") continue;
    const value = values[field.id];
    if (field.kind === "bool") {
      out[field.id] = value === true ? "true" : "false";
      continue;
    }
    if (typeof value === "string" && value !== "") out[field.id] = value;
  }
  return out;
}

/**
 * `filepath.IsAbs` plus `filepath.Clean` equality plus the no-dot-segment
 * rule, which is validate.go:253's three checks and its one sentence.
 *
 * Written as a segment walk rather than as a JS re-implementation of
 * Clean, because the only thing Clean can disagree with here is a
 * doubled, trailing or dot segment, and those are exactly what the walk
 * refuses. A second cleaning algorithm would be a place for the two
 * languages to differ.
 */
function absolutePathProblem(value: string): string | null {
  const bad = 'an absolute path with no ".", ".." or empty segment, and no trailing "/"';
  if (!value.startsWith("/")) return bad;
  if (value === "/") return null;
  const segments = value.split("/").slice(1);
  return segments.some((s) => s === "" || s === "." || s === "..") ? bad : null;
}

/** validate.go:269: an http or https URL, with a host, carrying no
 *  credentials. `new URL` throws on anything without a scheme, which is
 *  the same refusal Go reaches by parsing and finding no host. */
function urlProblem(value: string): string | null {
  const bad = "an http or https URL, with a host, and with no username or password in it";
  let parsed: URL;
  try {
    parsed = new URL(value);
  } catch {
    return bad;
  }
  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") return bad;
  if (parsed.hostname === "") return bad;
  if (parsed.username !== "" || parsed.password !== "") return bad;
  return null;
}

/**
 * config.validateMediumPrefix's four rules, via
 * backend.validateKeyPrefix: no leading or trailing "/", no empty
 * segment, no "." or ".." segment.
 *
 * The last of those is not tidiness. Restoring an artifact writes it to
 * a local path derived from its key, so a ".." segment in a key
 * namespace is a path escape on this host - KindKeyPrefix's own comment
 * in manifest.go makes that argument and cites the config rule it came
 * from.
 */
function keyPrefixProblem(value: string): string | null {
  if (value.startsWith("/") || value.endsWith("/")) {
    return 'no leading or trailing "/": the key layout joins with "/" already, so a slash here produces an empty key segment';
  }
  for (const segment of value.split("/")) {
    if (segment === "") return 'no empty segment ("//")';
    if (segment === "." || segment === "..") {
      return 'no "." or ".." segment: a key becomes a local path when an artifact is restored from it';
    }
  }
  return null;
}

/**
 * The manifest's own RE2, compiled here.
 *
 * The engine refused a pattern that does not compile when it loaded the
 * manifest, so this one has already been proven - by Go's regexp, on
 * RE2 syntax. JavaScript's engine is not RE2, and the small set of
 * patterns it rejects that RE2 accepts would otherwise become a field an
 * operator cannot fill in with no explanation anywhere. So a pattern
 * this browser cannot compile says so, in those words, and is not
 * treated as a value that failed: the difference matters, because one is
 * the operator's problem and the other is this product's.
 */
function stringPatternProblem(field: BackendManifestField, value: string): string | null {
  if (!field.pattern) return null;
  let re: RegExp;
  try {
    re = new RegExp(field.pattern);
  } catch {
    return "this backend declares a shape for this field that this browser cannot check; the manager will check it on save";
  }
  return re.test(value) ? null : "not the shape this backend accepts for this field";
}
