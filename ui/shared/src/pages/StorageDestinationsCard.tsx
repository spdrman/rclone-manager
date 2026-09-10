/**
 * The storage destinations an operator owns: the list, the verify button,
 * the removal, and the FR-30 report when a destination artifacts already
 * live on stops answering (G2.2, issue #594).
 *
 * # Adding one is two wizards, in order (EPIC I, #664)
 *
 * "Add a destination" opens AddDestinationWizard: choose a registered
 * backend, name this instance, confirm. It writes nothing. What it hands
 * back is a backend id and an instance name, and the CONFIGURE step that
 * follows collects the values, proves them, and performs the one create.
 *
 * The configure step here is still S3DestinationWizard, which is
 * S3-shaped: issue #669's manifest-driven renderer replaces it, and the
 * two land as a pair, which is also what makes a local volume
 * configurable from this card at all. Until then the id chosen in step 2
 * is passed to it as `presetId` rather than asked for twice.
 *
 * # What FR-30 makes this card responsible for
 *
 * FR-30's invariant is that at no instant may an artifact have no
 * confirmed readable copy. Two things on this card could break it by
 * accident, and both are handled here rather than left to the backend to
 * refuse quietly.
 *
 * A failed re-verification is a REPORT and never a state change. Nothing
 * is deleted and nothing is marked lost. The affected copies read as
 * unreachable, which is the word the engine already produces for a medium
 * the configuration cannot reach and which means "this deployment cannot
 * confirm or deny the copy", emphatically not "the copy is gone". No prune
 * runs against them, and no source copy anywhere is reclaimed on the
 * strength of a placement that could not be confirmed. The engine already
 * declines to delete a source it cannot confirm; this card's job is to say
 * so on screen instead of letting it be silent.
 *
 * And the banner names the count AND lists the backup sets, because "148
 * copies affected" with no list is not something an operator can act on.
 *
 * Editing a destination stays available while it is failing, deliberately:
 * a credential that expired is exactly the thing an operator needs to be
 * able to fix. Removing it is refused while any placement names it, and
 * the refusal says how many.
 *
 * # Every action names its CLI equivalent
 *
 * EPIC G's standing rule. The command lines come from
 * storageDestinationCommands.ts, which is also where the argument for why
 * none of them can carry a secret lives. G1.2's global terminal will print
 * them; until it exists they are shown beside the control that produces
 * them, which is the same string either way.
 */
import { useState } from "react";
import { useApi } from "@shared/api/ApiContext";
import type {
  ApiError,
  BackendCatalog,
  MediumPreflight,
  StorageMedium,
  StorageMediumUsage
} from "@shared/api/contracts";
import { useAsync } from "@shared/hooks/useAsync";
import { Banner } from "@shared/components/Banner";
import { ErrorState } from "@shared/components/EmptyState";
import { ConfirmationDialog } from "@shared/components/ConfirmationDialog";
import { apiErrorOf, isNotConfigured } from "@shared/api/failure";
import { AddDestinationWizard } from "@shared/pages/AddDestinationWizard";
import { S3DestinationWizard } from "@shared/pages/S3DestinationWizard";
import { DestinationConfigureWizard } from "@shared/pages/DestinationConfigureWizard";
import { MediumPreflightChecks } from "@shared/pages/MediumPreflightChecks";
import { CommandEcho } from "@shared/pages/CommandEcho";
import {
  removeCommand,
  setDefaultCommand,
  showCommand,
  testConnectionCommand
} from "@shared/pages/storageDestinationCommands";
import { localDriveDescription } from "@shared/pages/retentionChain";

