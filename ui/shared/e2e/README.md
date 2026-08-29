# End-to-end suite

Runs against the mock API (`npm run dev`), which is deterministic and covers
every documented product state — no fixtures to seed, no backend required.

```bash
npx playwright install          # once
npm run e2e                     # generic project + small-window project
npm run e2e:ui                  # interactive runner
npm run e2e:providers           # provider treatment, current VITE_PLATFORM
npm run e2e:all-providers       # provider treatment across all seven
```

## Files

| Spec | Guards |
| --- | --- |
| `shell.spec.ts` | lockup, section nav, active-link state, counts, theme persistence |
| `dashboard.spec.ts` | backup-vs-service health separation, host-key escalation, metrics, transfer stages, empty + storage-critical scenarios |
| `backup-sets.spec.ts` | card content, non-colour-only state, halted set cannot run, detail sections, destructive-action placement |
| `wizard.spec.ts` | six steps, per-step fields, trust decision, inferred-completion warning, acknowledgement gate (including re-arming on uncheck) |
| `backups.spec.ts` | table semantics, all seven columns, multi-classification, filtering, artifact detail, lifecycle ordering |
| `retention.spec.ts` | server-issued plan id, keep/delete itemisation, stale-plan refusal, consequence-named confirm, safe default focus, Escape |
| `quarantine-activity.spec.ts` | three safe actions only, no "delete anyway", event vocabulary, filters |
| `settings-recovery.spec.ts` | service settings, capability honesty, build provenance, version-mismatch read-only mode, non-destructive catalog rebuild |
| `auth.spec.ts` | login is not a NAS system login, enrolment validation, password never echoed |
| `provider-treatment.spec.ts` | per-provider identity, shared IA, accent token, chrome/picker/notifications only where supported |
| `responsive.spec.ts` | small app window: no clipped table column, no page-level horizontal scroll, dialogs fully visible |
| `accessibility.spec.ts` | one h1 per page, every control labelled, no colour-only status, focus visibility, dialog semantics, zero console errors |
| `safety-invariants.spec.ts` | no remote/file delete anywhere, no private key, no "restore point", no stack traces, no vague destructive labels |

## Conventions

- Query by role and accessible name. A spec that needs a CSS selector is a hint
  that the component is missing a label.
- `fixtures.ts` holds the page object and the `?scenario=` helper. Add selectors
  there, not inline in specs.
- Provider specs assert *capability honesty* — that the UI never presents an
  unsupported capability as available — not visual difference.
