/**
 * Adding a destination: choose a backend, name this instance, confirm
 * (EPIC I item I2.1, issue #668).
 *
 * # Why choosing and naming are two steps
 *
 * This is the design decision that carries the epic, and it will look like
 * ceremony in the release that ships two backends, so the argument is
 * written here rather than left to be re-derived.
 *
 * A destination is an INSTANCE of a registered backend. Several instances
 * of one backend is the normal case — two local volumes, a hot bucket and
 * a cold one — not an edge. Collapsed into one screen, "pick S3" and "call
 * it cold_archive" become one act, and a wizard that treats them as one
 * act has quietly re-asserted that a destination IS a backend type, which
 * is precisely the assumption #664 exists to remove. Keeping them apart
 * also puts the uniqueness rule where it applies: step 2 lists the
 * instances that already exist on the chosen backend, so an operator sees
 * why a name is taken instead of meeting a validation error about it.
 *
 * # The backend list comes from the registry
 *
 * There is no array of backend names in this file and no branch on which
 * backend was chosen. The list is whatever GET /api/v1/backends serves,
 * which is whatever manifests the build bundles. A hardcoded list here
 * would be the epic failing in the one surface the epic is about: it would
 * compile, it would look right with two backends, and it would silently
 * not grow when a third manifest was added.
 *
 * The naming rules come from the same response for the same reason. The
 * pattern and the reserved id are the engine's own constants, so this form
 * refuses exactly what a hand-edited configuration file would be refused
 * for. See destinationInstanceName.ts, which is also where the argument
 * about never echoing what was typed lives.
 *
 * # The wider catalogue is shown, dimmed, and there are two kinds of it
 *
 * Backends the engine understands and no manifest declares are rendered
 * and cannot be chosen. Hiding them answers an operator worse: somebody
 * who came here for SFTP learns nothing from a menu that never mentions
 * it and asks again next month, whereas a row saying the shape is
 * understood and is not registered is a real answer. They carry no
 * radio, so there is nothing to submit, and the server would refuse an
 * id no manifest declares in any case.
 *
 * A REGISTERED backend can be dimmed too, and that is #731's row: a
 * manifest reporting `configurable: false` is a backend this build
 * describes in full and cannot yet store or dial. It is rendered with
 * its label, its summary and the reason, and its radio is disabled, so
 * it is findable and unchoosable. Offering it instead would be worse
 * than either: the wizard would collect a name, the configure step
 * would collect eight values and a credential, and the save would be
 * refused by the schema — a dead end an operator walks the whole length
 * of before finding out. Tracked in #235.
 *
 * # Nothing is written here, and why that is not what the issue says
 *
 * #668's text says a destination is created by this wizard and is not
 * usable until it has been configured and a check has passed. The product
 * does not do that, deliberately, because the schema cannot hold it: an
 * instance carrying no values is refused by
 * backend.Registry.ValidateInstance (validate.go:228-233), which
 * config.Validate delegates every per-field rule to since #667 — a
 * required field that is absent is `<field> is required`, and both
 * bundled backends have required fields. An unconfigured destination is
 * not representable in config.yaml at all.
 *
 * So this wizard chooses and names, and the configure step that follows
 * performs the ONE create, after proving it. That is #594's property
 * ("nothing is written until the destination has been proven")
 * strengthened rather than weakened, and it leaves no half-created record
 * in an operator's configuration file. The confirm step says what will be
 * written and that nothing has been yet.
 *
 * The two-phase write the issue describes remains a defensible design. It
 * needs a notion of "unconfigured" in config.StorageMedium itself, plus a
 * migration and a decision in every consumer of a medium — a tier picker,
 * a probe runner, the health surface — which is an issue of its own and
 * not a step in a wizard.
 */
import { useMemo, useState } from "react";
import { useApi } from "@shared/api/ApiContext";
import type { BackendCatalog, BackendManifest, StorageMedium } from "@shared/api/contracts";
import { useAsync } from "@shared/hooks/useAsync";
import { ErrorState } from "@shared/components/EmptyState";
import { refuseInstanceName } from "@shared/pages/destinationInstanceName";
import type { InstanceNameRefusal } from "@shared/pages/destinationInstanceName";

