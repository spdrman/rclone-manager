/**
 * One backup set: its health, its connection, its retention, its recent
 * activity, and the in-place editor for everything about it that can
 * change.
 *
 * The page is large because editing is a MODE it enters rather than a
 * dialog it opens, and that mode has real obligations. Each box saves on
 * its own and saves only itself. A save is armed by comparing against what
 * the box held when the mode opened, not against the last keystroke, so
 * typing a character and deleting it leaves Save inactive. And while the
 * mode is open this page holds a lease on the set, renewed on a heartbeat,
 * so a cycle cannot start underneath an operator mid-edit; the heartbeat
 * exists for the exits a browser cannot report, like a closed laptop,
 * because a set left permanently paused is a backup silently not
 * happening.
 *
 * Two things here look like defensive over-engineering and are not, and
 * both are consequences of the router keeping this component mounted when
 * only the URL params change. The edit state is reset synchronously on a
 * set change, because an effect would paint one set's draft under the
 * other's heading first, and a Save in that frame would write the wrong
 * set's values. The repoint refusal is held apart from the field errors
 * for a different reason: it is not a field error at all, the value is
 * fine, and what the operator has is one decision to make about pointing
 * this set at different data.
 */
import { useEffect, useState } from "react";
import type { ReactNode } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { useApi } from "@shared/api/ApiContext";
import { fetchResource, useResource } from "@shared/state/resource";
import { graph, useCausl } from "@shared/state/graph";
import { setsNode, versionNode } from "@shared/state/appNodes";
import {
  captureSetEditSnapshot,
  currentSetActivityNode,
  currentSetDetailNode,
  isSetEditStale
} from "@shared/state/backupSetDetailNodes";
import type { SetEditSnapshot } from "@shared/state/backupSetDetailNodes";
import { PageHeader } from "@shared/components/PageHeader";
import { HealthBadge } from "@shared/components/StatusBadge";
import { FingerprintDisplay } from "@shared/components/FingerprintDisplay";
import { ActivityTimeline } from "@shared/components/ActivityTimeline";
import { WarningBanner } from "@shared/components/WarningBanner";
import { HaltBanner } from "@shared/components/HaltBanner";
import { ConfirmationDialog } from "@shared/components/ConfirmationDialog";
import { RemoveBackupSetDialog } from "@shared/components/RemoveBackupSetDialog";
import { SSHAuthWizard } from "@shared/components/SSHAuthWizard";
import { HelpField } from "@shared/components/FieldHelp";
import { ErrorState } from "@shared/components/EmptyState";
import { RetentionPreviewDialog } from "./RetentionPreviewDialog";
import { BackupSetRetentionCard } from "./BackupSetRetentionCard";
import { EDIT_FIELDS, readEditFields, visibleEditFields, withCompanions } from "./backupSetEditFields";
import type { EditField, EditFieldKey } from "./backupSetEditFields";
import type { BackupSetPatch, RunningWork } from "@shared/api/contracts";
import { apiErrorOf, describeFailure } from "@shared/api/failure";
import { bytes, relativeAge } from "@shared/utilities/format";

/**
 * How often an open edit form renews its hold (issue #350).
 *
 * A third of the server's own 90-second lease, so two consecutive
 * heartbeats can be lost to a slow or flapping network before the set
 * resumes under an operator who is still typing. Renewing is the cheap
 * direction to be wrong in; letting the lease lapse is not.
 */
const HOLD_HEARTBEAT_MS = 30_000;

const COMPLETION_COPY = {
  "atomic-rename": ["Atomic rename", "Producer writes to a temporary name, then renames into place."],
  "completion-marker": ["Completion marker / manifest", "Producer writes a sidecar manifest when the artifact is complete."],
  "stable-size": ["Stable file size / timestamp", "Infers completion \u2014 less assurance than a producer-provided marker."]
} as const;