export function StorageDestinationsCard({
  readOnly,
  onChanged
}: {
  readOnly: boolean;
  /** Called after anything on this card changes the destinations: a
   *  declaration added, edited or removed, and the default moved.
   *
   *  It exists because the retention chain editor beside this card holds
   *  its OWN read of the same facts, and moving the default is the case
   *  where the two came apart: the badge moved here, the picker's idea of
   *  where a new tier starts did not, and both cards were individually
   *  correct (#634). A card cannot reload a sibling, so the page joins
   *  them and this is that card's half of the join. */
  onChanged?(): void;
}) {
  const api = useApi();
  const mediums = useAsync<StorageMedium[]>(() => api.listStorageMediums(), [api]);
  // The registry, for the configure step's renderer. Read here rather
  // than inside that step so the ONE fact both halves of the add flow
  // need - which backends exist and what each one declares - is read
  // once: AddDestinationWizard's picker and the form it hands off to
  // must not be able to disagree about a manifest.
  const catalog = useAsync<BackendCatalog>(() => api.listBackends(), [api]);

  // One function rather than passing `mediums.reload` and `onChanged`
  // separately down two paths: every change on this card has to do both,
  // and a row that remembered one and forgot the other is exactly the
  // half-refresh #634 is about.
  function changed() {
    mediums.reload();
    onChanged?.();
  }
  const [editing, setEditing] = useState<StorageMedium | null>(null);
  const [adding, setAdding] = useState(false);
  // The name chosen by the add wizard's step 2, held while the configure
  // step collects the values. Null means no add is part-way through.
  //
  // It is the NAME rather than a created destination because nothing has
  // been created: the configure step performs the one create, after it
  // has proved the destination (see this file's own doc, and
  // AddDestinationWizard's on why an unconfigured instance cannot exist).
  const [configuring, setConfiguring] = useState<{ backendId: string; instanceId: string } | null>(
    null
  );

  const configuringManifest = configuring
    ? catalog.data?.registered.find((m) => m.id === configuring.backendId)
    : undefined;

  return (
    <section className="card" role="region" aria-label="Storage destinations">
      <div className="card__header">
        <h2 className="eyebrow">Storage destinations</h2>
      </div>
      <div className="card__body" style={{ display: "flex", flexDirection: "column", gap: 14 }}>
        {isNotConfigured(mediums.error) ? (
          <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)" }}>
            Storage destinations are part of a configuration this instance has not been given
            yet. They become editable once the first backup set has been added.
          </p>
        ) : mediums.error ? (
          <ErrorState
            message={mediums.error.message}
            remediation="The storage destinations could not be read, so they cannot be edited here yet."
            correlationId={mediums.error.correlationId}
            onRetry={mediums.reload}
          />
        ) : mediums.data ? (
          <>
            <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)", maxWidth: "74ch" }}>
              Every place a retention tier can send a backup to, starting with the hard drive on
              this machine, which is where they land unless a tier says otherwise. Declaring
              another moves nothing on its own: backups arrive there only once a retention tier
              names it, which is a separate decision with a disclosure of its own.
            </p>

            {mediums.data.length === 0 ? (
              // Unreachable against a current engine, which always serves
              // the drive backups land on (#622), and kept as a hole
              // rather than deleted: an empty list is either an older
              // engine or a broken invariant, and both are better said
              // out loud than rendered as a card with nothing in it.
              <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>
                This deployment reports no storage destinations at all, not even the drive its
                backups land on. That should not be possible; the service is older than this page,
                or something is wrong with its configuration.
              </p>
            ) : (
              <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
                {mediums.data.map((m) => (
                  <DestinationRow
                    key={m.id}
                    medium={m}
                    currentDefaultId={mediums.data?.find((d) => d.isDefault)?.id}
                    readOnly={readOnly}
                    onEdit={() => setEditing(m)}
                    onChanged={changed}
                  />
                ))}
              </div>
            )}

            <div>
              <button className="btn btn--primary" disabled={readOnly} onClick={() => setAdding(true)}>
                Add a destination
              </button>
              <span style={{ marginLeft: 10, fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
                writes storage_mediums[] in config.yaml
              </span>
            </div>
          </>
        ) : (
          <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>Loading storage destinations…</p>
        )}

        {adding ? (
          <AddDestinationWizard
            existing={mediums.data ?? []}
            onClose={() => setAdding(false)}
            onConfirmed={(backendId, instanceId) => {
              // The backend id is carried, not discarded, and it is
              // carried as an ID rather than as a chosen surface: it is
              // what the configure step looks the MANIFEST up by, and
              // choosing a component by backend type would be the switch
              // EPIC I exists to delete.
              setAdding(false);
              setConfiguring({ backendId, instanceId });
            }}
          />
        ) : null}
        {configuring !== null ? (
          configuringManifest === undefined ? (
            // The registry has not answered yet, or has answered and
            // does not name this backend. The second case is a real
            // state and not a defect: a manifest is data, and a picker
            // held open across a manager restart that dropped one is
            // exactly when it happens. Rendering the form anyway would
            // mean rendering no fields at all and offering to save it.
            <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>
              {catalog.error
                ? "The backends this manager registers could not be read, so there is nothing to render a form from yet."
                : "Reading what this backend asks for…"}
            </p>
          ) : (
            <DestinationConfigureWizard
              manifest={configuringManifest}
              instanceId={configuring.instanceId}
              // Nothing has been created: the save below performs the one
              // create, after the probe has passed. See
              // AddDestinationWizard on why an unconfigured instance
              // cannot exist.
              destination={null}
              currentDefault={mediums.data?.find((m) => m.isDefault)?.id ?? null}
              onClose={() => setConfiguring(null)}
              onSaved={() => {
                setConfiguring(null);
                changed();
              }}
            />
          )
        ) : null}
        {editing ? (
          <S3DestinationWizard
            editing={editing}
            onClose={() => setEditing(null)}
            onSaved={() => {
              setEditing(null);
              changed();
            }}
          />
        ) : null}
      </div>
    </section>
  );
}

