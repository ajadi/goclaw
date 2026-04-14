# Trace Stop/Abort: Cascading Failure Across Four Layers

**Date:** 2026-04-14 17:02
**Severity:** High
**Component:** Agent tracing, HTTP client, WebSocket, UI real-time refresh
**Status:** Designed, ready for implementation (Phase 1-7 planned)

## What Happened

User reported the trace page Stop button broken: UI stuck in "running" state, toast "abortFailed", most often during long agent tasks where the LLM stream stalls or a tool hangs. Spent morning brainstorming root causes, spent afternoon designing a comprehensive fix. Approved plan now in `plans/260414-1702-trace-stop-abort-redesign/`.

## The Brutal Truth

This isn't a single bug. It's a cascade of small architectural choices across four independent layers that compound into a reliability failure. Each layer alone seems innocuous—a hardcoded timeout here, a check-after-block there, a silent error swallow there. But they stack: socket read blocks ctx cancellation, router returns before goroutine actually exits, trace status silently fails to persist, UI doesn't know anything happened. User clicks Stop, sees confirmation toast, but trace is actually still running and burning tokens. It damages trust.

The worst part? The bug surfaces most when users need it most—long-running tasks where patience is thin. By the time they hit Stop, they're already frustrated.

## Technical Details

**Root Causes (7 specific failure points):**

1. **`providers/defaults.go:9` + `anthropic.go:63`** — `http.Client{Timeout: 300s}` is socket-level, never sees context cancellation. Stream can block for full 5 minutes even after ctx.Cancel().

2. **`providers/anthropic_stream.go:43-44`** — SSE scanner checks `ctx.Err()` **after** `sse.Next()` blocks on socket read. Read is OS-level; ctx cancellation doesn't unblock it. Goroutine becomes zombie.

3. **`tracing/collector.go:171-176`** — `SetTraceStatus()` silently swallows database errors (transaction conflict, constraint violation). No retry. Trace stays "running" forever.

4. **`agent/router.go:276-292`** — `AbortRun()` returns true immediately after calling `Cancel()`, then deletes entry. Doesn't wait for goroutine to actually exit. Caller (UI) thinks it's done; loop still running. Second click: "not found" error.

5. **`ui/web/src/pages/traces/hooks/use-traces.ts:38`** — `staleTime: 60_000` with no WebSocket event. Trace query won't refresh for a full minute. Even if backend fixed trace status, UI doesn't see it.

6. **`tracing/collector.go:231`** — Stale recovery threshold: 30 minutes. Too slow to catch. If all 4 above fail, trace lives in broken state for half hour.

7. **`agent/router.go:AbortRun` return value** — Returns `false` for both "not found" AND "sessionKey mismatch". UI error toast can't distinguish; user has no idea which problem.

## What We Tried

**Approach A (Chosen): Abort Contract Redesign**
- Layer 1: HTTP `Transport` with `ResponseHeaderTimeout: 60s` + `IdleConnTimeout: 90s`, drop `Client.Timeout`. Goroutine-based ctx-aware body close with `sync.Once`.
- Layer 2: Router 2-phase abort—atomic state CAS (running→aborting), `Done` chan, 3s wait for goroutine with force-mark fallback.
- Layer 3: Detached trace status update (`context.WithoutCancel`) with 5s timeout, 3-retry backoff, bounded in-memory queue.
- Layer 4: New WS event `trace.status` for real-time UI refresh, drop 60s staleTime, shrink stale recovery from 30min→2min with 30s interval.
- Tool audit: process group kill (shell), rod Page close (browser), 5s fallback (MCP).

**Why not B (targeted race fixes)?** Would fix 4-5 immediate pain points but leave blocking SSE issue unresolved. Could recur under different load patterns.

**Why not C (UX band-aid)?** Stop button already shows optimistic UI; problem is backend didn't actually stop. Reload button doesn't fix it if trace still running. Doesn't address token waste.

## Root Cause Analysis

Three things conspired:

1. **Timeout design gap** — Hardcoded socket timeout predates context-aware cancellation patterns. Original code assumed "always wait 5 minutes for response." Didn't account for mid-stream abort.

2. **Check-after-block pattern** — Reading SSE from blocked socket, then checking context, is fundamentally racy. Need to interleave: ctx watch on one goroutine, socket read on another, close body to unblock.

3. **Silent error swallow** — `SetTraceStatus` failure doesn't fail the operation. Router's `AbortRun` doesn't sync on goroutine exit. UI doesn't know to keep polling. When a layer silently fails, upper layers can't compensate.

The cascade happens because each layer trusts the one below it. Layer 2 (router) says "abort done", Layer 3 (tracing) is broken but silent, Layer 4 (UI) stops polling. Result: false completion.

## Lessons Learned

1. **Timeouts are tricky.** Socket-level timeouts don't compose well with context cancellation. Always prefer transport-level configuration + ctx awareness over Client timeout.

2. **Never return before goroutine exits.** If caller expects "operation done," it must be **actually done**. Use `sync.WaitGroup`, `Done` chans, or atomic state flags. Don't optimistically return.

3. **Errors must propagate.** Silent failure in middleware is silent failure in outer layers. Either fail loud (return error) or retry with bounds. In-memory queue + stale recovery worker is acceptable if you log failures and have a fallback.

4. **Real-time UI requires real-time signaling.** Polling with long staleTime is fragile for user-initiated actions. WebSocket events aren't overhead; they're correctness.

5. **Test the layered failure modes.** Single-layer tests won't catch this. Need integration tests with: stream stall, process hang, DB transaction conflict, rapid clicks, network blip. Race detector (`-race`) is mandatory.

## Next Steps

- **Phase 1** (Layer 1 HTTP ctx-aware): 6-8 hours. Tests for socket close-on-cancel.
- **Phase 2** (Layer 2 router 2-phase abort): 4-6 hours. Must test race detector.
- **Phase 3** (Layer 3 trace status persist): 3-4 hours. Bounded retry queue + backoff.
- **Phase 4** (Layer 4 WS event + UI): 5-6 hours. React Query invalidation + toast UX.
- **Phase 5** (Tool execution audit): Parallel, 4-5 hours. Process group kill, rod Page close.
- **Phase 6** (i18n + refinement): 3-4 hours. Native speaker review for vi/zh error messages.
- **Phase 7** (Integration tests): 6-8 hours. Race detector + stuck scenarios + multi-click. **Gates merge.**

Critical path: 1→2→3→4→6→7 (roughly 30-36 hours). Phase 5 independent.

Open questions moved to plan phases: runID==traceID check, stale threshold tuning on legitimate long runs, WS event subscription allow-list, i18n review, CI PostgreSQL availability.

---

**Related:**
- Brainstorm report: `plans/reports/brainstorm-260414-trace-stop-abort-redesign.md`
- Implementation plan: `plans/260414-1702-trace-stop-abort-redesign/plan.md` (phases 1-7)
- Git branch: `dev` (ready for task delegation)
