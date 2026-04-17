# Changelog

All notable changes to GoClaw are documented here. For full documentation, see [docs.goclaw.sh](https://docs.goclaw.sh).

## Unreleased

### Breaking Changes

- **Pancake channel: `first_inbox` → `private_reply` rename.** Matches the
  Pancake API + Facebook Graph policy term. Channel config keys + feature flag
  renamed:
  - `features.first_inbox` → `features.private_reply`
  - `first_inbox_message` → `private_reply_message` (supports `{{commenter_name}}`
    and `{{post_title}}` template vars)

  Migration SQL for existing deployments (dev-only deployments can skip — the
  feature was five days old with minimal adoption):
  ```sql
  UPDATE channel_instances
  SET config = jsonb_set(
    jsonb_set(
      config - 'features' - 'first_inbox_message' || jsonb_build_object('first_inbox_message', null),
      '{features,private_reply}', (config->'features'->'first_inbox'), true
    ),
    '{private_reply_message}', (config->'first_inbox_message'), true
  )
  WHERE channel_type = 'pancake'
    AND config->'features' ? 'first_inbox';
  ```

- **Context pruning now opt-in.** Previously tool-result trimming ran by default
  for all providers; now requires explicit `contextPruning.mode: "cache-ttl"` in
  `config.agents.defaults` to enable. Matches upstream TS design and prevents
  silent prompt-cache invalidation on Anthropic.

  Migration — add to `config.json5`:
  ```json5
  agents: {
    defaults: {
      contextPruning: { mode: "cache-ttl" }
    }
  }
  ```

### New Features

- **Pancake private-reply funnel.** Expands the renamed `private_reply` feature
  into a full comment → DM funnel:
  - **Two modes** via `private_reply_mode`: `after_reply` (default — public
    reply then DM) and `standalone` (DM-only, bypasses keyword filter, skips
    the LLM pipeline via a synthetic outbound fast-path).
  - **Post-level scope filter** via `private_reply_options.allow_post_ids` /
    `deny_post_ids` (deny beats allow; nil = send to all posts).
  - **Configurable TTL** via `private_reply_ttl_days` (default 7) — how long
    before the same commenter can receive the auto-DM again.
  - **Template variables** `{{commenter_name}}` and `{{post_title}}` with
    literal-replace semantics (pre-sanitizes `{{`/`}}` from var values to
    prevent var-in-var substitution).
  - **DB-backed dedup** (`pancake_private_reply_sent`) replaces the in-memory
    `sync.Map` — survives channel restarts. Atomic TryClaim/Unclaim eliminates
    the concurrent-comment TOCTOU that would have fired duplicate DMs.
  - **Locale-aware default text** when operator leaves `private_reply_message`
    blank — resolves via `i18n.T(locale, MsgPancakePrivateReplyDefault)` with
    en/vi/zh catalogs and English fallback.
  - PG migration 000056; SQLite schema v25 (RequiredSchemaVersion 55→56).
- **Prometheus `/metrics` endpoint.** New `internal/metrics` package exposes
  `pancake_private_reply_total{page_id,result,reason}` counters (result ∈
  `sent|skipped|failed`; reason covers `feature_off`, `store_unwired`,
  `scope_filter`, `dedup_hit`, `dedup_error`, `api_error`). Endpoint is gated
  behind Bearer auth when `gateway.token` is set; open for local dev scraping
  when unset (warns in logs). Bind `GOCLAW_HOST=127.0.0.1` or set a token to
  avoid leaking page identifiers on public networks.

### Improvements

- **Context pruning cleanup.** Removed redundant Pass 0 (per-result 30% guard),
  deduplicated double prune call per iteration, added SanitizeHistory to
  PruneStage for broken tool_use/tool_result pair cleanup.
- **Context pruning config backfill (migration).** Agents with existing custom
  `context_pruning` config (e.g., `softTrimRatio`, `keepLastAssistants`) but
  missing a `mode` field get auto-backfilled with `mode: "cache-ttl"` to
  preserve their intent after the opt-in flip. Rows with NULL config stay
  NULL (new opt-in default applies). PG migration 51; SQLite schema v19.
- **Pancake channel metadata routing.** Whitelist in
  `internal/channels/routing_metadata.go` now preserves `private_reply_mode`,
  `private_reply_only`, `post_id`, `display_name` across the inbound →
  outbound hop so mode switch + scope filter + template vars survive the
  agent pipeline round-trip. Without this, these keys were silently stripped.
- **Channel `Send` tenant propagation.** Pancake `Channel.Send` now injects
  `store.WithTenantID(ctx, ch.TenantID())` before dispatching so store calls
  scoped by tenant (notably the new private-reply dedup store) see a non-nil
  tenant under the dispatcher path. Matches the pattern used by Telegram,
  Zalo, WhatsApp.

## Project Status

### Implemented & Tested in Production

- **Agent management & configuration** — Create, update, delete agents via API and web dashboard. Agent types (`open` / `predefined`), agent routing, and lazy resolution all tested.
- **Telegram channel** — Full integration tested: message handling, streaming responses, rich formatting (HTML, tables, code blocks), reactions, media, chunked long messages.
- **Seed data & bootstrapping** — Auto-onboard, DB seeding, migration pipeline tested end-to-end.
- **User-scope & content files** — Per-user context files (`user_context_files`), agent-level context files (`agent_context_files`), virtual FS interceptors, per-user seeding (`SeedUserFiles`), and user-agent profile tracking all implemented and tested.
- **Core built-in tools** — File system tools (`read_file`, `write_file`, `edit_file`, `list_files`, `search`, `glob`), shell execution (`exec`), web tools (`web_search`, `web_fetch`), and session management tools tested in real agent loops.
- **Memory system** — Long-term memory with pgvector hybrid search (FTS + vector) implemented and tested with real conversations.
- **Agent loop** — Think-act-observe cycle, tool use, session history, auto-summarization, and subagent spawning tested in production.
- **WebSocket RPC protocol (v3)** — Connect handshake, chat streaming, event push all tested with web dashboard and integration tests.
- **Store layer (PostgreSQL)** — All PG stores (sessions, agents, providers, skills, cron, pairing, tracing, memory, teams) implemented and running.
- **Browser automation** — Rod/CDP integration for headless Chrome, tested in production agent workflows.
- **Lane-based scheduler** — Main/subagent/team/cron lane isolation with concurrent execution tested. Group chats support up to 3 concurrent agent runs per session with adaptive throttle and deferred session writes for history isolation.
- **Security hardening** — Rate limiting, prompt injection detection, CORS, shell deny patterns, SSRF protection, credential scrubbing all implemented and verified.
- **Web dashboard** — Channel management, agent management, pairing approval, traces & spans viewer, skills, MCP, cron, sessions, teams, and config pages all implemented and working.
- **Prompt caching** — Anthropic (explicit `cache_control`), OpenAI/MiniMax/OpenRouter (automatic). Cache metrics tracked in trace spans and displayed in web dashboard.
- **Agent delegation** — Inter-agent task delegation with permission links, sync/async modes, per-user restrictions, concurrency limits, and hybrid agent search. Tested in production.
- **Agent teams** — Team creation with lead/member roles, shared task board (create, claim, complete, search, blocked_by dependencies), team mailbox (send, broadcast, read). Tested in production.
- **Evaluate loop** — Generator-evaluator feedback cycles with configurable max rounds and pass criteria. Tested in production.
- **Delegation history** — Queryable audit trail of inter-agent delegations. Tested in production.
- **Skill system** — BM25 search, ZIP upload, SKILL.md parsing, and embedding hybrid search. Tested in production.
- **MCP integration** — stdio, SSE, and streamable-http transports with per-agent/per-user grants. Tested in production.
- **Cron scheduling** — `at`, `every`, and cron expression scheduling. Tested in production.
- **Docker sandbox** — Isolated code execution in containers. Tested in production.
- **Text-to-Speech** — OpenAI, ElevenLabs, Edge, MiniMax providers. Tested in production.
- **HTTP API** — `/v1/chat/completions`, `/v1/agents`, `/v1/skills`, etc. Tested in production. Interactive Swagger UI at `/docs`.
- **API key management** — Multi-key auth with RBAC scopes, SHA-256 hashed storage, show-once pattern, optional expiry, revocation. HTTP + WebSocket CRUD. Web UI for management.
- **Hooks system** — Event-driven hooks with command evaluators (shell exit code) and agent evaluators (delegate to reviewer). Blocking gates with auto-retry and recursion-safe evaluation.
- **Media tools** — `create_image` (DashScope, MiniMax), `create_audio` (OpenAI, ElevenLabs, MiniMax, Suno), `create_video` (MiniMax, Veo), `read_document` (Gemini File API), `read_image`, `read_audio`, `read_video`. Persistent media storage with lazy-loaded MediaRef.
- **Additional provider modes** — Claude CLI (Anthropic via stdio + MCP bridge), Codex (OpenAI gpt-5.3-codex via OAuth).
- **Knowledge graph** — LLM-powered entity extraction, graph traversal, force-directed visualization, and `knowledge_graph_search` agent tool.
- **Memory management** — Admin dashboard for memory documents (CRUD, semantic search, chunk/embedding details, bulk re-indexing).
- **Persistent pending messages** — Channel messages persisted to PostgreSQL with auto-compaction (LLM summarization) and monitoring dashboard.
- **Heartbeat system** — Periodic agent check-ins via HEARTBEAT.md checklists with suppress-on-OK, active hours, retry logic, and channel delivery.

### Implemented but Not Fully Tested

- **Slack** — Channel integration implemented, not yet validated with real users.
- **Other messaging channels** — Discord, Zalo OA, Zalo Personal, Feishu/Lark, WhatsApp channel adapters are implemented but have not been tested end-to-end in production. Only Telegram has been validated with real users.
- **OpenTelemetry export** — OTLP gRPC/HTTP exporter implemented (build-tag gated). In-app tracing works; external OTel export not validated in production.
- **Tailscale integration** — tsnet listener implemented (build-tag gated). Not tested in a real deployment.
- **Redis cache** — Optional distributed cache backend (build-tag gated). Not tested in production.
- **Browser pairing** — Pairing code flow implemented with CLI and web UI approval. Basic flow tested but not validated at scale.