/**
 * One destination, with the things that can be done to it.
 *
 * Test connection and Remove both keep their result on this row rather
 * than navigating anywhere, because both answers are about this row and
 * because a failed check has to be able to sit beside the destination it
 * is about while the operator reads what is affected.
 *
 * # Which controls a row gets, and which are disabled rather than absent
 *
 * Absent is for a control that does not exist. Disabled-and-explained is
 * for a control an operator could have, once the deployment is in a
 * different state. Getting that distinction backwards is how a settings
 * page sends somebody hunting: a missing button has no reason beside it,
 * so the operator goes looking for the screen that has it, and there
 * isn't one.
 *
 * Edit is ABSENT on the local drive (#622). There is no editor here for
 * a destination whose location comes from the deployment's backup root,
 * and no state the operator can reach that would produce one.
 *
 * Make default and Remove are both DISABLED with a reason on the row
 * that holds the default (#671), never hidden. Those two are the pair
 * that traps an operator: the destination they cannot remove is the one
 * whose row used to offer neither the removal nor the way to earn it,
 * while the control that earns it sits on the OTHER rows. So the mark's
 * own row keeps both, says why each is off, and says what lifts them.
 *
 * Remove is keyed off `isDefault` and not off `isLocal`, which is the
 * one that changed with #670. The local drive is a declared destination
 * now (instance zero of the local_volume backend) and the backend
 * removes it like any other once it no longer holds the mark; keying the
 * button off `isLocal` made a real, removable destination permanently
 * undeletable from the browser, which is exactly the hazard
 * StorageMedium.isLocal's own doc warns about. A legacy configuration's
 * SYNTHESISED local entry is not reachable in the removable state: it
 * only exists while `default_storage_medium` is unset, which is to say
 * only while it IS the default, so its Remove is always the disabled and
 * explained one.
 *
 * # Moving the mark is confirmed, because two things change
 *
 * The transfer is not destructive — no backup moves, no tier is
 * rewritten — so the confirmation is not a consent gate and carries no
 * typed phrase. It exists because only ONE of the two things it does is
 * the thing the operator clicked: the destination they picked takes the
 * mark, and the one that had it becomes removable. An operator told only
 * the half they asked for has been misled by omission, and the half they
 * were not told is the one that makes a destination deletable.
 *
 * # "Test connection", not "Verify"
 *
 * The same idea was called three things: Verify here, `preflight` on the
 * command line, and "Test connection" on the source side of this same
 * product. One operator, one question, three words. This surface now says
 * what the other one already said, and the CLI keeps `preflight` working
 * as an alias so nothing scripted breaks.
 */