export function AddDestinationWizard({
  existing,
  onClose,
  onConfirmed
}: {
  /** Every destination that already exists, which is what makes the
   *  uniqueness rule checkable here instead of at the API. Passed in
   *  rather than fetched again: the list this wizard opens from already
   *  holds it, and two reads of one fact are two answers waiting to
   *  disagree. */
  existing: StorageMedium[];
  onClose(): void;
  /** The backend and the name the operator settled on, handed to the
   *  configure step, which collects the values, proves them, and performs
   *  the one create.
   *
   *  A callback rather than a write of its own because nothing is written
   *  here at all: see this file's own doc on why an unconfigured instance
   *  is not representable. It carries the manifest ID and not the
   *  manifest, so the surface on the other side reads the backend it was
   *  named from the same catalogue this one did rather than being handed a
   *  copy that could be stale by then. */
  onConfirmed(backendId: string, instanceId: string): void;
}) {
  const api = useApi();
  const catalog = useAsync<BackendCatalog>(() => api.listBackends(), [api]);

  const [step, setStep] = useState<1 | 2 | 3>(1);
  const [backendId, setBackendId] = useState("");
  const [search, setSearch] = useState("");
  const [name, setName] = useState("");
  // Untouched means "not asked yet", and it is why an empty field carries
  // no refusal: opening a step onto a red message tells an operator they
  // did something wrong before they did anything at all.
  const [touched, setTouched] = useState(false);

  // Only a CONFIGURABLE backend can be the chosen one. Steps 2 and 3
  // render nothing without it and onConfirmed is unreachable, so a
  // preview backend cannot be submitted even if something set the id
  // behind the disabled row's back: the check is here, once, rather
  // than repeated as a guard on every button that leads onward.
  const chosen = catalog.data?.registered.find((b) => b.id === backendId && b.configurable);

  const existingIds = useMemo(() => existing.map((m) => m.id), [existing]);

  const refusal: InstanceNameRefusal | null = catalog.data
    ? refuseInstanceName(name, {
        pattern: catalog.data.instanceIdPattern,
        reservedId: catalog.data.reservedInstanceId,
        existingIds
      })
    : null;

  return (
    <div
      role="group"
      aria-label="Add a destination"
      style={{
        border: "1px solid var(--border-strong)",
        borderRadius: "var(--radius-xl)",
        padding: 14,
        display: "flex",
        flexDirection: "column",
        gap: 14,
        background: "var(--surface-2)"
      }}
    >
      <div style={{ display: "flex", alignItems: "baseline", gap: 10, flexWrap: "wrap" }}>
        <strong style={{ fontSize: 14 }}>Add a destination</strong>
        <span style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
          {step === 3 ? "what will be written, before anything is" : "nothing is written by these steps"}
        </span>
        <span style={{ marginLeft: "auto", fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
          Step {step} of 3
        </span>
      </div>

      {/* There is no failure banner here, and that is not an omission:
          this wizard makes no request but the catalogue read below, whose
          own failure is reported by ErrorState. The refusal an operator
          might meet belongs to the configure step that follows, which is
          the thing that writes. */}

      {catalog.error ? (
        <ErrorState
          message={catalog.error.message}
          remediation="The registered backends could not be read, so there is nothing to choose between yet."
          correlationId={catalog.error.correlationId}
          onRetry={catalog.reload}
        />
      ) : !catalog.data ? (
        <p style={{ margin: 0, fontSize: 13, color: "var(--text-3)" }}>Loading the registered backends…</p>
      ) : (
        <>
          {step === 1 ? (
            <ChooseBackendPane
              catalog={catalog.data}
              search={search}
              onSearch={setSearch}
              chosenId={backendId}
              onChoose={setBackendId}
              onCancel={onClose}
              onNext={() => setStep(2)}
            />
          ) : null}

          {step === 2 && chosen ? (
            <NameInstancePane
              backend={chosen}
              existing={existing}
              name={name}
              onName={(v) => {
                setName(v);
                setTouched(true);
              }}
              refusal={refusal}
              showRefusal={touched}
              onBack={() => setStep(1)}
              onNext={() => setStep(3)}
            />
          ) : null}

          {step === 3 && chosen ? (
            <ConfirmPane
              backend={chosen}
              name={name}
              onBack={() => setStep(2)}
              onConfirm={() => onConfirmed(chosen.id, name)}
            />
          ) : null}
        </>
      )}
    </div>
  );
}

/**
 * Step 1: which backend. Every row comes from the catalogue.
 *
 * The search box exists because the dimmed half of this list is the wider
 * catalogue, which is the part somebody arrives looking for a specific
 * name in. It filters and never adds: a search that produced a row the
 * response did not carry would be this component inventing a backend.
 */
function ChooseBackendPane({
  catalog,
  search,
  onSearch,
  chosenId,
  onChoose,
  onCancel,
  onNext
}: {
  catalog: BackendCatalog;
  search: string;
  onSearch(v: string): void;
  chosenId: string;
  onChoose(id: string): void;
  onCancel(): void;
  onNext(): void;
}) {
  const needle = search.trim().toLowerCase();
  const registered = catalog.registered.filter(
    (b) =>
      needle === "" ||
      // No transport in the haystack, because a manifest no longer
      // reports one (#81). Nothing was lost: for both bundled manifests
      // every substring of the transport is already a substring of the
      // id, so no needle changed result.
      [b.id, b.label, b.summary].some((h) => h.toLowerCase().includes(needle))
  );
  const unregistered = catalog.unregistered.filter(
    (u) => needle === "" || u.transport.toLowerCase().includes(needle)
  );
  // What "chosen" is allowed to mean here. A row that cannot be
  // configured cannot be the answer, so the button that leads onward
  // reads this rather than "something is selected": disabling the radio
  // is what an operator SEES, and this is what the wizard obeys.
  const chosenIsConfigurable = catalog.registered.some((b) => b.id === chosenId && b.configurable);

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
      <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)", maxWidth: "74ch" }}>
        A destination is one instance of a backend. Choosing a backend here decides what questions the next
        steps ask; it does not decide which destination this is, which is the step after.
      </p>

      <label style={{ display: "flex", flexDirection: "column", gap: 4, maxWidth: "40ch" }}>
        <span style={{ fontSize: "var(--text-xs)", color: "var(--text-2)" }}>Search backends</span>
        <input
          className="input"
          type="text"
          value={search}
          onChange={(e) => onSearch(e.target.value)}
          placeholder="s3, local, sftp…"
        />
      </label>

      <ul
        role="list"
        aria-label="Registered backends"
        style={{ listStyle: "none", margin: 0, padding: 0, display: "flex", flexDirection: "column", gap: 8 }}
      >
        {registered.map((b) => (
          <li key={b.id}>
            <label
              // A registered backend this build cannot yet save is
              // shown and refused, exactly like the unregistered rows
              // below and for the same reason: the row is the answer
              // somebody came looking for. What it may not do is lead
              // anywhere — the radio is disabled, so there is nothing
              // to choose and nothing to submit.
              aria-disabled={b.configurable ? undefined : "true"}
              style={{
                display: "flex",
                gap: 10,
                alignItems: "flex-start",
                border: b.configurable ? "1px solid var(--border)" : "1px dashed var(--border)",
                borderRadius: "var(--radius-lg)",
                padding: 10,
                cursor: b.configurable ? "pointer" : "not-allowed",
                color: b.configurable ? undefined : "var(--text-3)"
              }}
            >
              <input
                type="radio"
                name="backend"
                value={b.id}
                checked={chosenId === b.id}
                disabled={!b.configurable}
                onChange={() => {
                  // Guarded rather than trusted to the disabled
                  // attribute: a change event that reaches a row this
                  // build cannot save must choose nothing, however it
                  // was produced.
                  if (b.configurable) {
                    onChoose(b.id);
                  }
                }}
                style={{ marginTop: 3 }}
              />
              <span style={{ display: "flex", flexDirection: "column", gap: 2 }}>
                <strong style={{ fontSize: 13 }}>{b.label}</strong>
                <span style={{ fontSize: 12, color: b.configurable ? "var(--text-2)" : "inherit" }}>
                  {b.summary}
                </span>
                {b.configurable ? null : (
                  <span style={{ fontSize: 12 }}>
                    Not yet configurable — tracked in #235. This build describes the shape and cannot save
                    one, so there is nothing here to fill in yet.
                  </span>
                )}
                <span style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
                  {/* The manifest id, because it is what the command line
                      and the configuration file both spell, and an
                      operator who reads it here recognises it there.
                      It used to be followed by the rclone backend this
                      dials; that named an implementation on a screen and
                      on the wire behind it, which #81 forbids, and the
                      product answer to what a backend is is its label,
                      its summary and its role. */}
                  {b.id}
                </span>
              </span>
            </label>
          </li>
        ))}
      </ul>

      {unregistered.length > 0 ? (
        <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
          <span style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
            Understood, and not registered in this build
          </span>
          <ul
            role="list"
            aria-label="Backends this build does not register"
            style={{ listStyle: "none", margin: 0, padding: 0, display: "flex", flexDirection: "column", gap: 6 }}
          >
            {unregistered.map((u) => (
              <li
                key={u.transport}
                aria-label={u.transport}
                aria-disabled="true"
                style={{
                  display: "flex",
                  gap: 10,
                  alignItems: "baseline",
                  border: "1px dashed var(--border)",
                  borderRadius: "var(--radius-lg)",
                  padding: 10,
                  color: "var(--text-3)"
                }}
              >
                <strong style={{ fontSize: 13 }}>{u.transport}</strong>
                <span style={{ fontSize: 12 }}>not registered</span>
                <span style={{ fontSize: "var(--text-xs)" }}>
                  This build knows the shape of it and ships no description of one, so no destination can be
                  an instance of it yet. It is listed so the answer is on the screen rather than absent.
                </span>
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      <div style={{ display: "flex", gap: 8 }}>
        <button className="btn" type="button" onClick={onCancel}>
          Cancel
        </button>
        <button className="btn btn--primary" type="button" disabled={!chosenIsConfigurable} onClick={onNext}>
          Next: name this instance
        </button>
      </div>
    </div>
  );
}

/**
 * Step 2: what this instance is called.
 *
 * It asks for a name and nothing else, and it reports the backend rather
 * than re-offering it: a step that let the backend be changed here would
 * be the collapsed single screen this wizard exists not to be.
 *
 * The instances that already exist on this backend are listed because the
 * uniqueness rule applies here. Two local volumes is the ordinary case, so
 * the list is not a warning — it is the context that makes a name choice
 * make sense.
 */
function NameInstancePane({
  backend,
  existing,
  name,
  onName,
  refusal,
  showRefusal,
  onBack,
  onNext
}: {
  backend: BackendManifest;
  existing: StorageMedium[];
  name: string;
  onName(v: string): void;
  refusal: InstanceNameRefusal | null;
  showRefusal: boolean;
  onBack(): void;
  onNext(): void;
}) {
  // Which existing destinations are instances of THIS backend. Matched on
  // the manifest id, and on the pre-EPIC-I `type` spelling as well,
  // because a destination declared before the manifest format existed
  // carries the old word and is still an instance of the same backend.
  const siblings = existing.filter((m) => m.type === backend.id);

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
      <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)", maxWidth: "74ch" }}>
        This will be one <strong>{backend.label}</strong> destination. The name is how a retention tier will
        refer to it, and how every stored copy records where it lives, so it is chosen once and does not
        change afterwards.
      </p>

      <label style={{ display: "flex", flexDirection: "column", gap: 4, maxWidth: "40ch" }}>
        <span style={{ fontSize: "var(--text-xs)", color: "var(--text-2)" }}>Instance name</span>
        <input
          className="input"
          type="text"
          value={name}
          onChange={(e) => onName(e.target.value)}
          placeholder="cold_archive"
        />
      </label>

      {showRefusal && refusal ? (
        <div role="alert" style={{ fontSize: 13, color: "var(--danger-text, var(--text-1))", maxWidth: "74ch" }}>
          {refusal.reason}
        </div>
      ) : null}

      <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
        <span style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
          {siblings.length === 0
            ? `No ${backend.label} destination exists yet, so any name is free.`
            : `Instances of ${backend.label} that already exist:`}
        </span>
        {siblings.length > 0 ? (
          <ul
            role="list"
            aria-label={`Instances of ${backend.label} that already exist`}
            style={{ listStyle: "none", margin: 0, padding: 0, display: "flex", gap: 8, flexWrap: "wrap" }}
          >
            {siblings.map((m) => (
              <li
                key={m.id}
                style={{
                  fontSize: 12,
                  border: "1px solid var(--border)",
                  borderRadius: "var(--radius-md)",
                  padding: "2px 8px"
                }}
              >
                {m.id}
              </li>
            ))}
          </ul>
        ) : null}
      </div>

      <div style={{ display: "flex", gap: 8 }}>
        <button className="btn" type="button" onClick={onBack}>
          Back
        </button>
        <button className="btn btn--primary" type="button" disabled={refusal !== null} onClick={onNext}>
          Next: confirm
        </button>
      </div>
    </div>
  );
}

/**
 * Step 3: what the destination will BE, and that nothing has been written
 * yet.
 *
 * The field list is read off the manifest rather than described in prose,
 * which is the honest version of a confirm step for a destination whose
 * values have not been asked for: "you will be asked for a directory and
 * a subdirectory next" is a promise this screen can keep, and only
 * because it is reading the same manifest the next step will render.
 * Nothing here collects a value and nothing here writes.
 */
function ConfirmPane({
  backend,
  name,
  onBack,
  onConfirm
}: {
  backend: BackendManifest;
  name: string;
  onBack(): void;
  onConfirm(): void;
}) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
      <dl
        style={{
          margin: 0,
          display: "grid",
          gridTemplateColumns: "auto 1fr",
          gap: "4px 12px",
          fontSize: 13
        }}
      >
        <dt style={{ color: "var(--text-2)" }}>Backend</dt>
        <dd style={{ margin: 0 }}>
          {backend.label} <span style={{ color: "var(--text-3)" }}>({backend.id})</span>
        </dd>
        <dt style={{ color: "var(--text-2)" }}>Name</dt>
        <dd style={{ margin: 0 }}>{name}</dd>
        <dt style={{ color: "var(--text-2)" }}>Values</dt>
        <dd style={{ margin: 0, color: "var(--text-3)" }}>not asked for yet</dd>
      </dl>

      <p style={{ margin: 0, fontSize: 13, color: "var(--text-2)", maxWidth: "74ch" }}>
        Nothing has been written yet. Configuring it comes next, and this destination is written once, after a
        connection test against it has passed — so no retention tier can select it until it has been proved.
        That is what stops a tier being pointed at a destination nobody has proved.
      </p>

      <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
        <span style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>
          Configuring it will ask for:
        </span>
        <span style={{ fontSize: 12, color: "var(--text-2)" }}>
          {backend.fields.map((f) => f.label + (f.required ? "" : " (optional)")).join(" · ")}
        </span>
      </div>

      {/* EPIC G's rule is that what an operator can DO names its
          equivalent command. These steps do nothing: they collect two
          answers and hand them on, and there is no `rbm` invocation that
          declares a destination carrying no values (see
          storageDestinationCommands.ts, which is where the `declareCommand`
          that used to be printed here was deleted and why). So the gap is
          named, the way core/cliecho names a route with no verb, rather
          than filled with a line that fails on execution. */}
      <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
        <span style={{ fontSize: "var(--text-xs)", color: "var(--text-3)" }}>Equivalent command</span>
        <span style={{ fontSize: 12, color: "var(--text-2)", maxWidth: "74ch" }}>
          These steps run none. On a terminal a destination is declared in one act, by{" "}
          <code className="mono">rbm medium add</code> carrying the values below — so the command this flow
          is equivalent to is printed by the configure step, which is the step that writes.
        </span>
      </div>

      <div style={{ display: "flex", gap: 8 }}>
        <button className="btn" type="button" onClick={onBack}>
          Back
        </button>
        <button className="btn btn--primary" type="button" onClick={onConfirm}>
          Next: configure it
        </button>
      </div>
    </div>
  );
}
