/**
 * The review step (issue #669, artboard `ConfigReview`).
 *
 * Three questions, in this order: what is about to be written, whether
 * it was proven, and whether this becomes the destination new tiers
 * start on.
 *
 * # There is no code path here that prints a credential
 *
 * That is the property, and it is structural rather than careful. A
 * KindCredential field renders a fixed sentence chosen by its declared
 * KIND, and nothing in this file reads a credential field's value - so
 * the assertion holds even when the value bag it is handed has material
 * in it, which is how manifest-review.test.tsx writes it. #665's C1-C5
 * keep material out of config.yaml, out of the echoed command line and
 * out of every API response; none of them can see a React component, and
 * "held in state, rendered by a later step" is the one remaining way to
 * leak an access key in this product. Refusals by shape, never by
 * content - the same doctrine mediumcreds.go argues on the server, in
 * the place the browser could still have got it wrong.
 *
 * # An unset field says what unset will mean, and does not pretend it
 * was chosen
 *
 * `unset — read as STANDARD` and not `STANDARD`. The first is true and
 * the second is a claim: `unset_means` is what an ACCESSOR resolves an
 * empty field to at read time, and printing it as the value turns a
 * default this product can change into one the operator appears to have
 * picked - which the next settings save then freezes into their file.
 * That is #294's argument, and the manifest carries `unset_means` as
 * data precisely so a surface can say the honest version.
 *
 * # The default is offered, never taken
 *
 * A checkbox with the consequence written next to it, naming the
 * destination that stops being the default and therefore becomes
 * removable. Never a side effect of saving: #669's own line. It is not
 * offered at all when this destination is already the default, because
 * the consequence sentence would have to name this destination as the
 * thing that stops being it, which is false.
 */
import type { BackendManifest, BackendManifestField } from "@shared/api/contracts";
import { Banner } from "@shared/components/Banner";
import type { ManifestFieldValues } from "@shared/components/manifest/manifestFieldRules";

export function ManifestReview({
  manifest,
  destinationId,
  values,
  credentialStored,
  verified,
  makeDefault,
  currentDefault,
  onMakeDefaultChange
}: {
  manifest: BackendManifest;
  destinationId: string;
  values: ManifestFieldValues;
  /** Whether a credential has been stored for this instance. A boolean
   *  and never the reference, let alone the material: what a reviewer
   *  needs to know is that the step happened. */
  credentialStored: boolean;
  verified: boolean;
  makeDefault: boolean;
  /** The destination new tiers start on today, or null when this
   *  deployment has none. Needed to name what stops being it, which is
   *  the consequence half of the offer. */
  currentDefault: string | null;
  onMakeDefaultChange(next: boolean): void;
}) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
      <div style={{ display: "flex", flexDirection: "column", gap: 2 }}>
        {manifest.fields.map((field) => (
          <div
            key={field.id}
            data-testid={`review-row-${field.id}`}
            data-field={field.id}
            style={{ display: "flex", gap: 10, alignItems: "baseline", fontSize: 12.5 }}
          >
            <span data-testid="review-label" style={{ width: 190, color: "var(--text-2)" }}>
              {field.label}
            </span>
            <span data-testid="review-value" className="mono" style={{ flex: 1 }}>
              {reviewValue(field, values, credentialStored)}
            </span>
          </div>
        ))}
      </div>

      {/* Not dismissible either way (#620). This is the sentence saying
          whether anything was proven, sitting above the button that
          writes the destination; #636's guarantee is that an unverified
          destination stays distinguishable from a proven one, and a mark
          an operator can put away is a mark that is not there. */}
      <div data-testid="review-verification">
        <Banner tone={verified ? "info" : "warn"} dismissible={false} style={{ fontSize: 12.5 }}>
          {verified
            ? "Verified: an object was written to this destination, read back byte for byte, and deleted."
            : "Not verified: nothing has been written to this destination and read back. Saving now leaves it marked unverified until a check passes."}
        </Banner>
      </div>

      {currentDefault === destinationId ? null : (
        <label
          data-testid="review-default"
          style={{ display: "flex", gap: 8, alignItems: "baseline", fontSize: 12.5 }}
        >
          <input
            type="checkbox"
            checked={makeDefault}
            onChange={(e) => onMakeDefaultChange(e.target.checked)}
          />
          <span style={{ maxWidth: "74ch" }}>
            Make this the default destination for newly created retention tiers.{" "}
            {currentDefault === null
              ? "This deployment has no default today, so nothing stops being one."
              : `${currentDefault} stops being the default and becomes removable, which it is not while it holds that role.`}{" "}
            No existing tier is rewritten and no backup moves.
          </span>
        </label>
      )}
    </div>
  );
}

/**
 * One field's value, as words.
 *
 * The credential case is first and returns before anything reads a
 * value, which is the whole point of the ordering: there is no path
 * through this function that reaches `values[field.id]` for a credential
 * field, so a caller who wires the bag up differently still cannot turn
 * this screen into a leak.
 */
function reviewValue(
  field: BackendManifestField,
  values: ManifestFieldValues,
  credentialStored: boolean
): string {
  if (field.kind === "credential") {
    return credentialStored ? "stored, and never shown again" : "none";
  }

  const value = values[field.id];
  if (field.kind === "bool") return value === true ? "yes" : "no";
  if (typeof value !== "string" || value === "") {
    return field.unsetMeans ? `unset — read as ${field.unsetMeans}` : "unset";
  }
  return value;
}
