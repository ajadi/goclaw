# Pancake auto-react allow/deny scope filter — TDD ship session

Date: 2026-04-17
Plan: `plans/260417-1423-pancake-auto-react-allow-deny/`
Branch: `dev`

## What shipped

5 phases, TDD flow, ~50 LOC backend + ~25 LOC UI, +20 tests.

| Phase | Change |
|-------|--------|
| 0 | UI surfacing `features.auto_react` toggle (UX gap — previously required raw JSON edit) |
| 1 | `AutoReactOptions` pointer struct in `types.go` |
| 2 | `filterAutoReact` + `containsString` helpers, callsite gate + rollout-phase Info log |
| 3 | 4 UI tags fields (allow/deny × post/user) gated on `features.auto_react=true` |
| 4 | Integration verify — PG build, sqliteonly build, vet, race, 14 channel packages green |

## Key deviation from plan

**Plan said:** anonymous inline struct value type with `omitempty` on parent key.
**Reality:** Go's `encoding/json` `omitempty` tag has **no effect on non-pointer struct fields**. Plan's `TestPancakeConfig_AutoReactOptionsOmitempty` caught this — first run produced `"auto_react_options":{}` in output.
**Fix:** Promoted inline struct to named type `AutoReactOptions` + changed field to pointer `*AutoReactOptions`. `omitempty` works with pointers. Phase 2 helper handles nil pointer as "no filter, allow all". Matrix test added `nil opts → allow` case.

This is a common Go JSON gotcha. Plan authors had the right intent (serialize clean) but wrong tag mechanics. Corrective path was trivial but only discovered because the test was explicit about the expected JSON shape.

## Red-team findings applied

- Whitespace trim in `containsString` (copy-paste IDs with stray spaces would silently miss).
- Empty-target short-circuit (prevents blank `sender_id` from matching an empty-string list entry).
- Deny evaluated before allow (short-circuit on deny match — plan's conflict rule).
- `showWhen: { key: "features.auto_react", value: "true" }` verified against `channel-fields.tsx` comparator (uses `String(depValue) === value`, so `true` → `"true"` works).

## What I did NOT do

- No i18n entries for the new UI fields — relied on inline `label`/`help` fallback per plan's red-team 2.2 (i18next treats `.` in keys as namespace separator; nested JSON refactor is outside scope).
- No desktop schema changes — desktop has no pancake block (Lite skips Pancake).
- No migrations — JSONB accepts new keys natively.
- No benchmark/load tests — lists expected <100 entries; premature optimization.

## Follow-ups

- Downgrade rollout Info log to Debug after ~2 weeks post-ship (TODO comment in code).
- Live webhook smoke test when a real FB page is available (unit matrix covers logic).
- Consider collapsible "Auto-react scope" section in UI if fields grow (defer — YAGNI).

## Verification numbers

- 3/3 new config roundtrip/omitempty/defaults tests pass
- 13/13 filterAutoReact matrix cases pass (plan had 12; added nil-opts case)
- 10/10 UI schema tests pass (6 existing + 4 new)
- 0 regressions across all 14 `internal/channels/*` packages with race detector
- UI build blocked by pre-existing `prismjs` error in `script-editor.tsx` (unrelated; flagged to user)
