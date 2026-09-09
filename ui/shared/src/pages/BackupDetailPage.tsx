/**
 * One artifact: what it is, what happened to it, and where its bytes
 * actually are.
 *
 * The page is three panels and the third is the one with a rule attached.
 * Copies are described by PlacementList, and the sentences explaining the
 * verification ladder and what a retrieval costs are fetched from the
 * service rather than written here, because those words are what an
 * operator reads while deciding whether a backup is safe, and a paraphrase
 * kept in a frontend eventually says something the engine does not.
 *
 * Both fetches are page-local rather than on the shared graph, and each
 * says why at the call site. The artifact one is the more interesting: it
 * changes identity on every navigation, so the stale-data-while-loading
 * behaviour that is correct for a singleton resource would show one
 * artifact's fields under another artifact's URL.
 */
import { useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import { useAsync } from "@shared/hooks/useAsync";
import { PageHeader } from "@shared/components/PageHeader";
import { StatusBadge } from "@shared/components/StatusBadge";
import { RetentionBadges, RetentionPolicyBadge } from "@shared/components/RetentionBadge";
import { LifecycleTimeline } from "@shared/components/LifecycleTimeline";
import { PlacementList } from "@shared/components/PlacementList";
import { ErrorState } from "@shared/components/EmptyState";
import { bytes, stamp } from "@shared/utilities/format";
import { BackupManagerError, RequestFailure } from "@shared/api/contracts";
import type { ApiErrorCode } from "@shared/api/contracts";
import type { ArtifactRetentionPolicy, BackupArtifact } from "@shared/types/backup";

export function BackupDetailPage({ readOnly = false }: { readOnly?: boolean }) {
  const { artifactId = "" } = useParams();
  const api = useApi();
  const navigate = useNavigate();
  // The outcome of the recovery press below, so a rejected call has a
  // visible answer instead of a silent no-op. Same shape and same reason
  // as QuarantinePage's, which learned it the same way.
  const [recovery, setRecovery] = useState<{ tone: "ok" | "bad"; text: string } | null>(null);
  const [retrying, setRetrying] = useState(false);
  // Page-local, like the sibling BackupSetDetailPage (mandatory review on
  // #144): nothing else reads this particular artifact, so there is no
  // duplicate fetch to eliminate by putting it on the shared graph, and
  // going through App.tsx's app-wide resource mechanism actively hurt here
  // — this "resource" changes identity on every navigation to a different
  // :artifactId, so the loading transition that correctly preserves stale
  // data for a genuinely singleton resource instead let one artifact's
  // fields render under a different artifact's URL while the new fetch was
  // in flight.
  const artifact = useAsync(() => api.getArtifact(artifactId), [api, artifactId]);
  // The verification ladder and the retrieval disclosure, in the backend's
  // own words (GET /settings). Fetched here rather than transcribed into
  // PlacementList, because those sentences are what an operator reads
  // while deciding whether a backup is safe, and a paraphrase kept in a
  // frontend is a paraphrase that eventually says something the engine
  // does not. It is deliberately NOT gated on: if it fails, the copies
  // still render and the explanatory sentences are simply absent, which
  // is a worse page and not a wrong one.
  //
  // Page-local rather than a shared graph node, matching the retention
  // card, which fetches the same document the same way. Settings IS a
  // singleton and would eventually belong on the graph, but putting it
  // there means App.tsx owning a fetch and a poll for it, and this page
  // wants the answer once, at open, and does not care if it goes stale
  // while somebody reads one backup's detail. When a third reader
  // appears, that is the change to make.
  const settings = useAsync(() => api.getSettings(), [api]);

  if (artifact.error) return <ErrorState {...artifact.error} onRetry={artifact.reload} />;
  // Both checks matter: `data` is null only before the first successful
  // fetch, but navigating list -> artifact A -> back -> artifact B (or any
  // browser back/forward between two previously visited artifact URLs)
  // keeps this component mounted and re-runs this same useAsync with a new
  // artifactId, so `loading` flips back to true while `data` still holds
  // the PREVIOUS artifact until the new fetch resolves. Gating on loading
  // too closes that window instead of rendering stale fields under the new
  // URL.
  if (!artifact.data || artifact.loading) return null;

  const a = artifact.data;

  // Issue #662, in the browser. A quarantined backup gets three verbs on
  // the Quarantine page; a FAILED one had none, anywhere, and
  // `retryFailedIngestion` was declared in the contract and implemented
  // in the client with nothing in src/pages, src/components or src/hooks
  // calling it. The product's own premise is a NAS with no shell, so a
  // backup the browser cannot act on is a backup nobody can act on.
  //
  // What "stuck" is, and why it is read off the state rather than off the
  // two verdict fields.
  //
  // The first shape of this gate was `validation === "failed" &&
  // quarantine === null`, and it never fired on the artifact #662 is
  // about. A backup that failed an attempt carries no validation verdict
  // at all: nothing on that path writes one, so the wire reports
  // `validation: "pending"` (core/service/artifacts.go maps a nil verdict
  // to pending), and the conjunction was close to unsatisfiable besides —
  // the branch that DOES record a failed verdict is the one that drives
  // the artifact to QUARANTINED, where the record is non-null. So the
  // control rendered for a fixture and for no real backup.
  //
  // FAILED is the fact this card is for, and it is one field. It is the
  // state whose two exits are both operator-only (see
  // core/internal/lifecycle/state.go): no cycle will attempt this backup
  // again on its own, and the Quarantine page will never list it, because
  // it is not quarantined. Comparing against the one state this page has
  // a behaviour for leaves every other state alone, which is what a
  // client that does not own the lifecycle graph should do.
  const stuck = a.state === "FAILED";

  const retryIngestion = () => {
    setRecovery(null);
    setRetrying(true);
    api
      .retryFailedIngestion(a.id)
      .then(() => {
        // Accepted, not recovered, and the difference is measurable: the
        // CLI's `retry` exits 0 and prints "re-entering the pipeline" for
        // a backup whose very next cycle lands FAILED again on the same
        // FR-12 collision (measured in a container against a real rbm).
        // The verb's success says the row moved to DISCOVERED and nothing
        // about what happens next, so this sentence must not read as "it
        // is fixed", and the page goes and looks rather than asserting an
        // outcome the response does not carry.
        setRecovery({
          tone: "ok",
          text:
            "Re-attempted: the service accepted the request and has not carried it out yet, so what this page " +
            "shows below is the state the next attempt leaves. If its local copy was already good and the retry " +
            "keeps meeting a final-name collision, that retry verifies the copy against the remote object and " +
            "trusts it in place. While this card is still here, the backup is still FAILED."
        });
        artifact.reload();
      })
      .catch((e: unknown) => setRecovery({ tone: "bad", text: retryRefusalSentence(e) }))
      .finally(() => setRetrying(false));
  };

  return (
    <>
      <PageHeader
        back={{ label: "Backups", onClick: () => navigate("/backups") }}
        title={
          <span style={{ display: "inline-flex", alignItems: "center", gap: 12, flexWrap: "wrap" }}>
            <span className="mono" style={{ fontSize: 19 }}>{a.filename}</span>
            <StatusBadge
              tone={a.validation === "verified" ? "ok" : "danger"}
              icon={a.validation === "verified" ? "success" : "failure"}
            >
              {a.validation === "verified" ? "Verified" : "Failed"}
            </StatusBadge>
            {/* One or the other, never both (issue #523). A backup whose
                set was removed still has the tiers the journal last
                recorded, and showing them here would say a chain is
                keeping it when no chain will ever look at it again. */}
            {a.retentionPolicy === "configured" ? (
              <RetentionBadges classes={a.retentionClasses} />
            ) : (
              <RetentionPolicyBadge policy={a.retentionPolicy} />
            )}
          </span>
        }
      />

      <div style={{ display: "grid", gridTemplateColumns: "minmax(0, 1fr) minmax(0, 1fr)", gap: 14, alignItems: "start" }}>
        <section className="card">
          <div className="card__header"><h2 className="eyebrow">Artifact</h2></div>
          <dl
            style={{
              margin: 0, padding: "15px 18px", display: "grid",
              gridTemplateColumns: "150px 1fr", gap: "11px 14px", fontSize: "var(--text-sm)"
            }}
          >
            <Row label="Artifact ID" value={a.id} mono />
            <Row label="Backup set" value={a.setName} />
            <Row label="Remote original" value={a.remoteOriginalPath} mono />
            {/* The ingestion landing path, labelled as what it is. It is not
                evidence that a readable file is sitting there, and the Copies
                card below is what answers "where are the bytes". */}
            <Row label="Ingestion path" value={a.localPath} mono />
            <Row label="Producer timestamp" value={stamp(a.producedAt)} mono />
            <Row label="Received timestamp" value={stamp(a.receivedAt)} mono />
            <Row label="Size" value={bytes(a.sizeBytes) + " \u00b7 " + a.sizeBytes + " B"} mono />
            <Row label="Checksum" value={a.checksumAlgorithm + ":" + a.checksum} mono />
            {/* The verdict, and separately what to do about it. They were
                one ternary and it printed "Failed" for a backup whose
                validation had never run, which is the #662 artifact
                exactly: nothing records a verdict on the collision path,
                so this field is "pending" while the state is FAILED. A
                row labelled "Validation result" that reports a failure
                nothing measured is the same class of untruth as the card
                below not rendering at all. */}
            <Row label="Validation result" value={validationResultSentence(a, stuck)} />
            <Row label="Retention classes" value={a.retentionClasses.join(", ") || "unclassified"} />
            {/* Spelled out in the field list as well as badged in the
                header, because this row is the sentence an operator can
                act on: it says what happens to the file, and what to do
                if that is not what they want. */}
            <Row label="Retention policy" value={retentionPolicySentence(a.retentionPolicy)} />
            <Row
              label="Remote source removed"
              value={a.remoteSourceRemovedAt ? stamp(a.remoteSourceRemovedAt) + " (after commit)" : "No — original retained"}
            />
          </dl>
        </section>

        <section className="card">
          <div className="card__header"><h2 className="eyebrow">Lifecycle</h2></div>
          <LifecycleTimeline artifact={a} />
          <p style={{ margin: 0, padding: "0 22px 20px", fontSize: "var(--text-sm)", color: "var(--text-3)", maxWidth: "60ch" }}>
            Remote deletion is a lifecycle consequence of a proven NAS copy — never
            an independent file operation.
          </p>
        </section>
      </div>

      {/* #662: the one control this page was missing. It is its own card
          rather than a button in the header, because the sentence beside
          it is half of the remedy: an operator meeting a FAILED backup
          has been told to intervene and never told with what. */}
      {stuck ? (
        <section className="card" style={{ marginTop: 14 }}>
          <div className="card__header"><h2 className="eyebrow">Recovery</h2></div>
          <div style={{ padding: "15px 18px", display: "grid", gap: 10, fontSize: "var(--text-sm)" }}>
            <p style={{ margin: 0, color: "var(--text-2)", maxWidth: "72ch" }}>
              This backup failed an attempt and is not quarantined, so no cycle will attempt it again on its own
              and the Quarantine page will not list it. Re-attempting it is the way back.
            </p>
            <span style={{ display: "flex", gap: 7 }}>
              <button className="btn btn--sm" disabled={readOnly || retrying} onClick={retryIngestion}>
                {retrying ? "Retrying…" : "Retry ingestion"}
              </button>
            </span>
            {recovery ? (
              <p
                role="status"
                style={{ margin: 0, maxWidth: "72ch", color: recovery.tone === "ok" ? "var(--text-2)" : "var(--danger)" }}
              >
                {recovery.text}
              </p>
            ) : null}
          </div>
        </section>
      ) : null}

      <div style={{ marginTop: 14 }}>
        <PlacementList placements={a.placements} storage={settings.data?.schema.storage} />
      </div>
    </>
  );
}

/**
 * What this card says when the retry did not go through.
 *
 * It rendered `e.message` bare, which fails the operator in the exact way
 * #662 complains about the docked terminal: a control that refuses and
 * explains nothing. Three things are added and none of them is invented.
 *
 * The correlation id, whenever the response carried one. It is the only
 * token that ties what an operator saw to what the engine logged, and the
 * position #662 was filed from — a NAS with no shell — is exactly the
 * position where quoting that id into an issue is all somebody can do.
 *
 * A next step for the two refusals whose own message cannot carry one.
 * ARTIFACT_NOT_FAILED means the row moved while this page was open (a
 * cycle, another browser, or the operator's own earlier press): the page
 * is stale rather than wrong, and reloading is the whole remedy. An
 * authentication or CSRF refusal is about the session and not about the
 * backup at all, so it must not read as a verdict on the backup.
 *
 * And the two non-refusals RequestFailure labels, kept apart, because
 * "nothing came back" leaves it genuinely unknown whether the retry was
 * carried out, and telling somebody to press again when it may already
 * have run is advice with a cost.
 *
 * # There is deliberately no destructive-gate branch
 *
 * POST /api/v1/backups/{source}/{set}/{name}/retry carries requireCSRF
 * and NOT requireDestructiveGate (router.go:425), the generated contract
 * records `destructiveGate: false` for `retryFailedIngestion`, and
 * webhost's own contract walk asserts that a closed production gate must
 * NOT refuse an operation declared ungated. So this button does not 403
 * on the shipped NotYetImplementedGate, DESTRUCTIVE_OPERATIONS_DISABLED
 * is not among the codes this route can emit, and a branch explaining
 * issue #92 here would be this page inventing a refusal the server never
 * sends — the same species of untruth as the fixture that used to gate
 * the card above.
 */
function retryRefusalSentence(e: unknown): string {
  if (e instanceof BackupManagerError) {
    const step = RETRY_REFUSAL_NEXT_STEP[e.api.code] ?? "";
    return [e.api.message, step, correlationSuffix(e.api.correlationId)].filter((part) => part !== "").join(" ");
  }
  if (e instanceof RequestFailure) {
    const said =
      e.kind === "no-response"
        ? "The backup service did not answer, so whether the re-attempt was started is unknown. Reload this page" +
          " before pressing again: if the backup is no longer FAILED, it did start."
        : "The backup service answered with something this build could not read, so what happened to the" +
          " re-attempt is unknown. Reload this page to see the backup's current state.";
    return [said, correlationSuffix(e.correlationId)].filter((part) => part !== "").join(" ");
  }
  // Not a refusal and not a request failure, so it did not come out of the
  // request at all (RequestFailure's own doc makes that distinction the
  // point of the type): it came out of what was chained onto it, and there
  // is nothing honest to add to it.
  return e instanceof Error ? e.message : String(e);
}

/** The refusals whose own sentence cannot say what to do next, because
 *  what to do next is about this page or this session rather than about
 *  the backup. Every other code is reported in the engine's own words
 *  alone, which is the house rule: a paraphrase kept in a frontend
 *  eventually says something the engine does not. */
const RETRY_REFUSAL_NEXT_STEP: Partial<Record<ApiErrorCode, string>> = {
  ARTIFACT_NOT_FAILED:
    "This backup is no longer FAILED, so this page is showing a state it has already left. Reload it to see" +
    " where the backup is now.",
  UNAUTHENTICATED: "This is about the session, not about the backup. Sign in again and the re-attempt is still available.",
  CSRF_TOKEN_MISSING: "This is about the session, not about the backup. Reload the page and press again.",
  CSRF_TOKEN_MISMATCH: "This is about the session, not about the backup. Reload the page and press again."
};

/** The correlation id as a clause, or nothing at all when the response
 *  carried none. Never a placeholder: an id an operator quotes has to be
 *  one the engine's log actually contains (the rule fromWireTransition
 *  states for the same field one file over). */
function correlationSuffix(correlationId: string | undefined): string {
  return correlationId ? "Correlation id " + correlationId + "." : "";
}

/**
 * The Validation result row: the verdict this backup actually carries,
 * plus where to go about it.
 *
 * Three verdicts and not two. Before #662 this row was
 * `verified ? "Checksum passed" : "Failed ..."`, which reports a failure
 * for "pending" — and pending is not a rare case, it is what the wire
 * says for every backup no validator has ruled on, including the FAILED
 * one this whole page's Recovery card exists for. "Nothing has validated
 * this" and "something validated this and rejected it" are different
 * facts with different next steps, and the reassuring-looking collapse
 * was the one that hid the artifact the issue was filed about.
 *
 * The pointer is appended rather than substituted for the verdict, for
 * the same reason: where to go is not a verdict.
 */
function validationResultSentence(a: BackupArtifact, stuck: boolean): string {
  const verdict =
    a.validation === "verified"
      ? "Checksum passed"
      : a.validation === "failed"
        ? "Failed"
        : "Not recorded — nothing has validated this backup";
  if (stuck) {
    return verdict + " · this backup is FAILED and not quarantined, so it is not on the Quarantine page: see Recovery below";
  }
  if (a.quarantine) return verdict + " · see Quarantine";
  return verdict;
}

/**
 * The one-line answer to "what will eventually delete this backup".
 *
 * The governed case names the chain in the vocabulary the rest of the page
 * already uses rather than repeating the tier badges above it. The other
 * two say the consequence: a backup under no policy is one nothing will
 * ever delete, so it holds its space until somebody acts, and a server
 * that did not answer leaves this page unable to say which of the two is
 * true, which is a gap rather than reassurance.
 */
function retentionPolicySentence(policy: ArtifactRetentionPolicy): string {
  switch (policy) {
    case "configured":
      return "This backup set's retention chain decides when this is deleted.";
    case "none":
      return (
        "None. This backup set's configuration was removed, so no retention chain selects or expires this" +
        " backup: nothing here will ever delete it. Create the backup set again to put it back under a" +
        " policy, or remove the file yourself."
      );
    default:
      return (
        "This server did not say, so this page cannot tell you whether anything will ever delete this" +
        " backup. Updating Backup Manager restores the answer; the rbm unconfigured command" +
        " has it in the meantime."
      );
  }
}

function Row({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <>
      <dt style={{ color: "var(--text-2)" }}>{label}</dt>
      <dd
        className={mono ? "mono" : undefined}
        style={{ margin: 0, wordBreak: "break-all" }}
      >
        {value}
      </dd>
    </>
  );
}