function DestinationRow({
  medium,
  currentDefaultId,
  readOnly,
  onEdit,
  onChanged
}: {
  medium: StorageMedium;
  /** The destination that holds the mark right now, which is the one
   *  this row's transfer would take it FROM.
   *
   *  It comes from the card rather than from this row, because a row
   *  knows only itself and the confirmation has to name the OTHER half
   *  of what the click does. Optional: a list with no default at all is
   *  a broken deployment the card already says so about, and the
   *  confirmation degrades to naming the consequence without the name
   *  rather than refusing to open. */
  currentDefaultId?: string;
  readOnly: boolean;
  onEdit(): void;
  onChanged(): void;
}) {
  const api = useApi();
  const [report, setReport] = useState<MediumPreflight | null>(null);
  const [usage, setUsage] = useState<StorageMediumUsage | null>(null);
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<ApiError | null>(null);
  const [confirmingTransfer, setConfirmingTransfer] = useState(false);

  async function testConnection() {
    setBusy(true);
    setFailure(null);
    try {
      const result = await api.preflightStorageMedium(medium.id);
      setReport(result);
      // The usage report is fetched WHEN THE CHECK FAILS and not before,
      // because that is the only moment it means anything: FR-30's
      // question is "what is affected", and asking it about a destination
      // that just answered would put a count on screen with nothing to
      // act on. Nothing about this fetch changes any state; it reads the
      // journal.
      setUsage(result.ok ? null : await api.getStorageMediumUsage(medium.id));
      // A check that PASSED against a destination carrying #636's mark has
      // just cleared it, server-side, so the list is re-read and the banner
      // below goes away in the same act that earned it. Only on a pass, and
      // only when there was a mark: a re-read after every button press
      // would be a request for nothing, and a banner that survived the
      // check that clears it is the thing an operator would report as a
      // bug.
      if (result.ok && medium.connectionUnverified) onChanged();
    } catch (e) {
      setFailure(apiErrorOf(e));
    } finally {
      setBusy(false);
    }
  }

  async function makeDefault() {
    // The dialog closes on confirm rather than on the answer. What it
    // asked has been answered; leaving it up while the request is in
    // flight would put the row's own failure banner behind a scrim.
    setConfirmingTransfer(false);
    setBusy(true);
    setFailure(null);
    try {
      await api.setDefaultStorageMedium(medium.id);
      onChanged();
    } catch (e) {
      setFailure(apiErrorOf(e));
    } finally {
      setBusy(false);
    }
  }

  async function remove() {
    setBusy(true);
    setFailure(null);
    try {
      await api.removeStorageMedium(medium.id);
      onChanged();
    } catch (e) {
      setFailure(apiErrorOf(e));
      // The refusal names a count; this puts the list behind it on screen,
      // because "148 copies affected" with nothing named is a number
      // rather than something an operator can act on.
      try {
        setUsage(await api.getStorageMediumUsage(medium.id));
      } catch {
        // A usage read that fails leaves the refusal standing on its own.
        // It is strictly extra detail, and swallowing its own failure is
        // better than replacing a precise refusal with a vaguer one.
      }
    } finally {
      setBusy(false);
    }
  }

  const failedVerification = report !== null && !report.ok;

  // One id per sentence, and the disabled control points at the sentence
  // that is about IT (aria-describedby). Not decoration: a disabled
  // button announces nothing on its own, so without the association a
  // screen reader reads "Remove, dimmed" and the reason further down the
  // row is a paragraph that could be about anything. It also makes
  // "explained" a thing a test can assert rather than infer from the row
  // happening to contain prose.
  const defaultReasonId = "destination-" + medium.id + "-holds-the-default";
  const unprovenReasonId = "destination-" + medium.id + "-never-proven";

  // A destination nobody has proven cannot take the mark (#671). Every
  // tier that follows the default would then start somewhere no check
  // has ever passed against, and the point of #636's mark is that this
  // deployment knows the difference. The control is off BEFORE the click
  // rather than failing after it, because "you cannot do that" is worth
  // more before an operator has decided than after.
  const transferBlockedBy = medium.isDefault
    ? defaultReasonId
    : medium.connectionUnverified
      ? unprovenReasonId
      : undefined;

  return (
    <div
      role="group"
      aria-label={"Storage destination " + medium.id}
      style={{
        border: "1px solid var(--border)",
        borderRadius: "var(--radius-lg)",
        padding: 12,
        display: "flex",
        flexDirection: "column",
        gap: 10
      }}
    >
      <div style={{ display: "flex", alignItems: "baseline", gap: 10, flexWrap: "wrap" }}>
        <span style={{ fontWeight: 600, fontSize: 14 }}>{medium.id}</span>
        <span className="mono" style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
          {describeDestination(medium)}
        </span>
        {medium.isDefault ? (
          <span className="badge" title="A retention tier created from here on starts on this destination.">
            Default
          </span>
        ) : null}
        {medium.readsRequireRestore ? (
          <span className="badge" style={{ color: "var(--warn)" }}>
            reads need a restore
          </span>
        ) : null}
        {medium.connectionUnverified ? (
          <span
            className="badge"
            style={{ color: "var(--warn)" }}
            title="This destination was declared without a check. A test connection that passes clears this."
          >
            never proven
          </span>
        ) : null}
      </div>

      <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
        <button className="btn" disabled={busy} onClick={testConnection}>
          {busy ? "Working…" : "Test connection"}
        </button>
        <button
          className="btn"
          disabled={readOnly || busy || transferBlockedBy !== undefined}
          aria-describedby={transferBlockedBy}
          onClick={() => setConfirmingTransfer(true)}
        >
          Make default
        </button>
        {medium.isLocal ? null : (
          <button className="btn" disabled={readOnly || busy} onClick={onEdit}>
            Edit
          </button>
        )}
        <button
          className="btn"
          disabled={readOnly || busy || medium.isDefault}
          aria-describedby={medium.isDefault ? defaultReasonId : undefined}
          onClick={remove}
        >
          Remove
        </button>
      </div>

      {medium.isDefault ? (
        <p
          id={defaultReasonId}
          style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--text-3)", maxWidth: "74ch" }}
        >
          This destination already carries the mark: a retention tier created from here on starts
          here, which is why Make default is off on this row. It cannot be removed while it carries
          the mark — give the mark to another destination and Remove comes back. Moving it moves no
          backup and rewrites no tier: it decides where the NEXT tier begins.
        </p>
      ) : null}
      {medium.isLocal ? (
        <p style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--text-3)", maxWidth: "74ch" }}>
          Every retention tier that names no other destination keeps its backups here. Where this
          one writes comes from the deployment&rsquo;s backup root rather than from fields on this
          list, so there is nothing here to edit.
        </p>
      ) : null}
      {/* Issue #636: a destination nobody ever proved, said out loud.
          This is what stops `--no-verify` being a hole rather than an
          escape hatch: the sentence the command printed was read once, by
          whoever typed it, and this is what is still here for the operator
          who did not. It clears itself the moment the Test connection
          button above comes back green, so it is a state rather than a
          permanent scar on a destination that happened to be declared
          offline.

          There is no button of its own here, deliberately. The control
          that clears this is already on the row, two lines up, and a
          second one would be two ways to do one thing. */}
      {medium.connectionUnverified ? (
        <p
          id={unprovenReasonId}
          style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--warn)", maxWidth: "74ch" }}
        >
          This destination was declared without a check, so nothing has shown that its credential is
          accepted, that the bucket is there, or that an object written here can be read back. That
          is also why it cannot be made the default: every tier that follows the default would start
          somewhere nobody has ever proven. Test connection clears this when it passes.
        </p>
      ) : null}

      <ConfirmationDialog
        open={confirmingTransfer}
        eyebrow="Two things change"
        title={"Make " + medium.id + " the default destination?"}
        confirmLabel={"Make " + medium.id + " the default"}
        onConfirm={makeDefault}
        onCancel={() => setConfirmingTransfer(false)}
      >
        <p style={{ margin: 0 }}>
          <strong>{medium.id}</strong> becomes the default: a retention tier created from here on
          starts here, and this destination cannot be removed while it carries the mark.
        </p>
        <p style={{ margin: 0 }}>
          <strong>{currentDefaultId ?? "The destination that holds it now"}</strong> stops being the
          default, and becomes removable. That is the half nobody clicked for, which is why it is
          said here rather than discovered later.
        </p>
        <p style={{ margin: 0 }}>
          Nothing already written moves. A tier that names a destination explicitly is untouched;
          only a tier that follows the default starts somewhere new, and no copy already stored is
          moved or deleted by this. Changing where new copies go is not a migration of the old ones.
        </p>
        <CommandEcho
          label="the same thing from a terminal — one line, both halves"
          commands={[setDefaultCommand(medium.id)]}
        />
      </ConfirmationDialog>

      <CommandEcho
        label="the same thing from a terminal"
        commands={commandsFor(medium)}
      />

      {failure ? (
        <Banner tone="warn" style={{ fontSize: 13 }} dismissKey={failure.message}>
          {failure.message}
        </Banner>
      ) : null}

      {failedVerification ? (
        <FailedVerificationBanner medium={medium} usage={usage} />
      ) : null}

      {report ? <MediumPreflightChecks report={report} /> : null}

      {usage && !failedVerification && failure ? <AffectedSets usage={usage} /> : null}
    </div>
  );
}

