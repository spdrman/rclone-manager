/**
 * Issue #176 wrote this screen; issue #275 is why it is all that remains of
 * FirstRunPage.
 *
 * First run used to be its own surface, rendered INSTEAD of the routed
 * application, which is what made an unconfigured instance a dead end: the
 * wizard's own "Cancel and return to backup sets" changed the URL to /sets
 * and nothing happened, because which surface rendered was gated on
 * whether a configuration existed rather than on the route. The wizard is
 * now reached at /sets/new in both modes, from a backup-sets list that
 * renders empty, so there is no separate first-run surface left to drift.
 *
 * This one state is the exception, and deliberately so. The configuration
 * was written and is safe, but this process could not start serving it, so
 * every route really would refuse and offering navigation would be
 * offering something that cannot work.
 */
export function ConfigurationSavedPage() {
  return (
    <div style={{ maxWidth: 720, margin: "0 auto", padding: "48px 20px" }}>
      <h1 style={{ marginTop: 0 }}>Configuration saved</h1>
      <p style={{ color: "var(--text-2)" }}>
        Your configuration has been written and is safe. This instance could not start
        serving it without a restart, so restart the Backup Manager container or service
        and open this page again. You do not need to enter any of it a second time.
      </p>
    </div>
  );
}