export function BackupSetDetailPage({ readOnly }: { readOnly: boolean }) {
  // The route is two segments (App.tsx: /sets/:source/:set), matching a
  // real backup set id's own shape (model.BackupSetID.String() joins them
  // with "/" — core/internal/model/ids.go). Rejoining them here is the
  // same join that produces core's own id string, and it is safe: neither
  // half may contain "/" (model.validPart), so there is exactly one way
  // to read this pair back as one id (issue #285).
  const { source = "", set: setName = "" } = useParams();
  const setId = source && setName ? source + "/" + setName : "";
  const api = useApi();
  const navigate = useNavigate();
  // B2.2 (#97) — graph-backed, not page-local useAsync state: an edit
  // form opened against `set.data` needs a value with a real commit
  // history behind it to check staleness against (see
  // state/backupSetDetailNodes.ts's captureSetEditSnapshot/isSetEditStale).
  const set = useResource(currentSetDetailNode, () => api.getSet(setId), [api, setId]);
  const activity = useResource(currentSetActivityNode, () => api.listActivity(), [api]);
  // The configuration revision this screen is CURRENTLY showing. A run
  // submitted against a revision nobody looking at the page has seen is
  // what CONFIG_REVISION_STALE exists to refuse, so this deliberately
  // reads the graph rather than fetching a fresh one at submit time.
  const version = useCausl(versionNode);
  const [previewOpen, setPreviewOpen] = useState(false);
  const [removeOpen, setRemoveOpen] = useState(false);
  // Issue #592. The SSH surface used to be two boxes in the edit list
  // above; it is a wizard now, opened from the Connection section, and it
  // lives here rather than inside edit mode because choosing an
  // authentication method is not a field you type into.
  const [sshWizardOpen, setSSHWizardOpen] = useState(false);
  // Issue #391's `removing`/`removeError` pair moved into
  // RemoveBackupSetDialog with the rest of the removal, so the list page
  // and this one cannot drift apart on what removal promises, what it
  // does about a double press, or which refusal it reads as a success.

  // ------------------------------------------------------- issue #350
  //
  // Edit is a mode this page enters, not a dialog it opens. `baseline` is
  // what each box held when the mode opened and `draft` is what it holds
  // now; a box's Save is armed by draft !== baseline, which is the
  // issue's own rule (compared against the value LOADED, so typing a
  // character and deleting it leaves Save inactive) and is why both are
  // strings.
  const [editing, setEditing] = useState(false);
  const [baseline, setBaseline] = useState<Record<EditFieldKey, string> | null>(null);
  const [draft, setDraft] = useState<Record<EditFieldKey, string> | null>(null);
  const [fieldErrors, setFieldErrors] = useState<Partial<Record<EditFieldKey, string>>>({});
  const [savingFields, setSavingFields] = useState<EditFieldKey[]>([]);
  const [snapshot, setSnapshot] = useState<SetEditSnapshot | null>(null);
  const [stale, setStale] = useState(false);
  const [enterError, setEnterError] = useState<string | null>(null);
  const [warnAbout, setWarnAbout] = useState<RunningWork | null>(null);
  const [stopped, setStopped] = useState<RunningWork | null>(null);
  // The service refused this save because it needs something said out
  // loud first. Held rather than turned into a field error, because it is
  // not a field error: the value is fine, and the operator has one
  // decision to make about it. `exitAfter` carries whether the refusal
  // came from SAVE ALL, so confirming finishes what was asked for rather
  // than dropping the operator back into edit mode having done half of it.
  //
  // Two refusals share this one piece of state, and `kind` is which:
  // pointing the set at different data (issue #333/#350,
  // core/service/backupsetrepoint.go) and trusting a different host key
  // for the same host (issue #572, core/service/backupsethostkey.go). One
  // banner rather than two, because an operator can only be answering one
  // of them at a time; separate acknowledgements on the retry, because
  // confirming one must never grant the other.
  //
  // `patch` is the EXACT body that was refused, kept rather than rebuilt.
  // The host-key refusal names a fingerprint and asks the operator to
  // compare it, and the boxes stay editable underneath the banner, so a
  // retry that re-read the draft would let somebody compare fingerprint A,
  // change the box, press "Save anyway" and pin B. The acknowledgement has
  // to answer for the value it was shown for, and this is what makes it
  // one value rather than a yes.
  const [refusal, setRefusal] = useState<{
    kind: AcknowledgeableRefusal;
    keys: EditFieldKey[];
    patch: BackupSetPatch;
    message: string;
    exitAfter: boolean;
  } | null>(null);

  // The backup set this page is currently showing. Every piece of edit
  // state above belongs to ONE set, and React Router does not remount
  // this page for a :source/:set change alone (the same property this
  // page already guards its FETCH against, further down). Without
  // noticing that change here, walking from set A to set B while editing
  // would carry A's draft onto B's page under B's heading, and a Save
  // there would write A's values to B.
  const [pageSetId, setPageSetId] = useState(setId);

  // Committed synchronously during render rather than in an effect: the
  // same pattern, and the same reasoning, the edit dialog this mode
  // replaced used for its own reset and BackupSetWizardPage still uses
  // for its reset-on-mount. An effect runs after the paint, so B's page
  // would flash A's draft before correcting itself, and eslint is right
  // that it is a cascading render besides. Doing it here also means
  // `editing` is already false by the time the hold effect below runs, so
  // that effect's cleanup releases A's hold and its next run does
  // nothing, rather than briefly arming a heartbeat for a set nothing
  // holds.
  if (pageSetId !== setId) {
    setPageSetId(setId);
    setEditing(false);
    setDraft(null);
    setBaseline(null);
    setSnapshot(null);
    setFieldErrors({});
    setSavingFields([]);
    setStale(false);
    setStopped(null);
    setWarnAbout(null);
    setEnterError(null);
    setRefusal(null);
  }

  // The hold's lifetime, tied to `editing` rather than to any one button.
  // The cleanup runs when edit mode is left by ANY route, this component
  // unmounting included, which is the issue's "exiting edit mode releases
  // the hold, whether by SAVE ALL & EXIT EDIT or by leaving edit mode any
  // other way". The heartbeat covers the routes a browser cannot report
  // at all: a closed laptop or a lost network stops renewing, and the
  // server's own lease lapses. A set left permanently paused because
  // somebody closed a tab is a backup silently not happening.
  useEffect(() => {
    if (!editing || !setId) return;
    const timer = setInterval(() => {
      void api
        .takeEditHold(source, setName)
        // A renewal usually stops nothing, because the set is already
        // held. It stops something in exactly one case, and it is a real
        // one: the lease lapsed while nobody was renewing (a sleeping
        // laptop, a network that came back), a cycle started against this
        // set in the meantime, and this renewal cancelled it. Reporting
        // that through the same banner an initial hold uses is the
        // difference between an operator knowing a backup was interrupted
        // and finding out from the artifact list later.
        .then((hold) => {
          if (hold.stopped) setStopped(hold.stopped);
        })
        // Swallowed on purpose. A failed renewal is not an operator's
        // problem to act on: the lease simply lapses and the set resumes
        // backing up, which is the safe direction to fail in (a set left
        // permanently paused because a heartbeat could not get through is
        // a backup silently not happening).
        .catch(() => {});
    }, HOLD_HEARTBEAT_MS);
    return () => {
      clearInterval(timer);
      void api.releaseEditHold(source, setName).catch(() => {});
    };
  }, [editing, api, source, setName, setId]);

  if (set.error) return <ErrorState {...set.error} onRetry={set.reload} />;
  // Both checks matter, same as BackupDetailPage.tsx's equivalent fix
  // (B2.4 mandatory review): React Router does not remount this
  // component for a :source/:set change alone, so navigating set A -> set B
  // re-triggers this fetch while `data` still holds set A until the new
  // fetch resolves. Gating on `loading` too closes that window instead
  // of rendering set A's fields under set B's url.
  if (!set.data || set.loading) return null;

  const s = set.data;
  const [methodLabel, methodDetail] = COMPLETION_COPY[s.completionMethod];
  const events = (activity.data ?? []).filter((e) => e.setId === s.id).slice(0, 6);

  // visibleEditFields, not EDIT_FIELDS: a conditional box that is not on
  // screen (the stable-size window, when another completion method is
  // selected) must never end up in a patch. Walking the full table here
  // would send whatever that hidden box happened to hold.
  const dirtyKeys = (): EditFieldKey[] =>
    !draft || !baseline
      ? []
      : visibleEditFields(draft)
          .filter((f) => draft[f.key] !== baseline[f.key])
          .map((f) => f.key);

  // Takes the hold, then opens the mode. In that order and never the
  // other way round: the hold is what actually stops a cycle running
  // against this set, so a form that opened first would be editable for
  // as long as the request took, which is the window the hold exists to
  // close.
  const enterEditMode = async () => {
    setEnterError(null);
    try {
      const hold = await api.takeEditHold(source, setName);
      setStopped(hold.stopped);
    } catch (e) {
      setEnterError(describeFailure(e, "Backup Manager could not pause this backup set for editing.").message);
      return;
    }
    const loaded = readEditFields(s);
    setBaseline(loaded);
    setDraft({ ...loaded });
    setFieldErrors({});
    setStale(false);
    setRefusal(null);
    setSnapshot(captureSetEditSnapshot());
    setEditing(true);
  };

  // Pressing Edit asks what a cycle is doing for this set BEFORE it holds
  // anything, which is what makes declining possible: an operator is
  // shown what stopping would discard and can say no, and the cycle is
  // untouched because nothing has been held yet.
  const onEditPressed = async () => {
    setEnterError(null);
    let running: RunningWork | null;
    try {
      running = (await api.getEditHold(source, setName)).running;
    } catch (e) {
      // Refused rather than entered anyway. Opening edit mode without
      // knowing whether a cycle is running is exactly the two-writers
      // race the hold exists to prevent, and doing it silently would be
      // worse than saying so.
      setEnterError(
        describeFailure(e, "Backup Manager could not check whether a backup is running for this set.").message
      );
      return;
    }
    if (running) {
      setWarnAbout(running);
      return;
    }
    await enterEditMode();
  };

  /**
   * Persists exactly `keys` and nothing else.
   *
   * One path for both the per-box Save and SAVE ALL, differing only in
   * which keys they hand it, so the two can never disagree about what a
   * save sends. Returns whether it succeeded, because SAVE ALL has to
   * stay in edit mode when it did not.
   */
  const saveFields = async (
    requested: EditFieldKey[],
    acknowledge: AcknowledgeableRefusal | null = null,
    exitAfter = false,
    // The body a refused save carried, replayed verbatim. Only
    // confirmRefusal passes one; every other save builds its own from the
    // draft. See the `refusal` state above for why answering a refusal
    // must not go back to the boxes for the values.
    resend: BackupSetPatch | null = null
  ): Promise<boolean> => {
    // No draft is edit mode not being open, which is the same "nothing to
    // save" as an empty key list rather than a failure.
    if (requested.length === 0 || !draft) return true;
    // Expanded here, once, rather than on the button that asked: every
    // save reaches this function, so a pair of boxes that have to travel
    // together (the completion method and its window) cannot be split by
    // whichever control happened to be pressed. Idempotent, so the
    // acknowledgement retry re-sending its own keys expands to the same
    // set. See withCompanions for what the pair is and why.
    const keys = withCompanions(requested, draft);
    // The staleness check the dialog already ran, kept and moved here.
    // Inline editing holds the page open longer than a dialog did, so
    // this matters more here, not less.
    if (!snapshot || isSetEditStale(snapshot)) {
      setStale(true);
      return false;
    }

    let patch: BackupSetPatch;
    if (resend) {
      patch = { ...resend };
    } else {
      patch = {};
      const problems: Partial<Record<EditFieldKey, string>> = {};
      for (const key of keys) {
        const field = fieldFor(key);
        const parsed = field.parse(draft[key]);
        if (parsed.error) problems[key] = parsed.error;
        else Object.assign(patch, parsed.patch);
      }
      if (Object.keys(problems).length > 0) {
        setFieldErrors((prev) => ({ ...prev, ...problems }));
        return false;
      }
    }
    // Whichever refusal is being answered, it is answered for the body
    // just built (or replayed), so this is the last thing to touch it.
    const refused = { ...patch };
    // Only ever set when the operator has just been shown what it costs
    // and said yes, and only the one they were shown. It is never carried
    // across saves: the next save starts unacknowledged again, so a
    // second, different refusal asks again rather than riding on the
    // first answer.
    if (acknowledge === "repoint") patch.acknowledgeRepoint = true;
    if (acknowledge === "hostKey") patch.acknowledgeHostKeyChange = true;
    setRefusal(null);

    // Added to, and later removed from, rather than replaced wholesale.
    // Two per-box Saves can genuinely overlap (press one, press another
    // before the first answers), and a wholesale replace loses the first
    // one's entry: its button springs back to "Save", enabled, while its
    // request is still in flight, which invites the double submit it was
    // meant to prevent.
    setSavingFields((prev) => [...prev, ...keys]);
    try {
      const updated = await api.updateBackupSet(source, setName, patch);
      // The persisted truth goes back on the graph directly, rather than
      // through a reload. Two reasons, and the second is the one that
      // matters: a reload is fire-and-forget so there is no moment to
      // re-baseline the staleness snapshot against, and re-baselining is
      // mandatory here because this page's own commit bumps the very
      // version counter isSetEditStale compares. Without it the SECOND
      // per-box Save of a session would always report a concurrent edit
      // that never happened.
      graph.commit("app.currentSetDetail/edit-saved", (tx) =>
        tx.set(currentSetDetailNode, { data: updated, error: null, loading: false })
      );
      setSnapshot(captureSetEditSnapshot());
      // Both baseline and draft are re-read from the SERVER's answer for
      // the saved keys, not from the text that was sent: the box then
      // shows what is actually persisted (an include list the server
      // normalised, say), and its Save goes quiet because the two agree.
      // Keys nobody saved are untouched, which is what keeps another
      // box's unsaved edit on screen.
      const persisted = readEditFields(updated);
      setBaseline((prev) => applyKeys(prev, persisted, keys));
      setDraft((prev) => applyKeys(prev, persisted, keys));
      setFieldErrors((prev) => clearKeys(prev, keys));
      return true;
    } catch (e) {
      // Edit mode stays open and the typed value stays where it is; only
      // an explanation is added. Dropping back to view mode here would
      // discard the operator's work and show them the old value as
      // though nothing had happened.
      const message = describeFailure(e, "Backup Manager could not save this change.").message;
      const kind = REFUSALS_NEEDING_AN_ANSWER[apiErrorOf(e)?.code ?? ""];
      if (kind) {
        // Not a field error. The service is not saying the value is
        // wrong, it is saying this edit does something it wants confirmed
        // first, which is a decision with its own two answers rather than
        // a sentence under a box.
        setRefusal({ kind, keys, patch: refused, message, exitAfter });
        return false;
      }
      setFieldErrors((prev) => ({ ...prev, ...Object.fromEntries(keys.map((k) => [k, message])) }));
      return false;
    } finally {
      setSavingFields((prev) => prev.filter((key) => !keys.includes(key)));
    }
  };

  // Saves whatever is STILL dirty and leaves, and leaves regardless of
  // whether anything was. It is not a second chance to re-save what a
  // per-box Save already wrote, which is why it asks dirtyKeys() rather
  // than sending every field.
  const saveAllAndExit = async () => {
    if (!(await saveFields(dirtyKeys(), null, true))) return;
    leaveEditMode();
  };

  // The one way out of a refusal that actually writes. It re-sends the
  // refused body EXACTLY as it was sent, with the one acknowledgement that
  // refusal asked for, and then finishes whatever was asked for: SAVE ALL
  // leaves edit mode, a per-box Save stays.
  //
  // The body rather than the keys, because the two come apart precisely
  // where it matters. A host-key refusal prints a fingerprint and asks the
  // operator to check it against the host; the boxes stay editable while
  // they do; and re-reading them on the way back would pin whatever the
  // box holds at that moment rather than the thing they just compared.
  const confirmRefusal = async () => {
    const pending = refusal;
    if (!pending) return;
    if (!(await saveFields(pending.keys, pending.kind, pending.exitAfter, pending.patch))) return;
    if (pending.exitAfter) leaveEditMode();
  };

  const leaveEditMode = () => {
    setEditing(false);
    setDraft(null);
    setBaseline(null);
    setSnapshot(null);
    setFieldErrors({});
    setStale(false);
    setStopped(null);
    setRefusal(null);
  };

  const reloadLatestValues = () => {
    const fresh = captureSetEditSnapshot();
    setSnapshot(fresh);
    setStale(false);
    if (fresh) {
      const loaded = readEditFields(fresh.set);
      setBaseline(loaded);
      setDraft({ ...loaded });
    }
  };

  return (
    <>
      <PageHeader
        back={{ label: "Backup sets", onClick: () => navigate("/sets") }}
        title={
          <span style={{ display: "inline-flex", alignItems: "center", gap: 11 }}>
            {s.name}
            <HealthBadge state={s.state} />
          </span>
        }
        subtitle={
          <span className="mono">
            {s.host + ":" + s.port + " \u00b7 " + s.remoteFolder}
          </span>
        }
        actions={
          <>
            {/* A run cycle is deployment-wide: core walks every enabled
                backup set in one pass, and there is no per-set run
                operation to call. The label says so rather than implying
                this button touches only the set on screen (#211).

                It is not gated on this set's own halt state either, and
                that is the same point from the other side: refusing a
                pass over every OTHER enabled set because the one on
                screen is halted is the per-set reading of a
                deployment-wide control all over again. A set whose host
                key changed is refused by the transport layer on every
                cycle (FR-6), which is core's job and not a reason to
                take the fleet's run away from the operator (#231). */}
            <button
              className="btn btn--primary"
              disabled={readOnly}
              title="Runs one pass over every enabled backup set, not only this one."
              onClick={() => api.runCycle(version.data?.configRevision ?? "").then(set.reload)}
            >
              Run all due sets
            </button>
            <button className="btn" disabled={readOnly} onClick={() => api.testConnection(s.id)}>Test connection</button>
            {/* Issue #350: Edit is a mode, so this one button is both the
                way in and the way out. Read-only keeps it unavailable
                exactly as it always has. */}
            {editing ? (
              // Disabled while a per-box Save is still in flight. Without
              // that, pressing this during one would re-send the field
              // that save is already writing, because dirtyKeys() cannot
              // yet see a baseline the in-flight response has not
              // returned. Two writes of the same value are harmless in
              // themselves; a control that says "save what is still
              // unsaved" and then re-sends something already saved is
              // lying about its scope, which is the thing this issue is
              // about.
              <button
                className="btn btn--primary"
                disabled={savingFields.length > 0}
                onClick={() => void saveAllAndExit()}
              >
                SAVE ALL &amp; EXIT EDIT
              </button>
            ) : (
              <button className="btn" disabled={readOnly} onClick={() => void onEditPressed()}>Edit</button>
            )}
            <button className="btn" disabled={readOnly} onClick={() => setPreviewOpen(true)}>Preview retention</button>
          </>
        }
      />

      {/* No actions beside it, deliberately (#245). This banner used to
          offer "Compare fingerprints" and "Keep set halted" and neither
          carried an onClick: controls that looked like actions and were
          not. The evidence an operator needs is already on this page, in
          the fingerprint panel below, and resuming a set whose host key
          changed is an administrator action taken out of band that this
          manager will not offer to perform (§77 invariant 5). */}
      <HaltBanner set={s} />

      {enterError ? (
        <div style={{ marginBottom: 14 }}>
          <WarningBanner tone="warn" title="Could not open edit mode">
            {enterError}
          </WarningBanner>
        </div>
      ) : null}

      {stale ? (
        <div style={{ marginBottom: 14 }}>
          <WarningBanner
            tone="warn"
            title="This backup set changed since you opened edit mode"
            actions={
              <button className="btn btn--sm" onClick={reloadLatestValues}>Reload latest values</button>
            }
          >
            Someone (or something) else saved a change to this set first. Nothing from
            this form was applied. Review the latest values before editing again.
          </WarningBanner>
        </div>
      ) : null}

      {refusal ? (
        <div style={{ marginBottom: 14 }}>
          <WarningBanner
            tone="warn"
            title={REFUSAL_TITLE[refusal.kind]}
            actions={
              <>
                <button
                  className="btn btn--sm btn--primary"
                  disabled={savingFields.length > 0}
                  onClick={() => void confirmRefusal()}
                >
                  Save anyway
                </button>
                <button className="btn btn--sm" onClick={() => setRefusal(null)}>
                  Leave it as it was
                </button>
              </>
            }
          >
            {refusal.message}
          </WarningBanner>
        </div>
      ) : null}

      {editing && stopped ? (
        <div style={{ marginBottom: 14 }}>
          <WarningBanner tone="info" title="A backup was stopped for this edit">
            {"Backup Manager stopped " +
              (stopped.artifact || "the cycle") +
              " at the " + stopped.stage + " stage. It stays incomplete rather than counting as a finished backup, and the next cycle after you leave edit mode picks it up again."}
          </WarningBanner>
        </div>
      ) : null}

      <div
        style={{
          display: "grid", gridTemplateColumns: "minmax(0, 1.55fr) minmax(0, 1fr)",
          gap: 14, alignItems: "start"
        }}
      >
        <div style={{ display: "flex", flexDirection: "column", gap: 14, minWidth: 0 }}>
          <Section title="Overview">
            <dl
              style={{
                margin: 0, display: "grid",
                gridTemplateColumns: "repeat(auto-fit, minmax(150px, 1fr))",
                gap: "15px 18px", fontSize: 13
              }}
            >
              <Cell label="Newest known-good" value={relativeAge(s.newestKnownGoodAt)} mono />
              <Cell label="Last successful run" value={relativeAge(s.lastRunAt)} mono />
              {/* "Not reported", not "0 \u00b7 0 B" and not "every 0h". Both
                  of these were literals in api/client.ts on every real
                  deployment, and a zero here is a claim about how much a
                  set is keeping. */}
              <Cell
                label="Retained"
                value={
                  s.retainedCount === null || s.retainedBytes === null
                    ? "Not reported"
                    : s.retainedCount + " \u00b7 " + bytes(s.retainedBytes)
                }
                mono
              />
              <Cell
                label="Expected cadence"
                value={s.expectedIntervalHours === null ? "Not reported" : "every " + s.expectedIntervalHours + "h"}
                mono
              />
              <Cell label="State" value={s.stateNote} />
              {/* This cell was labelled "Remote cleanup" and read
                  `s.enabled`, which are two different facts. `enabled` is
                  config.BackupSet.Disabled inverted, and that field's own
                  doc says what it does: it excludes the set from
                  RunCycle. It says nothing about deleting anything from
                  the source. So the detail page of a disabled set
                  announced "Remote cleanup: Disabled", which reads as a
                  safety property ("this set will not delete from my
                  server") that the set does not have: enable it again and
                  it deletes exactly as before. The axis that DOES decide
                  that is `readOnly`, in the cell below, which is why the
                  comment there calls itself "a second, independent axis"
                  from a first axis that was never there. */}
              <Cell
                label="Collection"
                value={s.enabled ? "Enabled" : "Disabled \u2014 skipped by every run"}
              />
              {/* Issue #282/#316: the axis that decides whether the
                  source original is deleted after a commit, which is
                  independent of whether the set is collected at all: a
                  disabled set keeps its remote cleanup policy for
                  whenever it runs again, while a read-only set never
                  deletes the remote source, running or not. Retained
                  count only when it is nonzero and read-only, the same "a
                  permanent zero is a line an operator stops seeing"
                  reasoning `status`'s own CLI output already follows for
                  this exact figure. */}
              <Cell
                label="Read-only source"
                value={
                  s.readOnly
                    ? s.readOnlyRetainedCount > 0
                      ? "Yes — " + s.readOnlyRetainedCount + " retained"
                      : "Yes"
                    : "No"
                }
              />
            </dl>
          </Section>

          {editing && draft && baseline ? (
            <Section title="Edit this backup set">
              <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
                {visibleEditFields(draft).map((field) => (
                  <EditRow
                    key={field.key}
                    field={field}
                    value={draft[field.key]}
                    dirty={draft[field.key] !== baseline[field.key]}
                    saving={savingFields.includes(field.key)}
                    error={fieldErrors[field.key]}
                    onChange={(next) =>
                      setDraft((prev) => (prev ? { ...prev, [field.key]: next } : prev))
                    }
                    onSave={() => void saveFields([field.key])}
                  />
                ))}
              </div>
              <p style={{ margin: "14px 0 0", fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
                This set&rsquo;s name and source are its identity: every backup, journal
                entry and recovery manifest is filed under them, so they are not editable
                here. Its SSH key and trusted host key are not boxes here either: they are
                the wizard in the Connection panel, which offers what this machine already
                has rather than asking you to paste an id nothing would tell you.
              </p>
            </Section>
          ) : null}

          <Section title="Connection">
            {/* Every value in here is one the service read out of this
                set's own known_hosts. It used to be one literal and one
                empty string: the algorithm was "ssh-ed25519" written into
                this JSX and the fingerprint was a field api/client.ts
                filled with "", because nothing on the wire carried a host
                key. The halt banner for a CHANGED host key links here so
                an operator can compare a fingerprint against the server,
                which is the one comparison this page has to be able to
                support. */}
            <FingerprintDisplay
              host={s.host + ":" + s.port}
              keys={s.trustedHostKeys}
              trustedAt={s.trustedHostKeyRecordedAt}
            />
            <p style={{ margin: "12px 0 0", fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
              The private key never leaves this NAS and is never displayed.
            </p>
            {/* Issue #592. This used to be two text boxes in the edit
                list above, one of which asked an operator to paste an id
                the product had shown them exactly once. The wizard offers
                the keys this deployment already holds and the keys on
                this machine, settles the host key through #572's own
                refusal, and proves the whole path before it writes
                anything. */}
            <div style={{ marginTop: 12 }}>
              <button className="btn btn--sm" onClick={() => setSSHWizardOpen(true)}>
                Change SSH authentication
              </button>
              <p style={{ margin: "8px 0 0", fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
                {s.sshKeyId === ""
                  ? "This set uses a key this deployment does not manage, so there is no key id to show. The wizard can point it at one this deployment holds."
                  : "Authenticating with key " + s.sshKeyId + "."}
              </p>
            </div>
          </Section>

          <Section title="Backup discovery">
            <dl style={{ margin: 0, display: "grid", gridTemplateColumns: "172px 1fr", gap: "11px 16px", fontSize: 13 }}>
              <Row label="Remote folder" value={s.remoteFolder} mono />
              <Row label="Include" value={s.includePatterns.join(", ") || "\u2014"} mono />
              <Row label="Exclude" value={s.excludePatterns.join(", ") || "\u2014"} mono />
              <Row
                label="Completion method"
                value={
                  <>
                    {methodLabel}
                    <span style={{ color: "var(--text-3)" }}>{" \u2014 " + methodDetail}</span>
                  </>
                }
              />
            </dl>
            {s.completionMethod === "stable-size" ? (
              <div style={{ marginTop: 12 }}>
                <WarningBanner tone="warn">
                  This method infers completion and provides less assurance than a
                  producer-provided completion marker.
                </WarningBanner>
              </div>
            ) : null}
          </Section>

          <Section title="Activity">
            <ActivityTimeline events={events} dense />
          </Section>
        </div>

        <div style={{ display: "flex", flexDirection: "column", gap: 14, minWidth: 0 }}>
          {/* Issue #333. This section used to render three numbers off
              BackupSet.retention, which nothing computed: it read
              "0 kept" three times against every real deployment. It now
              names which policy retains this set, shows the chain that
              policy actually is, and carries the three operations that
              change it. */}
          <Section title="Retention">
            <BackupSetRetentionCard
              source={s.source}
              set={s.set}
              readOnly={readOnly}
              onPreview={() => setPreviewOpen(true)}
            />
          </Section>

          <Section title="Validation">
            <ul style={{ margin: 0, padding: 0, listStyle: "none", display: "flex", flexDirection: "column", gap: 10, fontSize: 13 }}>
              {(["transfer", "checksum", "application"] as const).map((v) => {
                const on = s.validations.includes(v);
                return (
                  <li key={v} style={{ display: "flex", gap: 9 }}>
                    <span aria-hidden="true" style={{ color: on ? "var(--ok)" : "var(--text-3)" }}>
                      {on ? "\u2713" : "\u2013"}
                    </span>
                    <span>
                      {v === "transfer" ? "Transfer verification" : v === "checksum" ? "Checksum verification (SHA-256)" : "Application validation"}
                      {on ? null : <span style={{ color: "var(--text-3)" }}> — not enabled</span>}
                    </span>
                  </li>
                );
              })}
            </ul>
          </Section>

          {/* Caution and destructive actions live apart from ordinary ones (§11, §35). */}
          <Section title="Set management">
            <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
              <button className="btn btn--caution" disabled={readOnly} onClick={() => api.setEnabled(s.source, s.set, !s.enabled).then(set.reload)}>
                {s.enabled ? "Disable backup set" : "Enable backup set"}
              </button>
              {/* Issue #316: the read-only counterpart to the
                  enable/disable toggle above, following the same
                  CRUD-parity shape (a dedicated toggle route, not a
                  generic edit). Turning this ON only prevents a FUTURE
                  deletion; turning it back off does not reach back and
                  delete anything already retained under it
                  (core/service.SetBackupSetReadOnly's own doc) — so it
                  sits in the caution tier beside Disable, not the
                  destructive one below. */}
              <button
                className="btn btn--caution"
                disabled={readOnly}
                onClick={() => api.setReadOnly(s.source, s.set, !s.readOnly).then(set.reload)}
              >
                {s.readOnly ? "Allow remote deletion again" : "Declare source read-only"}
              </button>
              <button className="btn btn--destructive" disabled={readOnly} onClick={() => setPreviewOpen(true)}>
                Apply retention now…
              </button>
              <button className="btn btn--destructive" disabled={readOnly} onClick={() => setRemoveOpen(true)}>
                Remove set configuration…
              </button>
              <p style={{ margin: 0, fontSize: "var(--text-sm)", color: "var(--text-3)" }}>
                Removing configuration never deletes retained backups from NAS storage.
              </p>
            </div>
          </Section>
        </div>
      </div>

      <RetentionPreviewDialog source={s.source} set={s.set} open={previewOpen} onClose={() => setPreviewOpen(false)} />

      {/* Issue #350: entering edit mode stops any cycle running against
          this set, so an operator is told what that discards before it
          happens, and only when something is actually running. Declining
          leaves the cycle untouched, because nothing has been held yet. */}
      <ConfirmationDialog
        open={warnAbout !== null}
        destructive
        eyebrow="A backup is running for this set"
        title="Stop it to edit this backup set?"
        confirmLabel="Stop it and edit"
        cancelLabel="Keep backing up"
        onCancel={() => setWarnAbout(null)}
        onConfirm={() => {
          setWarnAbout(null);
          void enterEditMode();
        }}
      >
        <p style={{ margin: 0 }}>
          {"Backup Manager is " +
            (warnAbout?.stage ?? "") +
            (warnAbout?.artifact ? " " + warnAbout.artifact : " this set's current cycle") +
            " right now. Editing this set stops it, and holds the schedule until you leave edit mode."}
        </p>
        <p style={{ margin: 0, color: "var(--text-2)" }}>
          The stopped backup stays incomplete rather than counting as a finished one, and
          the next cycle after you leave edit mode starts it again from where it can.
        </p>
      </ConfirmationDialog>

      {/* Issue #391. This confirmation used to close and call nothing,
          which meant an operator confirmed a destructive action, watched
          it close, and reasonably believed the set was gone while it kept
          collecting. The dialog itself now lives in
          RemoveBackupSetDialog, shared with the list page, because the
          promise it makes and the 404 it has to read as a success are
          the parts that rot when they are written twice; that component's
          own doc has the reasoning.

          On success this page NAVIGATES rather than reloading: the set it
          is showing does not exist any more, so re-reading it would put a
          404 error state in front of somebody whose action had just
          worked. It refreshes the shared setsNode before it goes, because
          BackupSetsPage renders that node and App fetches it once on
          mount and then only on its poll; navigating does not remount
          App, so without the refresh the list the operator lands on still
          shows the set they just removed, for up to thirty seconds. Same
          refresh, same reason, as the create path in
          BackupSetWizardPage. */}
      {/* Issue #592. Mounted with the set it is about, and it applies
          through the same PATCH the edit list uses, so a successful pass
          reloads this page from the server's own answer rather than from
          what the wizard hoped it wrote. */}
      <SSHAuthWizard
        set={s}
        open={sshWizardOpen}
        onCancel={() => setSSHWizardOpen(false)}
        onApplied={() => {
          setSSHWizardOpen(false);
          set.reload();
        }}
      />

      <RemoveBackupSetDialog
        set={s}
        open={removeOpen}
        onCancel={() => setRemoveOpen(false)}
        onRemoved={() => {
          setRemoveOpen(false);
          fetchResource(setsNode, () => api.listSets());
          navigate("/sets");
        }}
      />
    </>
  );
}

/**
 * One editable box with its own Save.
 *
 * The Save button is a SIBLING of HelpField, never a child of it, and
 * that is load-bearing rather than a layout preference: HelpField wraps
 * its content in a <label>, a <label> binds to its first labelable
 * descendant, and <button> is labelable. A Save button placed inside
 * would silently rebind the field's label to the button, so the input
 * would have no accessible name and the button would have the field's.
 */
function EditRow({
  field,
  value,
  dirty,
  saving,
  error,
  onChange,
  onSave
}: {
  field: EditField;
  value: string;
  dirty: boolean;
  saving: boolean;
  error?: string;
  onChange(next: string): void;
  onSave(): void;
}) {
  return (
    <div>
      <div style={{ display: "flex", gap: 9, alignItems: "flex-end" }}>
        <div style={{ flex: 1, minWidth: 0 }}>
          <HelpField label={field.label} help={field.help}>
            {(helpId) =>
              field.control === "select" ? (
                <select
                  className="input"
                  aria-describedby={helpId}
                  value={value}
                  onChange={(e) => onChange(e.target.value)}
                >
                  {(field.options ?? []).map((option) => (
                    <option key={option.value} value={option.value}>{option.label}</option>
                  ))}
                </select>
              ) : (
                <input
                  className="input"
                  type={field.control === "number" ? "number" : "text"}
                  aria-describedby={helpId}
                  value={value}
                  onChange={(e) => onChange(e.target.value)}
                />
              )
            }
          </HelpField>
        </div>
        {/* The accessible name names the box, so a screen reader (and a
            test) can tell seven Saves apart. */}
        <button
          className="btn btn--sm"
          aria-label={"Save " + field.label.toLowerCase()}
          disabled={!dirty || saving}
          onClick={onSave}
        >
          {saving ? "Saving\u2026" : "Save"}
        </button>
      </div>
      {error ? (
        <p role="alert" style={{ margin: "6px 0 0", fontSize: "var(--text-sm)", color: "var(--danger)" }}>
          {error}
        </p>
      ) : null}
    </div>
  );
}

/** The two refusals a save can come back with that an operator can answer
 *  rather than fix. Each has its own acknowledgement on the retry, which
 *  is the whole reason this is a union and not a boolean: confirming one
 *  must never grant the other. */
type AcknowledgeableRefusal = "repoint" | "hostKey";

/** Which service refusal is which. Keyed by the wire code so an error
 *  this page does not recognise falls through to the ordinary field-error
 *  path rather than being offered a confirmation it has no answer for. */
const REFUSALS_NEEDING_AN_ANSWER: Record<string, AcknowledgeableRefusal | undefined> = {
  BACKUP_SET_REPOINT_NOT_ACKNOWLEDGED: "repoint",
  BACKUP_SET_HOST_KEY_CHANGE_NOT_ACKNOWLEDGED: "hostKey"
};

/** The banner's heading. The body is always the service's own sentence,
 *  which for the host key carries the fingerprints being compared; this is
 *  only what the operator is being asked about.
 *
 *  The host-key heading says "changes" rather than "trusts a different
 *  key", because the service asks in both directions: a new key being
 *  pinned, and a key already on record that the one line being sent would
 *  stop pinning. A heading naming only the first would contradict the
 *  sentence underneath it for the second. */
const REFUSAL_TITLE: Record<AcknowledgeableRefusal, string> = {
  repoint: "This change points the backup set at different data",
  hostKey: "This change alters what host keys this backup set trusts"
};

function fieldFor(key: EditFieldKey): EditField {
  const found = EDIT_FIELDS.find((f) => f.key === key);
  // EditFieldKey is a union of exactly EDIT_FIELDS' own keys, so this is
  // unreachable; throwing rather than falling back keeps a future key
  // added to the union but not to the table from silently saving nothing.
  if (!found) throw new Error("no edit field named " + key);
  return found;
}

/** Copies `keys` from `from` onto `into`, leaving every other key as it
 *  was. This is what "a per-box Save leaves other boxes alone" looks like
 *  in state, and it is a function rather than an inline spread because
 *  both baseline and draft need exactly the same operation. */
function applyKeys(
  into: Record<EditFieldKey, string> | null,
  from: Record<EditFieldKey, string>,
  keys: EditFieldKey[]
): Record<EditFieldKey, string> | null {
  if (!into) return into;
  const next = { ...into };
  for (const key of keys) next[key] = from[key];
  return next;
}

function clearKeys(
  errors: Partial<Record<EditFieldKey, string>>,
  keys: EditFieldKey[]
): Partial<Record<EditFieldKey, string>> {
  const next = { ...errors };
  for (const key of keys) delete next[key];
  return next;
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="card">
      <div className="card__header">
        <h2 className="eyebrow">{title}</h2>
      </div>
      <div className="card__body">{children}</div>
    </section>
  );
}

/**
 * The two label-and-value patterns this page states its facts in.
 *
 * Cell is a boxed summary tile; Row is a two-column grid whose dt and dd
 * are direct grid children, so it returns a fragment and must not wrap
 * them in anything.
 *
 * Neither gives its dt or its dd an accessible name, and that is a known
 * gap rather than an oversight. `<dt>` is role `term` and `<dd>` is role
 * `definition`, and both take their name from the author rather than from
 * their content, so every row here is unreachable by anything navigating
 * by role and every value is announced with nothing attached to it. The
 * fix is one line in each of these two functions (an id on the dt, an
 * aria-labelledby on the dd) and it was written, measured and taken back
 * out: four of this page's read-only labels are also the labels of its
 * inline EDIT fields (Host, User, Remote folder, Completion method), so
 * naming the values puts two elements with the same accessible name on
 * the page whenever edit mode is open. That is ambiguous for anything
 * looking a control up by its label, and deciding whether a read-only
 * value and an editable field may share a name is a decision about
 * backupSetEditFields.ts rather than about this file. See the pull
 * request that recorded it.
 */
function Cell({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div>
      <dt className="eyebrow" style={{ fontSize: 10.5, letterSpacing: "0.06em" }}>{label}</dt>
      <dd style={{ margin: "4px 0 0", fontFamily: mono ? "var(--font-mono)" : undefined }}>{value}</dd>
    </div>
  );
}

function Row({ label, value, mono }: { label: string; value: ReactNode; mono?: boolean }) {
  return (
    <>
      <dt style={{ color: "var(--text-2)" }}>{label}</dt>
      <dd className={mono ? "mono" : undefined} style={{ margin: 0 }}>{value}</dd>
    </>
  );
}