/**
 * The FR-30 case, and the one pane on this page whose wording is
 * load-bearing.
 *
 * Everything it says is a claim about what has NOT happened, because that
 * is the whole content of FR-30 at this moment: a destination this
 * deployment cannot reach is a destination it cannot ask about, and the
 * failure mode this banner exists to prevent is an operator reading a red
 * verification as "my backups are gone" and doing something drastic.
 */
function FailedVerificationBanner({
  medium,
  usage
}: {
  medium: StorageMedium;
  usage: StorageMediumUsage | null;
}) {
  const affected = usage?.placements ?? 0;
  const onlyCopy = (usage?.backupSets ?? []).reduce((n, s) => n + s.onlyCopyHere, 0);

  return (
    // Not dismissible (#620), and this is the pane the opt-out was
    // written for. Every sentence in it is a claim about what has NOT
    // happened, and the doc above says why that matters: the failure this
    // exists to prevent is an operator reading a red verification as "my
    // backups are gone" and doing something drastic. A close control here
    // would let the one thing standing between them and that reading be
    // put away in a click.
    <Banner tone="warn" dismissible={false} style={{ display: "block", fontSize: 13 }}>
      <div style={{ fontWeight: 600, marginBottom: 6 }}>
        {affected > 0
          ? `I cannot reach ${medium.id}, and ${affected} ${affected === 1 ? "copy is" : "copies are"} recorded there.`
          : `I cannot reach ${medium.id}. Nothing on record is stored there.`}
      </div>
      {affected > 0 ? (
        <>
          <p style={{ margin: "0 0 8px", maxWidth: "74ch" }}>
            Nothing has been deleted and nothing will be. A copy I cannot confirm is not a copy I
            treat as gone: those backups read as <strong>unreachable</strong> until this destination
            answers again, retention will not prune against them, and no source copy anywhere gets
            reclaimed on the strength of a placement I could not verify.
            {medium.isLocal
              ? " This is the drive on this machine, so it is not something the configuration declares and there is nothing here to edit or remove: the fix is on the machine, and the checks below say which part of it."
              : " Editing this destination is allowed. Removing it is refused while a copy names it."}
            {onlyCopy > 0
              ? ` ${onlyCopy} of them ${onlyCopy === 1 ? "is" : "are"} the only confirmed copy of their backup anywhere.`
              : ""}
          </p>
          {usage ? <AffectedSets usage={usage} /> : null}
        </>
      ) : (
        <p style={{ margin: 0, maxWidth: "74ch" }}>
          Nothing has changed.{" "}
          {medium.isLocal
            ? "No backup is recorded here yet, so nothing is lost. What it costs is the next backup: this is the drive they land on, and until it answers there is nowhere for one to land."
            : "No backup references this destination yet, so a failing check here costs nothing but the next move that would have used it."}
        </p>
      )}
    </Banner>
  );
}

/** The list behind the count. A count on its own is a number; this is the
 *  part an operator can act on. */
function AffectedSets({ usage }: { usage: StorageMediumUsage }) {
  if (usage.backupSets.length === 0) return null;
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
      <div className="eyebrow" style={{ fontSize: 10.5 }}>
        What is affected, listed rather than counted
      </div>
      {usage.backupSets.map((s) => (
        <div key={s.set} style={{ display: "flex", gap: 10, fontSize: 12.5, flexWrap: "wrap" }}>
          <span className="mono">{s.set}</span>
          <span style={{ color: "var(--text-2)" }}>
            {s.placements} {s.placements === 1 ? "backup" : "backups"}
            {s.onlyCopyHere > 0 ? " · only copy is here" : ""}
          </span>
          <span className="badge">unreachable</span>
        </div>
      ))}
      <p style={{ margin: 0, fontSize: 12, color: "var(--text-3)", maxWidth: "74ch" }}>
        Nothing on this list is reported as lost. Unreachable means this deployment currently has no
        way to ask, which is a different sentence from &ldquo;the copy is gone&rdquo;.
      </p>
    </div>
  );
}

/** The one-line description a row shows: everything about the place, and
 *  nothing about the credential, because the API reports nothing about the
 *  credential (see StorageMedium's own doc).
 *
 *  The local hard drive names the drive it writes to, which is the fact
 *  #622 says this LIST has to carry: an entry saying only "local" leaves
 *  an operator with two NAS volumes no better off than an entry that was
 *  missing entirely. It goes here rather than into the tier picker's
 *  label for the reason destinationLabel gives: this is the screen an
 *  operator comes to in order to see what their destinations ARE, and a
 *  picker is a list to choose between.
 *
 *  # Why the place is assembled from what is PRESENT (EPIC I, #664)
 *
 *  Two instances of one backend is the ordinary case, not an edge, and
 *  this line is what tells them apart. It used to read the bucket and
 *  nothing else, which is a description of one backend's idea of a place:
 *  a declared destination on a local volume has no bucket, so two of them
 *  both rendered as their type and stopped, and the list said two
 *  destinations were one thing — exactly the assumption #664 exists to
 *  remove, in the one place an operator would meet it.
 *
 *  So the location is whichever of bucket and path this destination
 *  actually carries, joined to its namespace. That is a branch on what is
 *  there rather than on which backend it is, which matters: a switch on
 *  backend type here would need a new arm for every manifest added, and
 *  the arm nobody wrote is a row that describes nothing. */
function describeDestination(m: StorageMedium): string {
  if (m.isLocal) return localDriveDescription(m);
  const where = [m.bucket || m.path, m.prefix].filter(Boolean).join("/");
  return [m.type, where, m.region, m.storageClass].filter(Boolean).join(" · ");
}

/** The commands one row's controls are equivalent to, in the order the
 *  buttons above them sit in.
 *
 *  A row prints the commands its buttons can actually be USED for, which
 *  is not the same as the buttons it shows. Since #671 the row that
 *  holds the mark shows a disabled Make default and a disabled Remove,
 *  and printing `medium default` under the destination that already
 *  carries the mark, or `medium remove` under one the engine refuses to
 *  remove, would teach two lines that do nothing but produce a refusal.
 *  The reason each control is off is a sentence on the row; the command
 *  list is not the place to repeat it.
 *
 *  `medium default` is also withheld from a destination nobody has
 *  proven, for the same reason its button is off there: it is not a
 *  transfer this deployment will make yet.
 *
 *  EPIC G's rule is that what an operator can DO has an equivalent
 *  command, not that every verb appears under every row. */
function commandsFor(m: StorageMedium): string[] {
  const out = [testConnectionCommand(m.id)];
  if (!m.isDefault && !m.connectionUnverified) out.push(setDefaultCommand(m.id));
  if (!m.isLocal) out.push(showCommand(m.id));
  if (!m.isDefault) out.push(removeCommand(m.id));
  return out;
}
